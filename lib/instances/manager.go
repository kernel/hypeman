package instances

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Manager interface {
	ListInstances(ctx context.Context, filter *ListInstancesFilter) ([]Instance, error)
	ListSnapshots(ctx context.Context, filter *ListSnapshotsFilter) ([]Snapshot, error)
	GetSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error)
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error)
	CreateSnapshot(ctx context.Context, id string, req CreateSnapshotRequest) (*Snapshot, error)
	// GetInstance returns an instance by ID, name, or ID prefix.
	// Lookup order: exact ID match -> exact name match -> ID prefix match.
	// Returns ErrAmbiguousName if prefix matches multiple instances.
	GetInstance(ctx context.Context, idOrName string) (*Instance, error)
	DeleteInstance(ctx context.Context, id string) error
	DeleteSnapshot(ctx context.Context, snapshotID string) error
	ForkInstance(ctx context.Context, id string, req ForkInstanceRequest) (*Instance, error)
	ForkSnapshot(ctx context.Context, snapshotID string, req ForkSnapshotRequest) (*Instance, error)
	StandbyInstance(ctx context.Context, id string) (*Instance, error)
	RestoreInstance(ctx context.Context, id string) (*Instance, error)
	RestoreSnapshot(ctx context.Context, id string, snapshotID string, req RestoreSnapshotRequest) (*Instance, error)
	StopInstance(ctx context.Context, id string) (*Instance, error)
	StartInstance(ctx context.Context, id string, req StartInstanceRequest) (*Instance, error)
	StreamInstanceLogs(ctx context.Context, id string, tail int, follow bool, source LogSource) (<-chan string, error)
	RotateLogs(ctx context.Context, maxBytes int64, maxFiles int) error
	AttachVolume(ctx context.Context, id string, volumeId string, req AttachVolumeRequest) (*Instance, error)
	DetachVolume(ctx context.Context, id string, volumeId string) (*Instance, error)
	// ListInstanceAllocations returns resource allocations for all instances.
	// Used by the resource manager for capacity tracking.
	ListInstanceAllocations(ctx context.Context) ([]resources.InstanceAllocation, error)
	// ListRunningInstancesInfo returns info needed for utilization metrics collection.
	// Used by the resource manager for VM utilization tracking.
	ListRunningInstancesInfo(ctx context.Context) ([]resources.InstanceUtilizationInfo, error)
	// SetResourceValidator sets the validator for aggregate resource limit checking.
	// Called after initialization to avoid circular dependencies.
	SetResourceValidator(v ResourceValidator)
	// GetVsockDialer returns a VsockDialer for the specified instance.
	GetVsockDialer(ctx context.Context, instanceID string) (hypervisor.VsockDialer, error)
}

// ResourceLimits contains configurable resource limits for instances
type ResourceLimits struct {
	MaxOverlaySize       int64 // Maximum overlay disk size in bytes per instance
	MaxVcpusPerInstance  int   // Maximum vCPUs per instance (0 = unlimited)
	MaxMemoryPerInstance int64 // Maximum memory in bytes per instance (0 = unlimited)
}

// ResourceValidator validates if resources can be allocated
type ResourceValidator interface {
	// ValidateAllocation checks if the requested resources are available.
	// Returns nil if allocation is allowed, or a detailed error describing
	// which resource is insufficient and the current capacity/usage.
	ValidateAllocation(ctx context.Context, vcpus int, memoryBytes int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, needsGPU bool) error
}

type manager struct {
	paths             *paths.Paths
	imageManager      images.Manager
	systemManager     system.Manager
	networkManager    network.Manager
	deviceManager     devices.Manager
	volumeManager     volumes.Manager
	limits            ResourceLimits
	resourceValidator ResourceValidator // Optional validator for aggregate resource limits
	instanceLocks     sync.Map          // map[string]*sync.RWMutex - per-instance locks
	bootMarkerScans   sync.Map          // map[string]time.Time next allowed boot-marker rescan
	hostTopology      *HostTopology     // Cached host CPU topology
	metrics           *Metrics
	now               func() time.Time

	// Hypervisor support
	vmStarters        map[hypervisor.Type]hypervisor.VMStarter
	defaultHypervisor hypervisor.Type // Default hypervisor type when not specified in request
	guestMemoryPolicy guestmemory.Policy
}

// platformStarters is populated by platform-specific init functions.
var platformStarters = make(map[hypervisor.Type]hypervisor.VMStarter)

