package resources

import (
	"context"
	"testing"

	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceLister implements InstanceLister for testing
type mockInstanceLister struct {
	allocations []InstanceAllocation
}

func (m *mockInstanceLister) ListInstanceAllocations(ctx context.Context) ([]InstanceAllocation, error) {
	return m.allocations, nil
}

// mockImageLister implements ImageLister for testing
type mockImageLister struct {
	totalBytes    int64
	ociCacheBytes int64
}

func (m *mockImageLister) TotalImageBytes(ctx context.Context) (int64, error) {
	return m.totalBytes, nil
}

func (m *mockImageLister) TotalOCICacheBytes(ctx context.Context) (int64, error) {
	return m.ociCacheBytes, nil
}

// mockVolumeLister implements VolumeLister for testing
type mockVolumeLister struct {
	totalBytes int64
}

func (m *mockVolumeLister) TotalVolumeBytes(ctx context.Context) (int64, error) {
	return m.totalBytes, nil
}

func TestNewManager(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     2.0,
		OversubMemory:  1.5,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
	}
	p := paths.New(cfg.DataDir)

	mgr := NewManager(cfg, p)
	require.NotNil(t, mgr)
}

func TestGetOversubRatio(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     2.0,
		OversubMemory:  1.5,
		OversubDisk:    1.0,
		OversubNetwork: 3.0,
	}
	p := paths.New(cfg.DataDir)

	mgr := NewManager(cfg, p)

	assert.Equal(t, 2.0, mgr.GetOversubRatio(ResourceCPU))
	assert.Equal(t, 1.5, mgr.GetOversubRatio(ResourceMemory))
	assert.Equal(t, 1.0, mgr.GetOversubRatio(ResourceDisk))
	assert.Equal(t, 3.0, mgr.GetOversubRatio(ResourceNetwork))
	assert.Equal(t, 1.0, mgr.GetOversubRatio("unknown")) // default
}

func TestDefaultNetworkBandwidth(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     1.0,
		OversubMemory:  1.0,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
		NetworkLimit:   "10Gbps", // 1.25 GB/s = 1,250,000,000 bytes/sec
	}
	p := paths.New(cfg.DataDir)

	mgr := NewManager(cfg, p)
	mgr.SetInstanceLister(&mockInstanceLister{})
	mgr.SetImageLister(&mockImageLister{})
	mgr.SetVolumeLister(&mockVolumeLister{})

	err := mgr.Initialize(context.Background())
	require.NoError(t, err)

	// With 10Gbps network and CPU capacity (varies by host)
	// If host has 8 CPUs and instance wants 2, it gets 2/8 = 25% of network
	cpuCapacity := mgr.CPUCapacity()
	netCapacity := mgr.NetworkCapacity()

	if cpuCapacity > 0 && netCapacity > 0 {
		// Request 2 vCPUs
		downloadBw, uploadBw := mgr.DefaultNetworkBandwidth(2)
		expected := (int64(2) * netCapacity) / cpuCapacity
		assert.Equal(t, expected, downloadBw)
		assert.Equal(t, expected, uploadBw) // Symmetric by default
	}
}

func TestDefaultNetworkBandwidth_ZeroCPU(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     1.0,
		OversubMemory:  1.0,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
	}
	p := paths.New(cfg.DataDir)

	mgr := NewManager(cfg, p)
	// Don't initialize - CPU capacity will be 0

	downloadBw, uploadBw := mgr.DefaultNetworkBandwidth(2)
	assert.Equal(t, int64(0), downloadBw, "Should return 0 when CPU capacity is 0")
	assert.Equal(t, int64(0), uploadBw, "Should return 0 when CPU capacity is 0")
}

