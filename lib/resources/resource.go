// Package resources provides host resource discovery, capacity tracking,
// and oversubscription-aware allocation management for CPU, memory, disk, and network.
package resources

import (
	"context"
	"fmt"
	"sync"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
)

// ResourceType identifies a type of host resource.
type ResourceType string

const (
	ResourceCPU     ResourceType = "cpu"
	ResourceMemory  ResourceType = "memory"
	ResourceDisk    ResourceType = "disk"
	ResourceNetwork ResourceType = "network"
	ResourceDiskIO  ResourceType = "disk_io"
)

// SourceType identifies how a resource capacity was determined.
type SourceType string

const (
	SourceDetected   SourceType = "detected"   // Auto-detected from host hardware
	SourceConfigured SourceType = "configured" // Explicitly configured by operator
)

var (
	gpuStatusProviderMu sync.RWMutex
	gpuStatusProvider   = GetGPUStatus
)

func currentGPUStatusProvider() func(context.Context) *GPUResourceStatus {
	gpuStatusProviderMu.RLock()
	defer gpuStatusProviderMu.RUnlock()
	return gpuStatusProvider
}

func setGPUStatusProvider(fn func(context.Context) *GPUResourceStatus) {
	if fn == nil {
		fn = GetGPUStatus
	}
	gpuStatusProviderMu.Lock()
	defer gpuStatusProviderMu.Unlock()
	gpuStatusProvider = fn
}

// Resource represents a discoverable and allocatable host resource.
type Resource interface {
	// Type returns the resource type identifier.
	Type() ResourceType

	// Capacity returns the raw host capacity (before oversubscription).
	Capacity() int64

	// Allocated returns current total allocation across all instances.
	Allocated(ctx context.Context) (int64, error)
}

// ResourceStatus represents the current state of a resource type.
type ResourceStatus struct {
	Type           ResourceType `json:"type"`
	Capacity       int64        `json:"capacity"`         // Raw host capacity
	EffectiveLimit int64        `json:"effective_limit"`  // Capacity * oversubscription ratio
	Allocated      int64        `json:"allocated"`        // Currently allocated
	Available      int64        `json:"available"`        // EffectiveLimit - Allocated
	OversubRatio   float64      `json:"oversub_ratio"`    // Oversubscription ratio applied
	Source         SourceType   `json:"source,omitempty"` // How capacity was determined
}

// AllocationBreakdown shows per-instance resource allocations.
type AllocationBreakdown struct {
	InstanceID         string `json:"instance_id"`
	InstanceName       string `json:"instance_name"`
	CPU                int    `json:"cpu"`
	MemoryBytes        int64  `json:"memory_bytes"`
	DiskBytes          int64  `json:"disk_bytes"`
	NetworkDownloadBps int64  `json:"network_download_bps"` // External→VM
	NetworkUploadBps   int64  `json:"network_upload_bps"`   // VM→External
	DiskIOBps          int64  `json:"disk_io_bps"`          // Disk I/O bandwidth
}

// DiskBreakdown shows disk usage by category.
type DiskBreakdown struct {
	Images   int64 `json:"images_bytes"`    // Exported rootfs disk files
	OCICache int64 `json:"oci_cache_bytes"` // OCI layer cache (shared blobs)
	Volumes  int64 `json:"volumes_bytes"`
	Overlays int64 `json:"overlays_bytes"` // Rootfs overlays + volume overlays
}

// FullResourceStatus is the complete resource status for the API response.
type FullResourceStatus struct {
	CPU         ResourceStatus        `json:"cpu"`
	Memory      ResourceStatus        `json:"memory"`
	Disk        ResourceStatus        `json:"disk"`
	Network     ResourceStatus        `json:"network"`
	DiskIO      ResourceStatus        `json:"disk_io"`
	DiskDetail  *DiskBreakdown        `json:"disk_breakdown,omitempty"`
	GPU         *GPUResourceStatus    `json:"gpu,omitempty"` // nil if no GPU available
	Allocations []AllocationBreakdown `json:"allocations"`
}

