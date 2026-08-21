package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/egressproxy"
	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/uffdpager"
	"github.com/kernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Manager interface {
	ListInstances(ctx context.Context, filter *ListInstancesFilter) ([]Instance, error)
	ListInstancesForReconcile(ctx context.Context) ([]Instance, error)
	ListSnapshots(ctx context.Context, filter *ListSnapshotsFilter) ([]Snapshot, error)
	GetSnapshot(ctx context.Context, snapshotID string) (*Snapshot, error)
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error)
	CreateSnapshot(ctx context.Context, id string, req CreateSnapshotRequest) (*Snapshot, error)
	// GetInstance returns an instance by ID, name, or ID prefix.
	// Lookup order: exact ID match -> exact name match -> ID prefix match.
	// Returns ErrAmbiguousName if prefix matches multiple instances.
	GetInstance(ctx context.Context, idOrName string) (*Instance, error)
	DeleteInstance(ctx context.Context, id string) error
	DeleteInstanceWithOptions(ctx context.Context, id string, options DeleteInstanceOptions) error
	DeleteSnapshot(ctx context.Context, snapshotID string) error
	ForkInstance(ctx context.Context, id string, req ForkInstanceRequest) (*Instance, error)
	ForkSnapshot(ctx context.Context, snapshotID string, req ForkSnapshotRequest) (*Instance, error)
	StandbyInstance(ctx context.Context, id string, req StandbyInstanceRequest) (*Instance, error)
	RestoreInstance(ctx context.Context, id string) (*Instance, error)
	RestoreSnapshot(ctx context.Context, id string, snapshotID string, req RestoreSnapshotRequest) (*Instance, error)
	StopInstance(ctx context.Context, id string) (*Instance, error)
	StartInstance(ctx context.Context, id string, req StartInstanceRequest) (*Instance, error)
	UpdateInstance(ctx context.Context, id string, req UpdateInstanceRequest) (*Instance, error)
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
	// SubscribeLifecycleEvents returns the shared internal lifecycle event stream.
	SubscribeLifecycleEvents(consumer LifecycleEventConsumer) (<-chan LifecycleEvent, func())
}

// ImageUsageRecorder records newly used images before instance metadata is persisted.
type ImageUsageRecorder interface {
	MarkUsed(ctx context.Context, imageName, digest string) error
}

// ImageUsageRecorderSetter configures an optional image usage recorder on the manager.
type ImageUsageRecorderSetter interface {
	SetImageUsageRecorder(recorder ImageUsageRecorder)
}

// ResourceLimits contains configurable resource limits for instances
type ResourceLimits struct {
	MaxOverlaySize       int64 // Maximum overlay disk size in bytes per instance
	MaxVcpusPerInstance  int   // Maximum vCPUs per instance (0 = unlimited)
	MaxMemoryPerInstance int64 // Maximum memory in bytes per instance (0 = unlimited)
}

// ManagerConfig holds non-resource manager behavior settings.
type ManagerConfig struct {
	LifecycleEventBufferSize          int
	FirecrackerSnapshotMemoryBackend  string
	FirecrackerUFFDCacheMaxBytes      int64
	MaxConcurrentRestoresByHypervisor map[hypervisor.Type]int
}

const defaultManagerMaxConcurrentRestores = 4

type restoreSlotPoolKey struct {
	hypervisorType hypervisor.Type
	limit          int
}

var restoreSlotPools sync.Map // map[restoreSlotPoolKey]chan struct{}

// Normalize applies defaults to manager config values.
func (c ManagerConfig) Normalize() ManagerConfig {
	if c.LifecycleEventBufferSize <= 0 {
		c.LifecycleEventBufferSize = defaultLifecycleEventBufferSize
	}
	c.FirecrackerSnapshotMemoryBackend = strings.ToLower(strings.TrimSpace(c.FirecrackerSnapshotMemoryBackend))
	if c.FirecrackerSnapshotMemoryBackend == "" {
		c.FirecrackerSnapshotMemoryBackend = uffdpager.BackendFile
	}
	if c.FirecrackerUFFDCacheMaxBytes <= 0 {
		c.FirecrackerUFFDCacheMaxBytes = 4 << 30
	}
	if c.MaxConcurrentRestoresByHypervisor == nil {
		c.MaxConcurrentRestoresByHypervisor = map[hypervisor.Type]int{
			hypervisor.TypeFirecracker: defaultManagerMaxConcurrentRestores,
		}
	} else {
		limits := make(map[hypervisor.Type]int, len(c.MaxConcurrentRestoresByHypervisor))
		for hvType, limit := range c.MaxConcurrentRestoresByHypervisor {
			if limit <= 0 {
				limit = defaultManagerMaxConcurrentRestores
			}
			limits[hvType] = limit
		}
		c.MaxConcurrentRestoresByHypervisor = limits
	}
	return c
}