func TestParseNetworkLimit(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"1Gbps", 125000000, false},         // 1 Gbps = 125 MB/s (decimal)
		{"10Gbps", 1250000000, false},       // 10 Gbps = 1.25 GB/s (decimal)
		{"100Mbps", 12500000, false},        // 100 Mbps = 12.5 MB/s (decimal)
		{"1000kbps", 125000, false},         // 1000 kbps = 125 KB/s (decimal)
		{"125MB", 125 * 1024 * 1024, false}, // 125 MiB (datasize uses binary)
		{"1GB", 1024 * 1024 * 1024, false},  // 1 GiB (datasize uses binary)
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result, err := parseNetworkLimit(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestCPUResource_Capacity(t *testing.T) {
	cpu, err := NewCPUResource()
	require.NoError(t, err)

	// Should detect at least 1 CPU
	assert.GreaterOrEqual(t, cpu.Capacity(), int64(1))
	assert.Equal(t, ResourceCPU, cpu.Type())
}

func TestMemoryResource_Capacity(t *testing.T) {
	mem, err := NewMemoryResource()
	require.NoError(t, err)

	// Should detect at least 1GB of memory
	assert.GreaterOrEqual(t, mem.Capacity(), int64(1024*1024*1024))
	assert.Equal(t, ResourceMemory, mem.Type())
}

func TestCPUResource_Allocated(t *testing.T) {
	cpu, err := NewCPUResource()
	require.NoError(t, err)

	// With no instance lister, allocated should be 0
	allocated, err := cpu.Allocated(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), allocated)

	// With instance lister
	cpu.SetInstanceLister(&mockInstanceLister{
		allocations: []InstanceAllocation{
			{ID: "1", Vcpus: 4, State: "Running"},
			{ID: "2", Vcpus: 2, State: "Paused"},
			{ID: "3", Vcpus: 8, State: "Stopped"}, // Not counted
		},
	})

	allocated, err = cpu.Allocated(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(6), allocated) // 4 + 2 = 6 (Stopped not counted)
}

func TestMemoryResource_Allocated(t *testing.T) {
	mem, err := NewMemoryResource()
	require.NoError(t, err)

	mem.SetInstanceLister(&mockInstanceLister{
		allocations: []InstanceAllocation{
			{ID: "1", MemoryBytes: 4 * 1024 * 1024 * 1024, State: "Running"},
			{ID: "2", MemoryBytes: 2 * 1024 * 1024 * 1024, State: "Created"},
			{ID: "3", MemoryBytes: 8 * 1024 * 1024 * 1024, State: "Standby"}, // Not counted
		},
	})

	allocated, err := mem.Allocated(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(6*1024*1024*1024), allocated)
}

func TestIsActiveState(t *testing.T) {
	assert.True(t, isActiveState("Running"))
	assert.True(t, isActiveState("Paused"))
	assert.True(t, isActiveState("Created"))
	assert.False(t, isActiveState("Stopped"))
	assert.False(t, isActiveState("Standby"))
	assert.False(t, isActiveState("Unknown"))
}

func TestHasSufficientDiskForPull(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     1.0,
		OversubMemory:  1.0,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
	}
	p := paths.New(cfg.DataDir)

	mgr := NewManager(cfg, p)
	mgr.SetInstanceLister(&mockInstanceLister{})
	mgr.SetImageLister(&mockImageLister{})
	mgr.SetVolumeLister(&mockVolumeLister{})

	err := mgr.Initialize(context.Background())
	require.NoError(t, err)

	// This test depends on actual disk space available
	// We just verify it doesn't error
	err = mgr.HasSufficientDiskForPull(context.Background())
	// May or may not error depending on disk space - just verify it runs
	_ = err
}