// InstanceLister provides access to instance data for allocation calculations.
type InstanceLister interface {
	// ListInstanceAllocations returns resource allocations for all instances.
	ListInstanceAllocations(ctx context.Context) ([]InstanceAllocation, error)
}

// InstanceAllocation represents the resources allocated to a single instance.
type InstanceAllocation struct {
	ID                 string
	Name               string
	Vcpus              int
	MemoryBytes        int64  // Size + HotplugSize
	OverlayBytes       int64  // Rootfs overlay size
	VolumeOverlayBytes int64  // Sum of volume overlay sizes
	NetworkDownloadBps int64  // Download rate limit (external→VM)
	NetworkUploadBps   int64  // Upload rate limit (VM→external)
	DiskIOBps          int64  // Disk I/O rate limit (bytes/sec)
	State              string // Only count running/paused/created instances
	VolumeBytes        int64  // Sum of attached volume base sizes (for per-instance reporting)
}

// ImageLister provides access to image sizes for disk calculations.
type ImageLister interface {
	// TotalImageBytes returns the total size of all images on disk.
	TotalImageBytes(ctx context.Context) (int64, error)
	// TotalOCICacheBytes returns the total size of the OCI layer cache.
	TotalOCICacheBytes(ctx context.Context) (int64, error)
}

// VolumeLister provides access to volume sizes for disk calculations.
type VolumeLister interface {
	// TotalVolumeBytes returns the total size of all volumes.
	TotalVolumeBytes(ctx context.Context) (int64, error)
}

// InstanceUtilizationInfo contains the minimal info needed to collect VM utilization metrics.
// Used by vm_metrics package via adapter.
type InstanceUtilizationInfo struct {
	ID            string
	Name          string
	HypervisorPID *int   // PID of the hypervisor process
	TAPDevice     string // Name of the TAP device (e.g., "hype-01234567")

	// Allocated resources (for computing utilization ratios)
	AllocatedVcpus       int   // Number of allocated vCPUs
	AllocatedMemoryBytes int64 // Allocated memory in bytes (Size + HotplugSize)
}

type pendingAllocation struct {
	CPU         int64
	MemoryBytes int64
	DiskBytes   int64
	NetworkBps  int64
	DiskIOBps   int64
	GPUSlots    int
}

type admissionUsage struct {
	CPU         int64
	MemoryBytes int64
	DiskBytes   int64
	NetworkBps  int64
	DiskIOBps   int64
}

// Manager coordinates resource discovery and allocation tracking.
type Manager struct {
	cfg   *config.Config
	paths *paths.Paths

	mu         sync.RWMutex
	resources  map[ResourceType]Resource
	monitoring *monitoringState

	// Dependencies for allocation calculations
	instanceLister InstanceLister
	imageLister    ImageLister
	volumeLister   VolumeLister

	// Pending allocations are used only for admission decisions.
	pending       map[string]pendingAllocation
	pendingTotals pendingAllocation
}

// NewManager creates a new resource manager.
func NewManager(cfg *config.Config, p *paths.Paths) *Manager {
	return &Manager{
		cfg:        cfg,
		paths:      p,
		resources:  make(map[ResourceType]Resource),
		monitoring: &monitoringState{},
		pending:    make(map[string]pendingAllocation),
	}
}

// SetInstanceLister sets the instance lister for allocation calculations.
func (m *Manager) SetInstanceLister(lister InstanceLister) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instanceLister = lister
}

// SetImageLister sets the image lister for disk calculations.
func (m *Manager) SetImageLister(lister ImageLister) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imageLister = lister
}

// SetVolumeLister sets the volume lister for disk calculations.
func (m *Manager) SetVolumeLister(lister VolumeLister) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.volumeLister = lister
}