// NewManager creates a new instances manager.
// If meter is nil, metrics are disabled.
// defaultHypervisor specifies which hypervisor to use when not specified in requests.
func NewManager(p *paths.Paths, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, deviceManager devices.Manager, volumeManager volumes.Manager, limits ResourceLimits, defaultHypervisor hypervisor.Type, meter metric.Meter, tracer trace.Tracer, memoryPolicy ...guestmemory.Policy) Manager {
	// Validate and default the hypervisor type
	if defaultHypervisor == "" {
		defaultHypervisor = hypervisor.TypeCloudHypervisor
	}

	policy := guestmemory.DefaultPolicy()
	if len(memoryPolicy) > 0 {
		policy = memoryPolicy[0]
	}
	policy = policy.Normalize()

	// Initialize VM starters from platform-specific init functions
	vmStarters := make(map[hypervisor.Type]hypervisor.VMStarter, len(platformStarters))
	for hvType, starter := range platformStarters {
		vmStarters[hvType] = starter
	}

	m := &manager{
		paths:             p,
		imageManager:      imageManager,
		systemManager:     systemManager,
		networkManager:    networkManager,
		deviceManager:     deviceManager,
		volumeManager:     volumeManager,
		limits:            limits,
		instanceLocks:     sync.Map{},
		bootMarkerScans:   sync.Map{},
		hostTopology:      detectHostTopology(), // Detect and cache host topology
		vmStarters:        vmStarters,
		defaultHypervisor: defaultHypervisor,
		now:               time.Now,
		guestMemoryPolicy: policy,
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

// SetResourceValidator sets the resource validator for aggregate limit checking.
// This is called after initialization to avoid circular dependencies.
func (m *manager) SetResourceValidator(v ResourceValidator) {
	m.resourceValidator = v
}

// getHypervisor creates a hypervisor client for the given socket and type.
// Used for connecting to already-running VMs (e.g., for state queries).
func (m *manager) getHypervisor(socketPath string, hvType hypervisor.Type) (hypervisor.Hypervisor, error) {
	return hypervisor.NewClient(hvType, socketPath)
}

// getVMStarter returns the VM starter for the given hypervisor type.
func (m *manager) getVMStarter(hvType hypervisor.Type) (hypervisor.VMStarter, error) {
	starter, ok := m.vmStarters[hvType]
	if !ok {
		return nil, fmt.Errorf("no VM starter for hypervisor type: %s", hvType)
	}
	return starter, nil
}

func (m *manager) supportsSnapshotBaseReuse(hvType hypervisor.Type) bool {
	caps, ok := hypervisor.CapabilitiesForType(hvType)
	if !ok {
		return false
	}
	return caps.SupportsSnapshotBaseReuse
}

// getInstanceLock returns or creates a lock for a specific instance
func (m *manager) getInstanceLock(id string) *sync.RWMutex {
	lock, _ := m.instanceLocks.LoadOrStore(id, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

// maybePersistExitInfo persists exit info to metadata under the instance write lock.
// Called from read paths when in-memory exit info was parsed but not yet persisted.
func (m *manager) maybePersistExitInfo(ctx context.Context, id string) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	m.persistExitInfo(ctx, id)
}

// maybePersistBootMarkers persists boot markers to metadata under lock.
func (m *manager) maybePersistBootMarkers(ctx context.Context, id string) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	m.persistBootMarkers(ctx, id)
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

func (m *manager) ListSnapshots(ctx context.Context, filter *ListSnapshotsFilter) ([]Snapshot, error) {
	return m.listSnapshots(ctx, filter)
}

func (m *manager) GetSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error) {
	return m.getSnapshot(ctx, snapshotID)
}

func (m *manager) CreateSnapshot(ctx context.Context, id string, req CreateSnapshotRequest) (*Snapshot, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.createSnapshot(ctx, id, req)
}

func (m *manager) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	return m.deleteSnapshot(ctx, snapshotID)
}

// ForkInstance creates a forked copy of an instance.
func (m *manager) ForkInstance(ctx context.Context, id string, req ForkInstanceRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	forked, targetState, err := m.forkInstance(ctx, id, req)
	lock.Unlock()
	if err != nil {
		return nil, err
	}

	inst, err := m.applyForkTargetState(ctx, forked.Id, targetState)
	if err != nil {
		if cleanupErr := m.cleanupForkInstanceOnError(ctx, forked.Id); cleanupErr != nil {
			return nil, fmt.Errorf("apply fork target state: %w; additionally failed to cleanup forked instance %s: %v", err, forked.Id, cleanupErr)
		}
		return nil, fmt.Errorf("apply fork target state: %w", err)
	}
	if inst.State == StateRunning {
		if err := ensureGuestAgentReadyForForkPhase(ctx, &inst.StoredMetadata, "before returning running fork instance"); err != nil {
			if cleanupErr := m.cleanupForkInstanceOnError(ctx, forked.Id); cleanupErr != nil {
				return nil, fmt.Errorf("wait for fork guest agent readiness: %w; additionally failed to cleanup forked instance %s: %v", err, forked.Id, cleanupErr)
			}
			return nil, fmt.Errorf("wait for fork guest agent readiness: %w", err)
		}
	}
	return inst, nil
}

func (m *manager) ForkSnapshot(ctx context.Context, snapshotID string, req ForkSnapshotRequest) (*Instance, error) {
	return m.forkSnapshot(ctx, snapshotID, req)
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

func (m *manager) RestoreSnapshot(ctx context.Context, id string, snapshotID string, req RestoreSnapshotRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.restoreSnapshot(ctx, id, snapshotID, req)
}

// StopInstance gracefully stops a running instance
func (m *manager) StopInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.stopInstance(ctx, id)
}

// StartInstance starts a stopped instance with optional command overrides
func (m *manager) StartInstance(ctx context.Context, id string, req StartInstanceRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.startInstance(ctx, id, req)
}

// ListInstances returns instances, optionally filtered by the given criteria.
// Pass nil to return all instances.
func (m *manager) ListInstances(ctx context.Context, filter *ListInstancesFilter) ([]Instance, error) {
	// No lock - eventual consistency is acceptable for list operations.
	// State is derived dynamically, so list is always reasonably current.
	all, err := m.listInstances(ctx)
	if err != nil {
		return nil, err
	}
	result := all
	if filter != nil {
		filtered := make([]Instance, 0, len(all))
		for i := range all {
			if filter.Matches(&all[i]) {
				filtered = append(filtered, all[i])
			}
		}
		result = filtered
	}

	for i := range result {
		inst := result[i]
		if (inst.State == StateRunning || inst.State == StateInitializing) && inst.BootMarkersHydrated {
			m.maybePersistBootMarkers(ctx, inst.Id)
		}
	}

	return result, nil
}

// GetInstance returns an instance by ID, name, or ID prefix.
// Lookup order: exact ID match -> exact name match -> ID prefix match.
// Returns ErrAmbiguousName if prefix matches multiple instances.
func (m *manager) GetInstance(ctx context.Context, idOrName string) (*Instance, error) {
	// 1. Try exact ID match first (most common case)
	lock := m.getInstanceLock(idOrName)
	lock.RLock()
	inst, err := m.getInstance(ctx, idOrName)
	lock.RUnlock()
	if err == nil {
		// If VM is stopped with unpersisted exit info, persist under write lock.
		// This handles the "app exited on its own" case where stopInstance wasn't called.
		if inst.State == StateStopped && inst.ExitCode != nil {
			m.maybePersistExitInfo(ctx, inst.Id)
		}
		if (inst.State == StateRunning || inst.State == StateInitializing) && inst.BootMarkersHydrated {
			m.maybePersistBootMarkers(ctx, inst.Id)
		}
		return inst, nil
	}

	// 2. List all instances for name and prefix matching
	instances, err := m.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}

	// 3. Try exact name match
	var nameMatches []Instance
	for _, inst := range instances {
		if inst.Name == idOrName {
			nameMatches = append(nameMatches, inst)
		}
	}
	if len(nameMatches) == 1 {
		inst := &nameMatches[0]
		if inst.State == StateStopped && inst.ExitCode != nil {
			m.maybePersistExitInfo(ctx, inst.Id)
		}
		return inst, nil
	}
	if len(nameMatches) > 1 {
		return nil, ErrAmbiguousName
	}

	// 4. Try ID prefix match
	var prefixMatches []Instance
	for _, inst := range instances {
		if len(idOrName) > 0 && len(inst.Id) >= len(idOrName) && inst.Id[:len(idOrName)] == idOrName {
			prefixMatches = append(prefixMatches, inst)
		}
	}
	if len(prefixMatches) == 1 {
		inst := &prefixMatches[0]
		if inst.State == StateStopped && inst.ExitCode != nil {
			m.maybePersistExitInfo(ctx, inst.Id)
		}
		return inst, nil
	}
	if len(prefixMatches) > 1 {
		return nil, ErrAmbiguousName
	}

	return nil, ErrNotFound
}