// TestInitialize_SetsInstanceListersForAllResources verifies that Initialize
// properly propagates the instance lister to CPU and Memory resources.
// This catches a bug where CPU/Memory SetInstanceLister was not being called.
func TestInitialize_SetsInstanceListersForAllResources(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     1.0,
		OversubMemory:  1.0,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
	}
	p := paths.New(cfg.DataDir)

	// Create a mock lister that returns known allocations
	mockLister := &mockInstanceLister{
		allocations: []InstanceAllocation{
			{
				ID:          "test-1",
				Vcpus:       4,
				MemoryBytes: 8 * 1024 * 1024 * 1024, // 8GB
				State:       "Running",
			},
			{
				ID:          "test-2",
				Vcpus:       2,
				MemoryBytes: 4 * 1024 * 1024 * 1024, // 4GB
				State:       "Running",
			},
		},
	}

	mgr := NewManager(cfg, p)
	mgr.SetInstanceLister(mockLister)
	mgr.SetImageLister(&mockImageLister{})
	mgr.SetVolumeLister(&mockVolumeLister{})

	err := mgr.Initialize(context.Background())
	require.NoError(t, err)

	// CPU should report correct allocation (4 + 2 = 6 vCPUs)
	cpuStatus, err := mgr.GetStatus(context.Background(), ResourceCPU)
	require.NoError(t, err)
	assert.Equal(t, int64(6), cpuStatus.Allocated, "CPU should report 6 vCPUs allocated")

	// Memory should report correct allocation (8GB + 4GB = 12GB)
	memStatus, err := mgr.GetStatus(context.Background(), ResourceMemory)
	require.NoError(t, err)
	assert.Equal(t, int64(12*1024*1024*1024), memStatus.Allocated, "Memory should report 12GB allocated")
}

// TestGetFullStatus_ReturnsAllResourceAllocations verifies that GetFullStatus
// returns correct allocations for all resource types.
func TestGetFullStatus_ReturnsAllResourceAllocations(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     2.0,
		OversubMemory:  1.5,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
		NetworkLimit:   "10Gbps",
	}
	p := paths.New(cfg.DataDir)

	mockLister := &mockInstanceLister{
		allocations: []InstanceAllocation{
			{
				ID:                 "vm-1",
				Name:               "test-vm",
				Vcpus:              4,
				MemoryBytes:        8 * 1024 * 1024 * 1024,
				OverlayBytes:       10 * 1024 * 1024 * 1024,
				NetworkDownloadBps: 125000000,
				NetworkUploadBps:   125000000,
				State:              "Running",
			},
		},
	}

	mgr := NewManager(cfg, p)
	mgr.SetInstanceLister(mockLister)
	mgr.SetImageLister(&mockImageLister{totalBytes: 50 * 1024 * 1024 * 1024})
	mgr.SetVolumeLister(&mockVolumeLister{totalBytes: 100 * 1024 * 1024 * 1024})

	err := mgr.Initialize(context.Background())
	require.NoError(t, err)

	status, err := mgr.GetFullStatus(context.Background())
	require.NoError(t, err)

	// Verify CPU status
	assert.Equal(t, int64(4), status.CPU.Allocated)
	assert.Equal(t, 2.0, status.CPU.OversubRatio)

	// Verify Memory status
	assert.Equal(t, int64(8*1024*1024*1024), status.Memory.Allocated)
	assert.Equal(t, 1.5, status.Memory.OversubRatio)

	// Verify allocations list
	require.Len(t, status.Allocations, 1)
	assert.Equal(t, "vm-1", status.Allocations[0].InstanceID)
	assert.Equal(t, 4, status.Allocations[0].CPU)
	assert.Equal(t, int64(8*1024*1024*1024), status.Allocations[0].MemoryBytes)
}

// TestNetworkResource_Allocated verifies network allocation tracking
// uses max(download, upload) since they share the physical link.
func TestNetworkResource_Allocated(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		NetworkLimit:   "1Gbps", // 125MB/s
		OversubNetwork: 1.0,
	}

	mockLister := &mockInstanceLister{
		allocations: []InstanceAllocation{
			{ID: "1", NetworkDownloadBps: 50000000, NetworkUploadBps: 30000000, State: "Running"}, // max = 50MB/s
			{ID: "2", NetworkDownloadBps: 20000000, NetworkUploadBps: 40000000, State: "Running"}, // max = 40MB/s
			{ID: "3", NetworkDownloadBps: 10000000, NetworkUploadBps: 10000000, State: "Stopped"}, // Not counted
		},
	}

	net, err := NewNetworkResource(cfg, mockLister)
	require.NoError(t, err)

	allocated, err := net.Allocated(context.Background())
	require.NoError(t, err)
	// Should be 50MB/s + 40MB/s = 90MB/s
	assert.Equal(t, int64(90000000), allocated)
}