// Initialize discovers host resources and registers them.
// Must be called after setting listers and before using the manager.
func (m *Manager) Initialize(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Discover CPU
	cpu, err := NewCPUResource()
	if err != nil {
		return fmt.Errorf("discover CPU: %w", err)
	}
	cpu.SetInstanceLister(m.instanceLister)
	m.resources[ResourceCPU] = cpu

	// Discover memory
	mem, err := NewMemoryResource()
	if err != nil {
		return fmt.Errorf("discover memory: %w", err)
	}
	mem.SetInstanceLister(m.instanceLister)
	m.resources[ResourceMemory] = mem

	// Discover disk
	disk, err := NewDiskResource(m.cfg, m.paths, m.instanceLister, m.imageLister, m.volumeLister)
	if err != nil {
		return fmt.Errorf("discover disk: %w", err)
	}
	m.resources[ResourceDisk] = disk

	// Discover network
	net, err := NewNetworkResource(ctx, m.cfg, m.instanceLister)
	if err != nil {
		return fmt.Errorf("discover network: %w", err)
	}
	m.resources[ResourceNetwork] = net

	// Discover disk I/O (reuses existing DiskIOCapacity method)
	diskIO := NewDiskIOResource(m.DiskIOCapacity(), m.instanceLister)
	m.resources[ResourceDiskIO] = diskIO

	// Warm fast allocation-related caches at startup so the first reservation or
	// resource status read doesn't pay the one-time scan cost on the hot path.
	if m.instanceLister != nil {
		if _, err := m.instanceLister.ListInstanceAllocations(ctx); err != nil {
			return fmt.Errorf("warm instance allocations: %w", err)
		}
	}
	if m.imageLister != nil {
		if _, err := m.imageLister.TotalImageBytes(ctx); err != nil {
			return fmt.Errorf("warm image disk usage totals: %w", err)
		}
		if _, err := m.imageLister.TotalOCICacheBytes(ctx); err != nil {
			return fmt.Errorf("warm OCI cache usage totals: %w", err)
		}
	}
	if m.volumeLister != nil {
		if _, err := m.volumeLister.TotalVolumeBytes(ctx); err != nil {
			return fmt.Errorf("warm volume usage totals: %w", err)
		}
	}

	return nil
}

// GetOversubRatio returns the oversubscription ratio for a resource type.
// Returns 1.0 (no oversubscription) if the config value is not set or <= 0.
func (m *Manager) GetOversubRatio(rt ResourceType) float64 {
	var ratio float64
	switch rt {
	case ResourceCPU:
		ratio = m.cfg.Oversubscription.CPU
	case ResourceMemory:
		ratio = m.cfg.Oversubscription.Memory
	case ResourceDisk:
		ratio = m.cfg.Oversubscription.Disk
	case ResourceNetwork:
		ratio = m.cfg.Oversubscription.Network
	case ResourceDiskIO:
		ratio = m.cfg.Oversubscription.DiskIO
	default:
		return 1.0
	}
	// Default to 1.0 (no oversubscription) if not configured
	if ratio <= 0 {
		return 1.0
	}
	return ratio
}

// GetStatus returns the current status of a specific resource type.
func (m *Manager) GetStatus(ctx context.Context, rt ResourceType) (*ResourceStatus, error) {
	m.mu.RLock()
	res, ok := m.resources[rt]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown resource type: %s", rt)
	}

	capacity := res.Capacity()
	ratio := m.GetOversubRatio(rt)
	effectiveLimit := int64(float64(capacity) * ratio)

	allocated, err := res.Allocated(ctx)
	if err != nil {
		return nil, fmt.Errorf("get allocated %s: %w", rt, err)
	}

	available := effectiveLimit - allocated
	if available < 0 {
		available = 0
	}

	status := &ResourceStatus{
		Type:           rt,
		Capacity:       capacity,
		EffectiveLimit: effectiveLimit,
		Allocated:      allocated,
		Available:      available,
		OversubRatio:   ratio,
	}

	// Add source info for network
	if rt == ResourceNetwork {
		if m.cfg.Capacity.Network != "" {
			status.Source = SourceConfigured
		} else {
			status.Source = SourceDetected
		}
	}

	return status, nil
}