func sharedRestoreSlots(hvType hypervisor.Type, limit int) chan struct{} {
	if limit <= 0 {
		limit = defaultManagerMaxConcurrentRestores
	}
	key := restoreSlotPoolKey{hypervisorType: hvType, limit: limit}
	slots, _ := restoreSlotPools.LoadOrStore(key, make(chan struct{}, limit))
	return slots.(chan struct{})
}

func sharedRestoreSlotsByHypervisor(limits map[hypervisor.Type]int) map[hypervisor.Type]chan struct{} {
	if len(limits) == 0 {
		return nil
	}
	slots := make(map[hypervisor.Type]chan struct{}, len(limits))
	for hvType, limit := range limits {
		slots[hvType] = sharedRestoreSlots(hvType, limit)
	}
	return slots
}

// ResourceValidator validates if resources can be allocated
type ResourceValidator interface {
	// ValidateAllocation checks if the requested resources are available.
	// Returns nil if allocation is allowed, or a detailed error describing
	// which resource is insufficient and the current capacity/usage.
	ValidateAllocation(ctx context.Context, vcpus int, memoryBytes int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, diskBytes int64, needsGPU bool) error
	// ReserveAllocation tentatively reserves resources for an in-flight operation.
	// Call FinishAllocation once the operation fails or becomes visible to resource accounting.
	ReserveAllocation(ctx context.Context, instanceID string, vcpus int, memoryBytes int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, diskBytes int64, needsGPU bool) error
	// FinishAllocation removes any pending reservation for the given instance ID.
	FinishAllocation(instanceID string)
}

type manager struct {
	paths                     *paths.Paths
	imageManager              images.Manager
	systemManager             system.Manager
	networkManager            network.Manager
	deviceManager             devices.Manager
	volumeManager             volumes.Manager
	limits                    ResourceLimits
	resourceValidator         ResourceValidator // Optional validator for aggregate resource limits
	instanceLocks             sync.Map          // map[string]*sync.RWMutex - per-instance locks
	forkMetadataMu            sync.Mutex
	bootMarkerScans           sync.Map      // map[string]time.Time next allowed boot-marker rescan
	hypervisorStateCache      sync.Map      // map[string]hypervisorStateCacheEntry - last observed hypervisor state per instance
	hostTopology              *HostTopology // Cached host CPU topology
	metrics                   *Metrics
	meter                     metric.Meter
	tracer                    trace.Tracer
	now                       func() time.Time
	writeFile                 func(string, []byte, os.FileMode) error
	deleteInstanceFn          func(context.Context, string) error
	createVGPU                func(context.Context, string, string) (*devices.VGPUDevice, error)
	destroyVGPU               func(context.Context, devices.VGPUAssignment) error
	deleteSnapshotFn          func(context.Context, string) error
	ttlReaperDeleteTimeout    time.Duration
	egressProxy               *egressproxy.Service
	egressProxyServiceOptions egressproxy.ServiceOptions
	egressProxyMu             sync.Mutex
	snapshotDefaults          SnapshotPolicy
	compressionMu             sync.Mutex
	compressionJobs           map[string]*compressionJob
	snapshotPrepareLocks      sync.Map // map[string]*sync.Mutex - per-snapshot memory prepare locks
	compressionTimerFactory   func(time.Duration) compressionTimer
	nativeCodecMu             sync.Mutex
	nativeCodecPaths          map[string]string
	imageUsageRecorder        ImageUsageRecorder
	guestAgentReadyProbe      func(context.Context, *StoredMetadata) bool
	shutdownGuestFn           func(context.Context, hypervisor.VsockDialer, int32) error

	// Shared lifecycle event subscriptions for internal consumers.
	lifecycleEvents *lifecycleSubscribers

	// Cached conservative allocation view for fast admission control.
	admissionAllocationsMu     sync.RWMutex
	admissionAllocations       map[string]resources.InstanceAllocation
	admissionAllocationsLoaded bool
	admissionReconcileOnce     sync.Once

	// Periodic TAP garbage collection reconciler.
	tapGCOnce sync.Once

	// vGPU assignments that survived a completed delete, keyed by device
	// path, each with a background release retry in flight.
	// orphanedVGPURetryDelay overrides the retry delay in tests; zero means
	// the default.
	orphanedVGPUMu         sync.Mutex
	orphanedVGPUs          map[string]struct{}
	orphanedVGPURetryDelay time.Duration

	// vgpuInitTermGrace overrides terminateThenKill's SIGTERM wait for vGPU
	// instances still initializing; zero means the default.
	vgpuInitTermGrace time.Duration

	// Hypervisor support
	vmStarters                       map[hypervisor.Type]hypervisor.VMStarter
	defaultHypervisor                hypervisor.Type // Default hypervisor type when not specified in request
	guestMemoryPolicy                guestmemory.Policy
	firecrackerSnapshotMemoryBackend string
	firecrackerUFFDPager             *uffdpager.Supervisor
	restoreSlotsByHypervisor         map[hypervisor.Type]chan struct{}
}

