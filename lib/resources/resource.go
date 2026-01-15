// Package resources provides host resource discovery, capacity tracking,
// and oversubscription-aware allocation management for CPU, memory, disk, and network.
package resources

import (
	"context"
	"fmt"
	"sync"

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
)

// SourceType identifies how a resource capacity was determined.
type SourceType string

const (
	SourceDetected   SourceType = "detected"   // Auto-detected from host hardware
	SourceConfigured SourceType = "configured" // Explicitly configured by operator
)

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

// Manager coordinates resource discovery and allocation tracking.
type Manager struct {
	cfg   *config.Config
	paths *paths.Paths

	mu        sync.RWMutex
	resources map[ResourceType]Resource

	// Dependencies for allocation calculations
	instanceLister InstanceLister
	imageLister    ImageLister
	volumeLister   VolumeLister
}

// NewManager creates a new resource manager.
func NewManager(cfg *config.Config, p *paths.Paths) *Manager {
	return &Manager{
		cfg:       cfg,
		paths:     p,
		resources: make(map[ResourceType]Resource),
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

	return nil
}

// GetOversubRatio returns the oversubscription ratio for a resource type.
func (m *Manager) GetOversubRatio(rt ResourceType) float64 {
	switch rt {
	case ResourceCPU:
		return m.cfg.OversubCPU
	case ResourceMemory:
		return m.cfg.OversubMemory
	case ResourceDisk:
		return m.cfg.OversubDisk
	case ResourceNetwork:
		return m.cfg.OversubNetwork
	default:
		return 1.0
	}
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
		if m.cfg.NetworkLimit != "" {
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
					})
				}
			}
		}
	}

	// Get GPU status
	gpuStatus := GetGPUStatus()

	return &FullResourceStatus{
		CPU:         *cpuStatus,
		Memory:      *memStatus,
		Disk:        *diskStatus,
		Network:     *netStatus,
		DiskDetail:  diskBreakdown,
		GPU:         gpuStatus,
		Allocations: allocations,
	}, nil
}

// CanAllocate checks if the requested amount can be allocated for a resource type.
func (m *Manager) CanAllocate(ctx context.Context, rt ResourceType, amount int64) (bool, error) {
	status, err := m.GetStatus(ctx, rt)
	if err != nil {
		return false, err
	}
	return amount <= status.Available, nil
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
	if m.cfg.DiskIOLimit == "" {
		return 1 * 1000 * 1000 * 1000 // 1 GB/s default
	}
	// Parse the limit using the same format as network (e.g., "500MB/s")
	capacity, err := parseDiskIOLimit(m.cfg.DiskIOLimit)
	if err != nil {
		return 1 * 1000 * 1000 * 1000 // 1 GB/s fallback
	}
	return capacity
}

// DefaultNetworkBandwidth calculates the default network bandwidth for an instance
// based on its CPU allocation proportional to host CPU capacity.
// Formula: (instanceVcpus / hostCpuCapacity) * networkCapacity * oversubRatio
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

	ratio := m.GetOversubRatio(ResourceNetwork)
	effectiveNet := int64(float64(netCapacity) * ratio)

	// Proportional to CPU: (vcpus / cpuCapacity) * effectiveNet
	bandwidth := (int64(vcpus) * effectiveNet) / cpuCapacity

	// Symmetric limits by default
	return bandwidth, bandwidth
}

// DefaultDiskIOBandwidth calculates the default disk I/O bandwidth for an instance
// based on its CPU allocation proportional to host CPU capacity.
// Formula: (instanceVcpus / hostCpuCapacity) * diskIOCapacity * oversubRatio
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

	ratio := m.cfg.OversubDiskIO
	if ratio <= 0 {
		ratio = 2.0 // Default 2x oversubscription for disk I/O
	}
	effectiveIO := int64(float64(ioCapacity) * ratio)

	// Proportional to CPU: (vcpus / cpuCapacity) * effectiveIO
	sustained := (int64(vcpus) * effectiveIO) / cpuCapacity

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
	fraction := m.cfg.MaxImageStorage
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
			current, max, m.cfg.MaxImageStorage*100)
	}

	return nil
}