// GetFullStatus returns the complete resource status for all resource types.
func (m *Manager) GetFullStatus(ctx context.Context) (*FullResourceStatus, error) {
	cpuStatus, err := m.GetStatus(ctx, ResourceCPU)
	if err != nil {
		return nil, err
	}

	memStatus, err := m.GetStatus(ctx, ResourceMemory)
	if err != nil {
		return nil, err
	}

	diskStatus, err := m.GetStatus(ctx, ResourceDisk)
	if err != nil {
		return nil, err
	}

	netStatus, err := m.GetStatus(ctx, ResourceNetwork)
	if err != nil {
		return nil, err
	}

	diskIOStatus, err := m.GetStatus(ctx, ResourceDiskIO)
	if err != nil {
		return nil, err
	}

	// Get disk breakdown
	var diskBreakdown *DiskBreakdown
	m.mu.RLock()
	diskRes := m.resources[ResourceDisk]
	m.mu.RUnlock()
	if disk, ok := diskRes.(*DiskResource); ok {
		breakdown, err := disk.GetBreakdown(ctx)
		if err == nil {
			diskBreakdown = breakdown
		}
	}

	// Get per-instance allocations
	var allocations []AllocationBreakdown
	m.mu.RLock()
	lister := m.instanceLister
	m.mu.RUnlock()

	if lister != nil {
		instances, err := lister.ListInstanceAllocations(ctx)
		if err != nil {
			log := logger.FromContext(ctx)
			log.WarnContext(ctx, "failed to list instance allocations for resource status", "error", err)
		} else {
			for _, inst := range instances {
				// Only include active instances
				if inst.State == "Running" || inst.State == "Paused" || inst.State == "Created" {
					allocations = append(allocations, AllocationBreakdown{
						InstanceID:         inst.ID,
						InstanceName:       inst.Name,
						CPU:                inst.Vcpus,
						MemoryBytes:        inst.MemoryBytes,
						DiskBytes:          inst.OverlayBytes + inst.VolumeBytes,
						NetworkDownloadBps: inst.NetworkDownloadBps,
						NetworkUploadBps:   inst.NetworkUploadBps,
						DiskIOBps:          inst.DiskIOBps,
					})
				}
			}
		}
	}

	// Get GPU status
	gpuStatus := currentGPUStatusProvider()(ctx)

	return &FullResourceStatus{
		CPU:         *cpuStatus,
		Memory:      *memStatus,
		Disk:        *diskStatus,
		Network:     *netStatus,
		DiskIO:      *diskIOStatus,
		DiskDetail:  diskBreakdown,
		GPU:         gpuStatus,
		Allocations: allocations,
	}, nil
}

// CanAllocate checks if the requested amount can be allocated for a resource type.
func (m *Manager) CanAllocate(ctx context.Context, rt ResourceType, amount int64) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	usage, err := m.collectAdmissionUsageLocked(ctx)
	if err != nil {
		return false, err
	}
	pending := m.pendingTotalsExcludingLocked("")

	var visibleAllocated int64
	switch rt {
	case ResourceCPU:
		visibleAllocated = usage.CPU
	case ResourceMemory:
		visibleAllocated = usage.MemoryBytes
	case ResourceDisk:
		visibleAllocated = usage.DiskBytes
	case ResourceNetwork:
		visibleAllocated = usage.NetworkBps
	case ResourceDiskIO:
		visibleAllocated = usage.DiskIOBps
	default:
		return false, fmt.Errorf("unsupported admission resource type: %s", rt)
	}

	status, err := m.admissionStatusLocked(rt, visibleAllocated, pending)
	if err != nil {
		return false, err
	}
	return amount <= status.Available, nil
}

func normalizeNetworkBandwidth(downloadBps, uploadBps int64) int64 {
	netBandwidth := downloadBps
	if uploadBps > netBandwidth {
		netBandwidth = uploadBps
	}
	return netBandwidth
}

func newPendingAllocation(vcpus int, memoryBytes int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, diskBytes int64, needsGPU bool) pendingAllocation {
	req := pendingAllocation{
		CPU:         int64(vcpus),
		MemoryBytes: memoryBytes,
		DiskBytes:   diskBytes,
		NetworkBps:  normalizeNetworkBandwidth(networkDownloadBps, networkUploadBps),
		DiskIOBps:   diskIOBps,
	}
	if needsGPU {
		req.GPUSlots = 1
	}
	return req
}