// platformStarters is populated by platform-specific init functions.
var platformStarters = make(map[hypervisor.Type]hypervisor.VMStarter)

// NewManager creates a new instances manager.
// If meter is nil, metrics are disabled.
// defaultHypervisor specifies which hypervisor to use when not specified in requests.
func NewManager(p *paths.Paths, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, deviceManager devices.Manager, volumeManager volumes.Manager, limits ResourceLimits, defaultHypervisor hypervisor.Type, snapshotDefaults SnapshotPolicy, meter metric.Meter, tracer trace.Tracer, memoryPolicy ...guestmemory.Policy) Manager {
	return NewManagerWithConfig(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, defaultHypervisor, snapshotDefaults, ManagerConfig{}, meter, tracer, memoryPolicy...)
}

// NewManagerWithConfig creates a new instances manager with additional manager settings.
func NewManagerWithConfig(p *paths.Paths, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, deviceManager devices.Manager, volumeManager volumes.Manager, limits ResourceLimits, defaultHypervisor hypervisor.Type, snapshotDefaults SnapshotPolicy, managerConfig ManagerConfig, meter metric.Meter, tracer trace.Tracer, memoryPolicy ...guestmemory.Policy) Manager {
	m, err := NewManagerWithConfigE(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, defaultHypervisor, snapshotDefaults, managerConfig, meter, tracer, memoryPolicy...)
	if err != nil {
		panic(err)
	}
	return m
}

// NewManagerWithConfigE creates a new instances manager and returns startup
// errors for optional host services such as the Firecracker UFFD pager.
func NewManagerWithConfigE(p *paths.Paths, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, deviceManager devices.Manager, volumeManager volumes.Manager, limits ResourceLimits, defaultHypervisor hypervisor.Type, snapshotDefaults SnapshotPolicy, managerConfig ManagerConfig, meter metric.Meter, tracer trace.Tracer, memoryPolicy ...guestmemory.Policy) (Manager, error) {
	// Validate and default the hypervisor type
	if defaultHypervisor == "" {
		defaultHypervisor = hypervisor.TypeCloudHypervisor
	}

	policy := guestmemory.DefaultPolicy()
	if len(memoryPolicy) > 0 {
		policy = memoryPolicy[0]
	}
	policy = policy.Normalize()
	managerConfig = managerConfig.Normalize()

	// Initialize VM starters from platform-specific init functions
	rawStarters := make(map[hypervisor.Type]hypervisor.VMStarter, len(platformStarters))
	for hvType, starter := range platformStarters {
		rawStarters[hvType] = starter
	}
	firecrackerUFFDPager, err := configurePlatformStarters(context.Background(), p, rawStarters, managerConfig)
	if err != nil {
		return nil, err
	}
	vmStarters := make(map[hypervisor.Type]hypervisor.VMStarter, len(rawStarters))
	for hvType, starter := range rawStarters {
		vmStarters[hvType] = hypervisor.WrapVMStarter(hvType, starter)
	}

	m := &manager{
		paths:                            p,
		imageManager:                     imageManager,
		systemManager:                    systemManager,
		networkManager:                   networkManager,
		deviceManager:                    deviceManager,
		volumeManager:                    volumeManager,
		limits:                           limits,
		instanceLocks:                    sync.Map{},
		bootMarkerScans:                  sync.Map{},
		hostTopology:                     detectHostTopology(), // Detect and cache host topology
		vmStarters:                       vmStarters,
		defaultHypervisor:                defaultHypervisor,
		now:                              time.Now,
		writeFile:                        os.WriteFile,
		createVGPU:                       devices.CreateVGPU,
		destroyVGPU:                      devices.DestroyVGPU,
		meter:                            meter,
		tracer:                           tracer,
		guestMemoryPolicy:                policy,
		firecrackerSnapshotMemoryBackend: managerConfig.FirecrackerSnapshotMemoryBackend,
		firecrackerUFFDPager:             firecrackerUFFDPager,
		restoreSlotsByHypervisor:         sharedRestoreSlotsByHypervisor(managerConfig.MaxConcurrentRestoresByHypervisor),
		snapshotDefaults:                 snapshotDefaults,
		compressionJobs:                  make(map[string]*compressionJob),
		nativeCodecPaths:                 make(map[string]string),
		lifecycleEvents:                  newLifecycleSubscribersWithBufferSize(managerConfig.LifecycleEventBufferSize),
		guestAgentReadyProbe:             probeGuestAgentReady,
	}
	m.deleteSnapshotFn = m.deleteSnapshot

	// Initialize metrics if meter is provided
	if meter != nil {
		metrics, err := newInstanceMetrics(meter, tracer, m)
		if err == nil {
			m.metrics = metrics
		}
	}
	m.lifecycleEvents.onDrop = func(ctx context.Context, consumer LifecycleEventConsumer) {
		m.recordLifecycleEventDropped(ctx, consumer, lifecycleEventDropReasonBufferFull)
	}
	if err := m.recoverPendingStandbyCompressionJobs(context.Background()); err != nil {
		logger.FromContext(context.Background()).WarnContext(context.Background(), "failed to recover pending standby compression jobs", "error", err)
	}

	return m, nil
}

