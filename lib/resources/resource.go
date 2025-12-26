// Package resources provides host resource discovery, capacity tracking,
// and oversubscription-aware allocation management for CPU, memory, disk, and network.
package resources

import (
	"context"
	"fmt"
	"sync"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/paths"
)

// ResourceType identifies a type of host resource.
type ResourceType string

const (
	ResourceCPU     ResourceType = "cpu"
	ResourceMemory  ResourceType = "memory"
	ResourceDisk    ResourceType = "disk"
	ResourceNetwork ResourceType = "network"
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
	Source         string       `json:"source,omitempty"` // How capacity was determined (e.g., "detected", "configured")
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
	Images   int64 `json:"images_bytes"`
	Volumes  int64 `json:"volumes_bytes"`
	Overlays int64 `json:"overlays_bytes"`
}

// FullResourceStatus is the complete resource status for the API response.
type FullResourceStatus struct {
	CPU         ResourceStatus        `json:"cpu"`
	Memory      ResourceStatus        `json:"memory"`
	Disk        ResourceStatus        `json:"disk"`
	Network     ResourceStatus        `json:"network"`
	DiskDetail  *DiskBreakdown        `json:"disk_breakdown,omitempty"`
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
	MemoryBytes        int64 // Size + HotplugSize
	OverlayBytes       int64
	NetworkDownloadBps int64  // Download rate limit (external→VM)
	NetworkUploadBps   int64  // Upload rate limit (VM→external)
	State              string // Only count running/paused/created instances
	VolumeBytes        int64  // Sum of attached volume sizes
}

// ImageLister provides access to image sizes for disk calculations.
type ImageLister interface {
	// TotalImageBytes returns the total size of all images on disk.
	TotalImageBytes(ctx context.Context) (int64, error)
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
	m.resources[ResourceCPU] = cpu

	// Discover memory
	mem, err := NewMemoryResource()
	if err != nil {
		return fmt.Errorf("discover memory: %w", err)
	}
	m.resources[ResourceMemory] = mem

	// Discover disk
	disk, err := NewDiskResource(m.cfg, m.paths, m.instanceLister, m.imageLister, m.volumeLister)
	if err != nil {
		return fmt.Errorf("discover disk: %w", err)
	}
	m.resources[ResourceDisk] = disk

	// Discover network
	net, err := NewNetworkResource(m.cfg, m.instanceLister)
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
			status.Source = "configured"
		} else {
			status.Source = "detected"
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
	if disk, ok := m.resources[ResourceDisk].(*DiskResource); ok {
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
		if err == nil {
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

	return &FullResourceStatus{
		CPU:         *cpuStatus,
		Memory:      *memStatus,
		Disk:        *diskStatus,
		Network:     *netStatus,
		DiskDetail:  diskBreakdown,
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