func (m *Manager) addPendingTotalsLocked(req pendingAllocation) {
	m.pendingTotals.CPU += req.CPU
	m.pendingTotals.MemoryBytes += req.MemoryBytes
	m.pendingTotals.DiskBytes += req.DiskBytes
	m.pendingTotals.NetworkBps += req.NetworkBps
	m.pendingTotals.DiskIOBps += req.DiskIOBps
	m.pendingTotals.GPUSlots += req.GPUSlots
}

func (m *Manager) subtractPendingTotalsLocked(req pendingAllocation) {
	m.pendingTotals.CPU -= req.CPU
	m.pendingTotals.MemoryBytes -= req.MemoryBytes
	m.pendingTotals.DiskBytes -= req.DiskBytes
	m.pendingTotals.NetworkBps -= req.NetworkBps
	m.pendingTotals.DiskIOBps -= req.DiskIOBps
	m.pendingTotals.GPUSlots -= req.GPUSlots
}

func (m *Manager) pendingTotalsExcludingLocked(excludeID string) pendingAllocation {
	total := m.pendingTotals
	if excludeID == "" {
		return total
	}
	if existing, ok := m.pending[excludeID]; ok {
		total.CPU -= existing.CPU
		total.MemoryBytes -= existing.MemoryBytes
		total.DiskBytes -= existing.DiskBytes
		total.NetworkBps -= existing.NetworkBps
		total.DiskIOBps -= existing.DiskIOBps
		total.GPUSlots -= existing.GPUSlots
	}
	return total
}

func (m *Manager) collectAdmissionUsageLocked(ctx context.Context) (admissionUsage, error) {
	var usage admissionUsage

	if m.instanceLister != nil {
		instances, err := m.instanceLister.ListInstanceAllocations(ctx)
		if err != nil {
			return admissionUsage{}, fmt.Errorf("list instance allocations: %w", err)
		}
		for _, inst := range instances {
			if !isActiveState(inst.State) {
				continue
			}
			usage.CPU += int64(inst.Vcpus)
			usage.MemoryBytes += inst.MemoryBytes
			usage.DiskBytes += inst.OverlayBytes + inst.VolumeOverlayBytes
			usage.DiskIOBps += inst.DiskIOBps

			allocNet := inst.NetworkDownloadBps
			if inst.NetworkUploadBps > allocNet {
				allocNet = inst.NetworkUploadBps
			}
			usage.NetworkBps += allocNet
		}
	}

	if m.imageLister != nil {
		imageBytes, err := m.imageLister.TotalImageBytes(ctx)
		if err != nil {
			return admissionUsage{}, fmt.Errorf("total image bytes: %w", err)
		}
		ociCacheBytes, err := m.imageLister.TotalOCICacheBytes(ctx)
		if err != nil {
			return admissionUsage{}, fmt.Errorf("total OCI cache bytes: %w", err)
		}
		usage.DiskBytes += imageBytes + ociCacheBytes
	}

	if m.volumeLister != nil {
		volumeBytes, err := m.volumeLister.TotalVolumeBytes(ctx)
		if err != nil {
			return admissionUsage{}, fmt.Errorf("total volume bytes: %w", err)
		}
		usage.DiskBytes += volumeBytes
	}

	return usage, nil
}