// SetResourceValidator sets the resource validator for aggregate limit checking.
// This is called after initialization to avoid circular dependencies.
func (m *manager) SetResourceValidator(v ResourceValidator) {
	m.resourceValidator = v
}

// SetImageUsageRecorder configures an optional recorder for pre-persistence image usage.
func (m *manager) SetImageUsageRecorder(recorder ImageUsageRecorder) {
	m.imageUsageRecorder = recorder
}

func (m *manager) SubscribeLifecycleEvents(consumer LifecycleEventConsumer) (<-chan LifecycleEvent, func()) {
	return m.lifecycleEvents.Subscribe(consumer)
}

func (m *manager) notifyLifecycleEvent(ctx context.Context, action LifecycleEventAction, inst *Instance) {
	if inst == nil {
		return
	}
	m.updateCachedHypervisorStateFromInstance(inst)
	m.lifecycleEvents.Notify(ctx, LifecycleEvent{
		Action:     action,
		InstanceID: inst.Id,
		Instance:   inst,
	})
}

func (m *manager) notifyLifecycleDelete(ctx context.Context, instanceID string) {
	m.invalidateCachedHypervisorState(instanceID)
	m.lifecycleEvents.Notify(ctx, LifecycleEvent{
		Action:     LifecycleEventDelete,
		InstanceID: instanceID,
	})
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

func (m *manager) supportsConcurrentForkPrepare(hvType hypervisor.Type) bool {
	caps, ok := hypervisor.CapabilitiesForType(hvType)
	if !ok {
		return false
	}
	return caps.SupportsConcurrentForkPrepare
}

// getInstanceLock returns or creates a lock for a specific instance
func (m *manager) getInstanceLock(id string) *sync.RWMutex {
	lock, _ := m.loadOrStoreInstanceLock(id)
	return lock
}

func (m *manager) loadOrStoreInstanceLock(id string) (*sync.RWMutex, bool) {
	lock, loaded := m.instanceLocks.LoadOrStore(id, &sync.RWMutex{})
	return lock.(*sync.RWMutex), !loaded
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
	ctx, span := m.tracerOrDefault().Start(ctx, "instances.persist_boot_markers",
		traceWithInstanceID(id),
	)
	defer span.End()
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	m.persistBootMarkers(ctx, id)
}

func (m *manager) finalizeResolvedInstance(ctx context.Context, inst *Instance) {
	if inst.State == StateStopped && inst.ExitCode != nil {
		m.maybePersistExitInfo(ctx, inst.Id)
	}
	if (inst.State == StateRunning || inst.State == StateInitializing) && inst.BootMarkersHydrated {
		m.maybePersistBootMarkers(ctx, inst.Id)
	}
}

func (m *manager) recordImageUsage(ctx context.Context, imageInfo *images.Image) {
	if m.imageUsageRecorder == nil || imageInfo == nil {
		return
	}
	if err := m.imageUsageRecorder.MarkUsed(ctx, imageInfo.Name, imageInfo.Digest); err != nil {
		log := logger.FromContext(ctx)
		log.WarnContext(ctx, "failed to record image usage", "image", imageInfo.Name, "digest", imageInfo.Digest, "error", err)
	}
}

// CreateInstance creates and starts a new instance
func (m *manager) CreateInstance(ctx context.Context, req CreateInstanceRequest) (*Instance, error) {
	// Note: ID is generated inside createInstance, so we can't lock before calling it.
	// This is safe because:
	// 1. ULID generation is unique
	// 2. Filesystem mkdir is atomic per instance directory
	// 3. Concurrent creates of different instances don't conflict
	inst, err := m.createInstance(ctx, req)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventCreate, inst)
	}
	return inst, err
}