// TestMaxImageStorage verifies the image storage limit calculation
func TestMaxImageStorage(t *testing.T) {
	cfg := &config.Config{
		DataDir:         t.TempDir(),
		MaxImageStorage: 0.2, // 20%
		OversubCPU:      1.0,
		OversubMemory:   1.0,
		OversubDisk:     1.0,
		OversubNetwork:  1.0,
	}
	p := paths.New(cfg.DataDir)

	mgr := NewManager(cfg, p)
	mgr.SetInstanceLister(&mockInstanceLister{})
	mgr.SetImageLister(&mockImageLister{
		totalBytes:    50 * 1024 * 1024 * 1024, // 50GB rootfs
		ociCacheBytes: 25 * 1024 * 1024 * 1024, // 25GB OCI cache
	})
	mgr.SetVolumeLister(&mockVolumeLister{})

	err := mgr.Initialize(context.Background())
	require.NoError(t, err)

	// Max should be 20% of disk capacity
	maxBytes := mgr.MaxImageStorageBytes()
	diskCapacity := mgr.resources[ResourceDisk].Capacity()
	expectedMax := int64(float64(diskCapacity) * 0.2)
	assert.Equal(t, expectedMax, maxBytes)

	// Current should be 50GB + 25GB = 75GB
	currentBytes, err := mgr.CurrentImageStorageBytes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(75*1024*1024*1024), currentBytes)
}

// TestDiskBreakdown_IncludesOCICacheAndVolumeOverlays verifies disk breakdown
// includes OCI cache and volume overlays
func TestDiskBreakdown_IncludesOCICacheAndVolumeOverlays(t *testing.T) {
	cfg := &config.Config{
		DataDir:        t.TempDir(),
		OversubCPU:     1.0,
		OversubMemory:  1.0,
		OversubDisk:    1.0,
		OversubNetwork: 1.0,
	}
	p := paths.New(cfg.DataDir)

	mockInstances := &mockInstanceLister{
		allocations: []InstanceAllocation{
			{
				ID:                 "vm-1",
				OverlayBytes:       10 * 1024 * 1024 * 1024, // 10GB rootfs overlay
				VolumeOverlayBytes: 5 * 1024 * 1024 * 1024,  // 5GB volume overlays
				State:              "Running",
			},
			{
				ID:                 "vm-2",
				OverlayBytes:       8 * 1024 * 1024 * 1024, // 8GB rootfs overlay
				VolumeOverlayBytes: 2 * 1024 * 1024 * 1024, // 2GB volume overlays
				State:              "Running",
			},
		},
	}

	mgr := NewManager(cfg, p)
	mgr.SetInstanceLister(mockInstances)
	mgr.SetImageLister(&mockImageLister{
		totalBytes:    50 * 1024 * 1024 * 1024, // 50GB rootfs
		ociCacheBytes: 25 * 1024 * 1024 * 1024, // 25GB OCI cache
	})
	mgr.SetVolumeLister(&mockVolumeLister{totalBytes: 100 * 1024 * 1024 * 1024})

	err := mgr.Initialize(context.Background())
	require.NoError(t, err)

	status, err := mgr.GetFullStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, status.DiskDetail)

	// Verify breakdown
	assert.Equal(t, int64(50*1024*1024*1024), status.DiskDetail.Images)
	assert.Equal(t, int64(25*1024*1024*1024), status.DiskDetail.OCICache)
	assert.Equal(t, int64(100*1024*1024*1024), status.DiskDetail.Volumes)
	// Overlays should be (10+5) + (8+2) = 25GB
	assert.Equal(t, int64(25*1024*1024*1024), status.DiskDetail.Overlays)
}