func (m *Manager) admissionStatusLocked(rt ResourceType, visibleAllocated int64, pending pendingAllocation) (*ResourceStatus, error) {
	res, ok := m.resources[rt]
	if !ok {
		return nil, fmt.Errorf("unknown resource type: %s", rt)
	}

	status := &ResourceStatus{
		Type:         rt,
		Capacity:     res.Capacity(),
		OversubRatio: m.GetOversubRatio(rt),
	}
	status.EffectiveLimit = int64(float64(status.Capacity) * status.OversubRatio)

	switch rt {
	case ResourceCPU:
		status.Allocated = visibleAllocated + pending.CPU
	case ResourceMemory:
		status.Allocated = visibleAllocated + pending.MemoryBytes
	case ResourceDisk:
		status.Allocated = visibleAllocated + pending.DiskBytes
	case ResourceNetwork:
		status.Allocated = visibleAllocated + pending.NetworkBps
		if m.cfg.Capacity.Network != "" {
			status.Source = SourceConfigured
		} else {
			status.Source = SourceDetected
		}
	case ResourceDiskIO:
		status.Allocated = visibleAllocated + pending.DiskIOBps
	default:
		return nil, fmt.Errorf("unsupported admission resource type: %s", rt)
	}

	status.Available = status.EffectiveLimit - status.Allocated
	if status.Available < 0 {
		status.Available = 0
	}

	return status, nil
}

func (m *Manager) validateAllocationLocked(ctx context.Context, excludeID string, req pendingAllocation) error {
	usage, err := m.collectAdmissionUsageLocked(ctx)
	if err != nil {
		return err
	}
	pending := m.pendingTotalsExcludingLocked(excludeID)

	// Check CPU
	if req.CPU > 0 {
		status, err := m.admissionStatusLocked(ResourceCPU, usage.CPU, pending)
		if err != nil {
			return fmt.Errorf("check CPU capacity: %w", err)
		}
		if req.CPU > status.Available {
			return fmt.Errorf("insufficient CPU: requested %d vCPUs, but only %d available (currently allocated: %d, effective limit: %d with %.1fx oversubscription)",
				req.CPU, status.Available, status.Allocated, status.EffectiveLimit, status.OversubRatio)
		}
	}

	// Check Memory
	if req.MemoryBytes > 0 {
		status, err := m.admissionStatusLocked(ResourceMemory, usage.MemoryBytes, pending)
		if err != nil {
			return fmt.Errorf("check memory capacity: %w", err)
		}
		if req.MemoryBytes > status.Available {
			return fmt.Errorf("insufficient memory: requested %s, but only %s available (currently allocated: %s, effective limit: %s with %.1fx oversubscription)",
				datasize.ByteSize(req.MemoryBytes).HR(), datasize.ByteSize(status.Available).HR(), datasize.ByteSize(status.Allocated).HR(), datasize.ByteSize(status.EffectiveLimit).HR(), status.OversubRatio)
		}
	}

	// Check Disk
	if req.DiskBytes > 0 {
		status, err := m.admissionStatusLocked(ResourceDisk, usage.DiskBytes, pending)
		if err != nil {
			return fmt.Errorf("check disk capacity: %w", err)
		}
		if req.DiskBytes > status.Available {
			return fmt.Errorf("insufficient disk: requested %s, but only %s available (currently allocated: %s, effective limit: %s with %.1fx oversubscription)",
				datasize.ByteSize(req.DiskBytes).HR(), datasize.ByteSize(status.Available).HR(), datasize.ByteSize(status.Allocated).HR(), datasize.ByteSize(status.EffectiveLimit).HR(), status.OversubRatio)
		}
	}

	// Check Network (use max of download/upload since they share physical link)
	if req.NetworkBps > 0 {
		status, err := m.admissionStatusLocked(ResourceNetwork, usage.NetworkBps, pending)
		if err != nil {
			return fmt.Errorf("check network capacity: %w", err)
		}
		if req.NetworkBps > status.Available {
			return fmt.Errorf("insufficient network bandwidth: requested %s/s, but only %s/s available (currently allocated: %s/s, effective limit: %s/s with %.1fx oversubscription)",
				datasize.ByteSize(req.NetworkBps).HR(), datasize.ByteSize(status.Available).HR(), datasize.ByteSize(status.Allocated).HR(), datasize.ByteSize(status.EffectiveLimit).HR(), status.OversubRatio)
		}
	}

	// Check Disk I/O
	if req.DiskIOBps > 0 {
		status, err := m.admissionStatusLocked(ResourceDiskIO, usage.DiskIOBps, pending)
		if err != nil {
			return fmt.Errorf("check disk I/O capacity: %w", err)
		}
		if req.DiskIOBps > status.Available {
			return fmt.Errorf("insufficient disk I/O: requested %s/s, but only %s/s available (currently allocated: %s/s, effective limit: %s/s with %.1fx oversubscription)",
				datasize.ByteSize(req.DiskIOBps).HR(), datasize.ByteSize(status.Available).HR(),
				datasize.ByteSize(status.Allocated).HR(), datasize.ByteSize(status.EffectiveLimit).HR(),
				status.OversubRatio)
		}
	}

	// Check GPU if needed
	if req.GPUSlots > 0 {
		gpuStatus := currentGPUStatusProvider()(ctx)
		if gpuStatus == nil {
			return fmt.Errorf("insufficient GPU: no GPU available on this host")
		}
		availableSlots := gpuStatus.TotalSlots - gpuStatus.UsedSlots - pending.GPUSlots
		if availableSlots < req.GPUSlots {
			if availableSlots <= 0 {
				return fmt.Errorf("insufficient GPU: all %d %s slots are in use",
					gpuStatus.TotalSlots, gpuStatus.Mode)
			}
			return fmt.Errorf("insufficient GPU: requested %d %s slot(s), but only %d available",
				req.GPUSlots, gpuStatus.Mode, availableSlots)
		}
	}

	return nil
}