// DeleteInstance stops and deletes an instance.
func (m *manager) DeleteInstance(ctx context.Context, id string) error {
	return m.DeleteInstanceWithOptions(ctx, id, DeleteInstanceOptions{})
}

// DeleteInstanceWithOptions stops and deletes an instance with the supplied options.
func (m *manager) DeleteInstanceWithOptions(ctx context.Context, id string, options DeleteInstanceOptions) error {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()

	return m.deleteInstanceLockedWithOptions(ctx, id, options)
}

func (m *manager) deleteInstanceLocked(ctx context.Context, id string) error {
	return m.deleteInstanceLockedWithOptions(ctx, id, DeleteInstanceOptions{})
}

func (m *manager) deleteInstanceLockedWithOptions(ctx context.Context, id string, options DeleteInstanceOptions) error {
	deleteInstance := func(ctx context.Context, id string) error {
		return m.deleteInstanceWithOptions(ctx, id, options)
	}
	if m.deleteInstanceFn != nil {
		deleteInstance = m.deleteInstanceFn
	}
	err := deleteInstance(ctx, id)
	if err == nil {
		m.notifyLifecycleDelete(ctx, id)
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
	useReadLock := false
	var sourceState State
	// Some hypervisors can safely prepare stopped/standby forks concurrently
	// from the same source. Probe under a short read lock first; running sources
	// and unsupported hypervisors fall back to the normal exclusive fork path.
	lock.RLock()
	if meta, err := m.loadMetadata(id); err == nil {
		source := m.toInstance(ctx, meta)
		sourceState = source.State
		useReadLock = m.supportsConcurrentForkPrepare(meta.HypervisorType) && (source.State == StateStopped || source.State == StateStandby)
	}
	lock.RUnlock()

	var forked *Instance
	var targetState State
	var targetRestoreNeedsSourceLock bool
	var err error
	if useReadLock {
		// State may have changed after the first probe. Re-load while holding
		// the lock used by forkInstance, and only proceed under RLock if the
		// source is still in a concurrency-safe state.
		lock.RLock()
		if meta, loadErr := m.loadMetadata(id); loadErr == nil {
			source := m.toInstance(ctx, meta)
			sourceState = source.State
			useReadLock = m.supportsConcurrentForkPrepare(meta.HypervisorType) && (source.State == StateStopped || source.State == StateStandby)
		} else {
			useReadLock = false
		}
		if useReadLock {
			forked, targetState, targetRestoreNeedsSourceLock, err = m.forkInstance(ctx, id, req)
		}
		lock.RUnlock()
	}
	if !useReadLock {
		lock.Lock()
		forked, targetState, targetRestoreNeedsSourceLock, err = m.forkInstance(ctx, id, req)
		lock.Unlock()
	}
	if err != nil {
		return nil, err
	}

	inst := forked
	if !forkTargetStateAlreadyApplied(inst, targetState) {
		// Standby -> running forks prepared under RLock can still require the
		// source write lock if Firecracker snapshot paths could not be rewritten
		// and restore must temporarily alias the source data dir.
		if useReadLock && sourceState == StateStandby && targetState == StateRunning && targetRestoreNeedsSourceLock {
			inst, err = m.applyForkTargetStateWithSourceLock(ctx, lock, id, forked.Id, targetState)
		} else {
			inst, err = m.applyForkTargetState(ctx, forked.Id, targetState)
		}
		if err != nil {
			if cleanupErr := m.cleanupForkInstanceOnError(ctx, forked.Id); cleanupErr != nil {
				return nil, fmt.Errorf("apply fork target state: %w; additionally failed to cleanup forked instance %s: %v", err, forked.Id, cleanupErr)
			}
			return nil, fmt.Errorf("apply fork target state: %w", err)
		}
	}
	m.notifyLifecycleEvent(ctx, LifecycleEventFork, inst)
	return inst, nil
}

func (m *manager) applyForkTargetStateWithSourceLock(ctx context.Context, lock *sync.RWMutex, sourceID, forkID string, target State) (*Instance, error) {
	lock.Lock()
	defer lock.Unlock()
	sourceMeta, err := m.loadMetadata(sourceID)
	if err != nil {
		return nil, fmt.Errorf("reload source metadata before aliased fork restore: %w", err)
	}
	source := m.toInstance(ctx, sourceMeta)
	if source.State != StateStandby {
		return nil, fmt.Errorf("%w: cannot restore fork %s with snapshot source alias because source %s is now %s", ErrInvalidState, forkID, sourceID, source.State)
	}
	return m.applyForkTargetState(ctx, forkID, target)
}

func (m *manager) ForkSnapshot(ctx context.Context, snapshotID string, req ForkSnapshotRequest) (*Instance, error) {
	inst, err := m.forkSnapshot(ctx, snapshotID, req)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventFork, inst)
	}
	return inst, err
}