// StreamInstanceLogs streams instance logs from the specified source
// Returns last N lines, then continues following if follow=true
func (m *manager) StreamInstanceLogs(ctx context.Context, id string, tail int, follow bool, source LogSource) (<-chan string, error) {
	// Note: No lock held during streaming - we read from the file continuously
	// and the file is append-only, so this is safe
	return m.streamInstanceLogs(ctx, id, tail, follow, source)
}

// RotateLogs rotates all instance logs (app, vmm, hypeman) that exceed maxBytes
func (m *manager) RotateLogs(ctx context.Context, maxBytes int64, maxFiles int) error {
	instances, err := m.listInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances for rotation: %w", err)
	}

	var lastErr error
	for _, inst := range instances {
		// Rotate all three log types
		logPaths := []string{
			m.paths.InstanceAppLog(inst.Id),
			m.paths.InstanceVMMLog(inst.Id),
			m.paths.InstanceHypemanLog(inst.Id),
		}
		for _, logPath := range logPaths {
			if err := rotateLogIfNeeded(logPath, maxBytes, maxFiles); err != nil {
				lastErr = err // Continue with other logs, but track error
			}
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

// ListInstanceAllocations returns resource allocations for all instances.
// Used by the resource manager for capacity tracking.
func (m *manager) ListInstanceAllocations(ctx context.Context) ([]resources.InstanceAllocation, error) {
	instances, err := m.listInstances(ctx)
	if err != nil {
		return nil, err
	}

	allocations := make([]resources.InstanceAllocation, 0, len(instances))
	for _, inst := range instances {
		// Calculate volume bytes and volume overlay bytes separately
		var volumeBytes int64
		var volumeOverlayBytes int64
		for _, vol := range inst.Volumes {
			// Get actual volume size from volume manager
			if m.volumeManager != nil {
				if volume, err := m.volumeManager.GetVolume(ctx, vol.VolumeID); err == nil {
					volumeBytes += int64(volume.SizeGb) * 1024 * 1024 * 1024
				}
			}
			// Track overlay size separately for overlay volumes
			if vol.Overlay {
				volumeOverlayBytes += vol.OverlaySize
			}
		}

		allocations = append(allocations, resources.InstanceAllocation{
			ID:                 inst.Id,
			Name:               inst.Name,
			Vcpus:              inst.Vcpus,
			MemoryBytes:        inst.Size + inst.HotplugSize,
			OverlayBytes:       inst.OverlaySize,
			VolumeOverlayBytes: volumeOverlayBytes,
			NetworkDownloadBps: inst.NetworkBandwidthDownload,
			NetworkUploadBps:   inst.NetworkBandwidthUpload,
			DiskIOBps:          inst.DiskIOBps,
			State:              string(inst.State),
			VolumeBytes:        volumeBytes,
		})
	}

	return allocations, nil
}

// ListRunningInstancesInfo returns info needed for utilization metrics collection.
// Used by the resource manager for VM utilization tracking.
// Includes active VMs in Running or Initializing state.
func (m *manager) ListRunningInstancesInfo(ctx context.Context) ([]resources.InstanceUtilizationInfo, error) {
	instances, err := m.listInstances(ctx)
	if err != nil {
		return nil, err
	}

	infos := make([]resources.InstanceUtilizationInfo, 0, len(instances))
	for _, inst := range instances {
		// Only include active instances (they have a hypervisor process)
		if inst.State != StateRunning && inst.State != StateInitializing {
			continue
		}

		info := resources.InstanceUtilizationInfo{
			ID:            inst.Id,
			Name:          inst.Name,
			HypervisorPID: inst.HypervisorPID,
			// Include allocated resources for utilization ratio calculations
			AllocatedVcpus:       inst.Vcpus,
			AllocatedMemoryBytes: inst.Size + inst.HotplugSize,
		}

		// Derive TAP device name if networking is enabled
		if inst.NetworkEnabled {
			info.TAPDevice = network.GenerateTAPName(inst.Id)
		}

		infos = append(infos, info)
	}

	return infos, nil
}