// ValidateAllocation checks if the requested resources can be allocated.
// Returns nil if allocation is allowed, or a detailed error describing
// which resource is insufficient and the current capacity/usage.
func (m *Manager) ValidateAllocation(ctx context.Context, vcpus int, memoryBytes int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, diskBytes int64, needsGPU bool) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	req := newPendingAllocation(vcpus, memoryBytes, networkDownloadBps, networkUploadBps, diskIOBps, diskBytes, needsGPU)
	return m.validateAllocationLocked(ctx, "", req)
}

// ReserveAllocation tentatively reserves resources for an in-flight operation.
func (m *Manager) ReserveAllocation(ctx context.Context, instanceID string, vcpus int, memoryBytes int64, networkDownloadBps int64, networkUploadBps int64, diskIOBps int64, diskBytes int64, needsGPU bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	req := newPendingAllocation(vcpus, memoryBytes, networkDownloadBps, networkUploadBps, diskIOBps, diskBytes, needsGPU)
	if err := m.validateAllocationLocked(ctx, instanceID, req); err != nil {
		return err
	}
	if existing, ok := m.pending[instanceID]; ok {
		m.subtractPendingTotalsLocked(existing)
	}
	m.pending[instanceID] = req
	m.addPendingTotalsLocked(req)
	return nil
}

// FinishAllocation removes any pending reservation for the given instance ID.
func (m *Manager) FinishAllocation(instanceID string) {
	if instanceID == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.pending[instanceID]; ok {
		m.subtractPendingTotalsLocked(existing)
		delete(m.pending, instanceID)
	}
}

// CPUCapacity returns the raw CPU capacity (number of vCPUs).
func (m *Manager) CPUCapacity() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cpu, ok := m.resources[ResourceCPU]; ok {
		return cpu.Capacity()
	}
	return 0
}

// NetworkCapacity returns the raw network capacity in bytes/sec.
func (m *Manager) NetworkCapacity() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if net, ok := m.resources[ResourceNetwork]; ok {
		return net.Capacity()
	}
	return 0
}

// DiskIOCapacity returns the disk I/O capacity in bytes/sec.
// Uses configured DISK_IO_LIMIT if set, otherwise defaults to 1 GB/s.
func (m *Manager) DiskIOCapacity() int64 {
	if m.cfg.Capacity.DiskIO == "" {
		return 1 * 1000 * 1000 * 1000 // 1 GB/s default
	}
	// Parse the limit using the same format as network (e.g., "500MB/s")
	capacity, err := parseDiskIOLimit(m.cfg.Capacity.DiskIO)
	if err != nil {
		return 1 * 1000 * 1000 * 1000 // 1 GB/s fallback
	}
	return capacity
}

