package instances

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/network"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Manager interface {
	ListInstances(ctx context.Context) ([]Instance, error)
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error)
	GetInstance(ctx context.Context, id string) (*Instance, error)
	GetInstanceByName(ctx context.Context, name string) (*Instance, error)
	DeleteInstance(ctx context.Context, id string) error
	StandbyInstance(ctx context.Context, id string) (*Instance, error)
	RestoreInstance(ctx context.Context, id string) (*Instance, error)
	StreamInstanceLogs(ctx context.Context, id string, tail int, follow bool) (<-chan string, error)
	RotateLogs(ctx context.Context, maxBytes int64, maxFiles int) error
	AttachVolume(ctx context.Context, id string, volumeId string, req AttachVolumeRequest) (*Instance, error)
	DetachVolume(ctx context.Context, id string, volumeId string) (*Instance, error)
}

// ResourceLimits contains configurable resource limits for instances
type ResourceLimits struct {
	MaxOverlaySize       int64 // Maximum overlay disk size in bytes per instance
	MaxVcpusPerInstance  int   // Maximum vCPUs per instance (0 = unlimited)
	MaxMemoryPerInstance int64 // Maximum memory in bytes per instance (0 = unlimited)
	MaxTotalVcpus        int   // Maximum total vCPUs across all instances (0 = unlimited)
	MaxTotalMemory       int64 // Maximum total memory in bytes across all instances (0 = unlimited)
}

// Metrics holds the metrics instruments for instance operations.
type Metrics struct {
	createDuration   metric.Float64Histogram
	restoreDuration  metric.Float64Histogram
	standbyDuration  metric.Float64Histogram
	stateTransitions metric.Int64Counter
	tracer           trace.Tracer
}

type manager struct {
	paths          *paths.Paths
	imageManager   images.Manager
	systemManager  system.Manager
	networkManager network.Manager
	volumeManager  volumes.Manager
	limits         ResourceLimits
	instanceLocks  sync.Map      // map[string]*sync.RWMutex - per-instance locks
	hostTopology   *HostTopology // Cached host CPU topology
	metrics        *Metrics
}

// NewManager creates a new instances manager.
// If meter is nil, metrics are disabled.
func NewManager(p *paths.Paths, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, volumeManager volumes.Manager, limits ResourceLimits, meter metric.Meter, tracer trace.Tracer) Manager {
	m := &manager{
		paths:          p,
		imageManager:   imageManager,
		systemManager:  systemManager,
		networkManager: networkManager,
		volumeManager:  volumeManager,
		limits:         limits,
		instanceLocks:  sync.Map{},
		hostTopology:   detectHostTopology(), // Detect and cache host topology
	}

	// Initialize metrics if meter is provided
	if meter != nil {
		metrics, err := newInstanceMetrics(meter, tracer, m)
		if err == nil {
			m.metrics = metrics
		}
	}

	return m
}

