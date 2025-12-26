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
	totalBytes int64
}

func (m *mockImageLister) TotalImageBytes(ctx context.Context) (int64, error) {
	return m.totalBytes, nil
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