// DefaultNetworkBandwidth calculates the default network bandwidth for an instance
// based on its CPU allocation proportional to host CPU capacity.
// Formula: (instanceVcpus / hostCpuCapacity) * networkCapacity
// Returns symmetric download/upload limits.
func (m *Manager) DefaultNetworkBandwidth(vcpus int) (downloadBps, uploadBps int64) {
	cpuCapacity := m.CPUCapacity()
	if cpuCapacity == 0 {
		return 0, 0
	}

	netCapacity := m.NetworkCapacity()
	if netCapacity == 0 {
		return 0, 0
	}

	bandwidth := (int64(vcpus) * netCapacity) / cpuCapacity

	// Symmetric limits by default
	return bandwidth, bandwidth
}

// DefaultDiskIOBandwidth calculates the default disk I/O bandwidth for an instance
// based on its CPU allocation proportional to host CPU capacity.
// Formula: (instanceVcpus / hostCpuCapacity) * diskIOCapacity
// Returns sustained rate and burst rate (4x sustained).
func (m *Manager) DefaultDiskIOBandwidth(vcpus int) (ioBps, burstBps int64) {
	cpuCapacity := m.CPUCapacity()
	if cpuCapacity == 0 {
		return 0, 0
	}

	ioCapacity := m.DiskIOCapacity()
	if ioCapacity == 0 {
		return 0, 0
	}

	sustained := (int64(vcpus) * ioCapacity) / cpuCapacity

	// Burst is 4x sustained (allows fast cold starts)
	burst := sustained * 4

	return sustained, burst
}

// HasSufficientDiskForPull checks if there's enough disk space for an image pull.
// Returns an error if available disk is below the minimum threshold (5GB).
func (m *Manager) HasSufficientDiskForPull(ctx context.Context) error {
	const minDiskForPull = 5 * 1024 * 1024 * 1024 // 5GB

	status, err := m.GetStatus(ctx, ResourceDisk)
	if err != nil {
		return fmt.Errorf("check disk status: %w", err)
	}

	if status.Available < minDiskForPull {
		return fmt.Errorf("insufficient disk space for image pull: %d bytes available, minimum %d bytes required",
			status.Available, minDiskForPull)
	}

	return nil
}

// MaxImageStorageBytes returns the maximum allowed image storage (OCI cache + rootfs).
// Based on MaxImageStorage fraction (default 20%) of disk capacity.
func (m *Manager) MaxImageStorageBytes() int64 {
	m.mu.RLock()
	diskRes, ok := m.resources[ResourceDisk]
	m.mu.RUnlock()

	if !ok {
		return 0
	}

	capacity := diskRes.Capacity()
	fraction := m.cfg.Limits.MaxImageStorage
	if fraction <= 0 {
		fraction = 0.2 // Default 20%
	}

	return int64(float64(capacity) * fraction)
}

// CurrentImageStorageBytes returns the current image storage usage (OCI cache + rootfs).
func (m *Manager) CurrentImageStorageBytes(ctx context.Context) (int64, error) {
	if m.imageLister == nil {
		return 0, nil
	}

	rootfsBytes, err := m.imageLister.TotalImageBytes(ctx)
	if err != nil {
		return 0, err
	}

	ociCacheBytes, err := m.imageLister.TotalOCICacheBytes(ctx)
	if err != nil {
		return 0, err
	}

	return rootfsBytes + ociCacheBytes, nil
}

// HasSufficientImageStorage checks if pulling another image would exceed the image storage limit.
// Returns an error if current image storage >= max allowed.
func (m *Manager) HasSufficientImageStorage(ctx context.Context) error {
	current, err := m.CurrentImageStorageBytes(ctx)
	if err != nil {
		return fmt.Errorf("check image storage: %w", err)
	}

	max := m.MaxImageStorageBytes()
	if max > 0 && current >= max {
		return fmt.Errorf("image storage limit exceeded: %d bytes used, limit is %d bytes (%.0f%% of disk)",
			current, max, m.cfg.Limits.MaxImageStorage*100)
	}

	return nil
}