// newInstanceMetrics creates and registers all instance metrics.
func newInstanceMetrics(meter metric.Meter, tracer trace.Tracer, m *manager) (*Metrics, error) {
	createDuration, err := meter.Float64Histogram(
		"hypeman_instances_create_duration_seconds",
		metric.WithDescription("Time to create an instance"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	restoreDuration, err := meter.Float64Histogram(
		"hypeman_instances_restore_duration_seconds",
		metric.WithDescription("Time to restore an instance from standby"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	standbyDuration, err := meter.Float64Histogram(
		"hypeman_instances_standby_duration_seconds",
		metric.WithDescription("Time to put an instance in standby"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	stateTransitions, err := meter.Int64Counter(
		"hypeman_instances_state_transitions_total",
		metric.WithDescription("Total number of instance state transitions"),
	)
	if err != nil {
		return nil, err
	}

	// Register observable gauge for instance counts by state
	instancesTotal, err := meter.Int64ObservableGauge(
		"hypeman_instances_total",
		metric.WithDescription("Total number of instances by state"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			instances, err := m.listInstances(ctx)
			if err != nil {
				return nil
			}
			stateCounts := make(map[string]int64)
			for _, inst := range instances {
				stateCounts[string(inst.State)]++
			}
			for state, count := range stateCounts {
				o.ObserveInt64(instancesTotal, count,
					metric.WithAttributes(attribute.String("state", state)))
			}
			return nil
		},
		instancesTotal,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		createDuration:   createDuration,
		restoreDuration:  restoreDuration,
		standbyDuration:  standbyDuration,
		stateTransitions: stateTransitions,
		tracer:           tracer,
	}, nil
}

// recordDuration records operation duration.
func (m *manager) recordDuration(ctx context.Context, histogram metric.Float64Histogram, start time.Time, status string) {
	if m.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	histogram.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))
}

// recordStateTransition records a state transition.
func (m *manager) recordStateTransition(ctx context.Context, fromState, toState string) {
	if m.metrics == nil {
		return
	}
	m.metrics.stateTransitions.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("from", fromState),
			attribute.String("to", toState),
		))
}

// getInstanceLock returns or creates a lock for a specific instance
func (m *manager) getInstanceLock(id string) *sync.RWMutex {
	lock, _ := m.instanceLocks.LoadOrStore(id, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

// CreateInstance creates and starts a new instance
func (m *manager) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error) {
	// Note: ID is generated inside createInstance, so we can't lock before calling it.
	// This is safe because:
	// 1. ULID generation is unique
	// 2. Filesystem mkdir is atomic per instance directory
	// 3. Concurrent creates of different instances don't conflict
	return m.createInstance(ctx, req)
}

// DeleteInstance stops and deletes an instance
func (m *manager) DeleteInstance(ctx context.Context, id string) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	err := m.deleteInstance(ctx, id)
	if err == nil {
		// Clean up the lock after successful deletion
		m.instanceLocks.Delete(id)
	}
	return err
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
func (m *manager) StandbyInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.standbyInstance(ctx, id)
}

// RestoreInstance restores an instance from standby
func (m *manager) RestoreInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.restoreInstance(ctx, id)
}

// ListInstances returns all instances
func (m *manager) ListInstances(ctx context.Context) ([]Instance, error) {
	// No lock - eventual consistency is acceptable for list operations.
	// State is derived dynamically, so list is always reasonably current.
	return m.listInstances(ctx)
}

// GetInstance returns a single instance
func (m *manager) GetInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.RLock()
	defer lock.RUnlock()
	return m.getInstance(ctx, id)
}

// GetInstanceByName returns an instance by name
// Returns ErrNotFound if no instance matches, ErrAmbiguousName if multiple match
func (m *manager) GetInstanceByName(ctx context.Context, name string) (*Instance, error) {
	instances, err := m.ListInstances(ctx)
	if err != nil {
		return nil, err
	}

	var matches []Instance
	for _, inst := range instances {
		if inst.Name == name {
			matches = append(matches, inst)
		}
	}

	if len(matches) == 0 {
		return nil, ErrNotFound
	}
	if len(matches) > 1 {
		return nil, ErrAmbiguousName
	}

	return &matches[0], nil
}

// StreamInstanceLogs streams instance console logs
// Returns last N lines, then continues following if follow=true
func (m *manager) StreamInstanceLogs(ctx context.Context, id string, tail int, follow bool) (<-chan string, error) {
	// Note: No lock held during streaming - we read from the file continuously
	// and the file is append-only, so this is safe
	return m.streamInstanceLogs(ctx, id, tail, follow)
}

// RotateLogs rotates console logs for all instances that exceed maxBytes
func (m *manager) RotateLogs(ctx context.Context, maxBytes int64, maxFiles int) error {
	instances, err := m.listInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances for rotation: %w", err)
	}

	var lastErr error
	for _, inst := range instances {
		logPath := m.paths.InstanceConsoleLog(inst.Id)
		if err := rotateLogIfNeeded(logPath, maxBytes, maxFiles); err != nil {
			lastErr = err // Continue with other instances, but track error
		}
	}
	return lastErr
}

// AttachVolume attaches a volume to an instance (not yet implemented)
func (m *manager) AttachVolume(ctx context.Context, id string, volumeId string, req AttachVolumeRequest) (*Instance, error) {
	return nil, fmt.Errorf("attach volume not yet implemented")
}

// DetachVolume detaches a volume from an instance (not yet implemented)
func (m *manager) DetachVolume(ctx context.Context, id string, volumeId string) (*Instance, error) {
	return nil, fmt.Errorf("detach volume not yet implemented")
}