// StandbyInstance puts an instance in standby (pause, snapshot, delete VMM)
func (m *manager) StandbyInstance(ctx context.Context, id string, req StandbyInstanceRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	if !standbyRequestHasOptions(req) {
		current, err := m.currentInstanceWithoutHydration(ctx, id)
		if err != nil {
			return nil, err
		}
		if current.State == StateStandby {
			return current, nil
		}
	}
	inst, err := m.standbyInstance(ctx, id, req, false)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventStandby, inst)
	}
	return inst, err
}

// RestoreInstance restores an instance from standby
func (m *manager) RestoreInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	current, err := m.currentInstanceWithoutHydration(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.State == StateRunning || current.State == StateInitializing {
		return current, nil
	}
	inst, err := m.restoreInstance(ctx, id)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventRestore, inst)
	}
	return inst, err
}

func (m *manager) RestoreSnapshot(ctx context.Context, id string, snapshotID string, req RestoreSnapshotRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	inst, err := m.restoreSnapshot(ctx, id, snapshotID, req)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventRestore, inst)
	}
	return inst, err
}

// StopInstance gracefully stops a running instance
func (m *manager) StopInstance(ctx context.Context, id string) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	current, err := m.currentInstanceWithoutHydration(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.State == StateStopped {
		if err := m.markRestartManualStopLocked(ctx, id); err != nil {
			return nil, err
		}
		// A stopped instance can retain a vGPU assignment when the release
		// failed during the original stop. Retry it here so the vGPU slot is
		// not held until the next start, delete, or hypeman restart. A failed
		// retry only logs, keeping stop's no-op contract for already-stopped
		// instances.
		m.releaseRetainedVGPULocked(ctx, id)
		updated, err := m.currentInstanceWithoutHydration(ctx, id)
		if err != nil {
			return nil, err
		}
		return updated, nil
	}
	if err := m.markRestartManualStopLocked(ctx, id); err != nil {
		return nil, err
	}
	inst, err := m.stopInstance(ctx, id)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventStop, inst)
	}
	return inst, err
}

// StartInstance starts a stopped instance with optional command overrides
func (m *manager) StartInstance(ctx context.Context, id string, req StartInstanceRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := m.clearRestartStatusLocked(ctx, id); err != nil {
		return nil, err
	}
	if !startRequestHasOverrides(req) {
		current, err := m.currentInstanceWithoutHydration(ctx, id)
		if err != nil {
			return nil, err
		}
		if current.State == StateRunning || current.State == StateInitializing {
			return current, nil
		}
	}
	inst, err := m.startInstance(ctx, id, req)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventStart, inst)
	}
	return inst, err
}

func (m *manager) currentInstanceWithoutHydration(ctx context.Context, id string) (*Instance, error) {
	meta, err := m.loadMetadata(id)
	if err != nil {
		return nil, err
	}
	inst := m.toInstanceWithoutHydration(ctx, meta)
	return &inst, nil
}

func startRequestHasOverrides(req StartInstanceRequest) bool {
	return len(req.Entrypoint) > 0 || len(req.Cmd) > 0
}

func standbyRequestHasOptions(req StandbyInstanceRequest) bool {
	return req.Compression != nil || req.CompressionDelay != nil
}

// UpdateInstance updates mutable properties of a running instance
func (m *manager) UpdateInstance(ctx context.Context, id string, req UpdateInstanceRequest) (*Instance, error) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	inst, err := m.updateInstance(ctx, id, req)
	if err == nil {
		m.notifyLifecycleEvent(ctx, LifecycleEventUpdate, inst)
	}
	return inst, err
}

// DefaultHypervisor returns the effective default hypervisor type used for
// launches that do not specify one. It is deliberately not part of the
// Manager interface — widening that interface would break alternate
// implementations (mocks, wrappers) compiled against this public module — so
// callers that need it type-assert for this method instead.
func (m *manager) DefaultHypervisor() hypervisor.Type {
	return m.defaultHypervisor
}

// ListInstancesForReconcile returns every instance's stored metadata or an
// invalid metadata error. It does not derive state: reconcile protection only
// needs raw metadata fields, and hydration would query the hypervisor of
// every instance on the host before the API serves.
func (m *manager) ListInstancesForReconcile(ctx context.Context) ([]Instance, error) {
	files, err := m.listMetadataFilesStrict()
	if err != nil {
		return nil, err
	}
	result := make([]Instance, 0, len(files))
	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		meta, err := m.loadMetadata(id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Deleted between listing and load; a vanished record cannot
				// claim a VF. Failing here instead would zero the grace-period
				// retry and disable the vendor VFIO sweep whenever it races a
				// concurrent delete.
				continue
			}
			return nil, fmt.Errorf("load metadata for instance %s: %w", id, err)
		}
		result = append(result, Instance{StoredMetadata: meta.StoredMetadata})
	}
	return result, nil
}

// ListInstances returns instances, optionally filtered by the given criteria.
// Pass nil to return all instances.
func (m *manager) ListInstances(ctx context.Context, filter *ListInstancesFilter) ([]Instance, error) {
	ctx, span := m.tracerOrDefault().Start(ctx, "instances.list")
	defer span.End()
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
	span.SetAttributes(attribute.Int("instances", len(result)))

	persistCtx, persistSpan := m.tracerOrDefault().Start(ctx, "instances.list.persist_boot_markers")
	persisted := 0
	for i := range result {
		inst := result[i]
		if (inst.State == StateRunning || inst.State == StateInitializing) && inst.BootMarkersHydrated {
			m.maybePersistBootMarkers(persistCtx, inst.Id)
			persisted++
		}
	}
	persistSpan.SetAttributes(attribute.Int("persisted", persisted))
	persistSpan.End()

	return result, nil
}

// GetInstance returns an instance by ID, name, or ID prefix.
// Lookup order: exact ID match -> exact name match -> ID prefix match.
// Returns ErrAmbiguousName if prefix matches multiple instances.
func (m *manager) GetInstance(ctx context.Context, idOrName string) (*Instance, error) {
	return m.getInstanceWithMinIDPrefix(ctx, idOrName, 1)
}

func (m *manager) getInstanceWithMinIDPrefix(ctx context.Context, idOrName string, minPrefixLength int) (*Instance, error) {
	// 1. Try exact ID match first (most common case)
	lock := m.getInstanceLock(idOrName)
	lock.RLock()
	inst, err := m.getInstance(ctx, idOrName)
	lock.RUnlock()
	if err == nil {
		m.finalizeResolvedInstance(ctx, inst)
		return inst, nil
	}

	// 2. Resolve exact name or ID prefix from metadata only, then hydrate the
	// single matched instance.
	meta, err := m.findInstanceMetadataByNameOrIDPrefix(idOrName, minPrefixLength)
	if err != nil {
		return nil, err
	}

	resolvedLock := m.getInstanceLock(meta.Id)
	resolvedLock.RLock()
	inst, err = m.getInstance(ctx, meta.Id)
	resolvedLock.RUnlock()
	if err != nil {
		return nil, err
	}
	m.finalizeResolvedInstance(ctx, inst)
	return inst, nil
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
