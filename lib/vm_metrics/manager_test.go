//go:build linux

package vm_metrics

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceSource implements InstanceSource for testing
type mockInstanceSource struct {
	instances []InstanceInfo
}

func (m *mockInstanceSource) ListRunningInstancesForMetrics() ([]InstanceInfo, error) {
	return m.instances, nil
}

func TestManager_GetInstanceStats(t *testing.T) {
	mgr := NewManager()

	// Test with no PID - should return stats with zero values
	info := InstanceInfo{
		ID:                   "test-instance-1",
		Name:                 "test-vm",
		HypervisorPID:        nil,
		TAPDevice:            "",
		AllocatedVcpus:       2,
		AllocatedMemoryBytes: 1024 * 1024 * 1024, // 1GB
	}

	stats := mgr.GetInstanceStats(context.Background(), info)
	require.NotNil(t, stats)
	assert.Equal(t, "test-instance-1", stats.InstanceID)
	assert.Equal(t, "test-vm", stats.InstanceName)
	assert.Equal(t, 2, stats.AllocatedVcpus)
	assert.Equal(t, int64(1024*1024*1024), stats.AllocatedMemoryBytes)
	assert.Equal(t, uint64(0), stats.CPUUsec)
	assert.Equal(t, uint64(0), stats.MemoryRSSBytes)
}

func TestManager_GetInstanceStats_WithCurrentProcess(t *testing.T) {
	mgr := NewManager()
	pid := os.Getpid()

	info := InstanceInfo{
		ID:                   "test-instance",
		Name:                 "test-vm",
		HypervisorPID:        &pid,
		AllocatedVcpus:       4,
		AllocatedMemoryBytes: 4 * 1024 * 1024 * 1024, // 4GB
	}

	stats := mgr.GetInstanceStats(context.Background(), info)
	require.NotNil(t, stats)

	// Should have non-zero values since we're reading from current process
	assert.True(t, stats.CPUUsec >= 0, "CPU time should be non-negative")
	assert.True(t, stats.MemoryRSSBytes > 0, "RSS should be positive")
	assert.True(t, stats.MemoryVMSBytes > 0, "VMS should be positive")
}

func TestManager_CollectAll_NilSource(t *testing.T) {
	mgr := NewManager()

	// Test with nil source - should return nil, no error
	stats, err := mgr.CollectAll(context.Background())
	require.NoError(t, err)
	assert.Nil(t, stats)
}

func TestManager_CollectAll_EmptySource(t *testing.T) {
	mgr := NewManager()
	mgr.SetInstanceSource(&mockInstanceSource{instances: []InstanceInfo{}})

	stats, err := mgr.CollectAll(context.Background())
	require.NoError(t, err)
	assert.Empty(t, stats)
}

func TestManager_CollectAll_MultipleInstances(t *testing.T) {
	mgr := NewManager()
	mgr.SetInstanceSource(&mockInstanceSource{
		instances: []InstanceInfo{
			{
				ID:                   "vm-001",
				Name:                 "web-server",
				HypervisorPID:        nil,
				TAPDevice:            "",
				AllocatedVcpus:       4,
				AllocatedMemoryBytes: 4 * 1024 * 1024 * 1024,
			},
			{
				ID:                   "vm-002",
				Name:                 "database",
				HypervisorPID:        nil,
				TAPDevice:            "",
				AllocatedVcpus:       8,
				AllocatedMemoryBytes: 16 * 1024 * 1024 * 1024,
			},
		},
	})

	stats, err := mgr.CollectAll(context.Background())
	require.NoError(t, err)
	require.Len(t, stats, 2)

	assert.Equal(t, "vm-001", stats[0].InstanceID)
	assert.Equal(t, "web-server", stats[0].InstanceName)
	assert.Equal(t, 4, stats[0].AllocatedVcpus)

	assert.Equal(t, "vm-002", stats[1].InstanceID)
	assert.Equal(t, "database", stats[1].InstanceName)
	assert.Equal(t, 8, stats[1].AllocatedVcpus)
}

func TestVMStats_CPUSeconds(t *testing.T) {
	stats := &VMStats{
		CPUUsec: 1500000, // 1.5 seconds in microseconds
	}
	assert.InDelta(t, 1.5, stats.CPUSeconds(), 0.001)
}

func TestVMStats_MemoryUtilizationRatio(t *testing.T) {
	tests := []struct {
		name         string
		rss          uint64
		allocated    int64
		expectRatio  *float64
		expectNil    bool
	}{
		{
			name:        "normal ratio",
			rss:         536870912,  // 512MB
			allocated:   1073741824, // 1GB
			expectRatio: ptrFloat64(0.5),
		},
		{
			name:        "100% utilization",
			rss:         1073741824, // 1GB
			allocated:   1073741824, // 1GB
			expectRatio: ptrFloat64(1.0),
		},
		{
			name:        "over 100% utilization",
			rss:         2147483648, // 2GB
			allocated:   1073741824, // 1GB
			expectRatio: ptrFloat64(2.0),
		},
		{
			name:      "zero allocated",
			rss:       536870912,
			allocated: 0,
			expectNil: true,
		},
		{
			name:      "negative allocated",
			rss:       536870912,
			allocated: -1,
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := &VMStats{
				MemoryRSSBytes:       tt.rss,
				AllocatedMemoryBytes: tt.allocated,
			}
			ratio := stats.MemoryUtilizationRatio()
			if tt.expectNil {
				assert.Nil(t, ratio)
			} else {
				require.NotNil(t, ratio)
				assert.InDelta(t, *tt.expectRatio, *ratio, 0.001)
			}
		})
	}
}

func TestBuildInstanceInfo(t *testing.T) {
	pid := 1234
	
	// With network enabled
	info := BuildInstanceInfo("abc123", "my-vm", &pid, true, 4, 4*1024*1024*1024)
	assert.Equal(t, "abc123", info.ID)
	assert.Equal(t, "my-vm", info.Name)
	assert.Equal(t, &pid, info.HypervisorPID)
	assert.Equal(t, 4, info.AllocatedVcpus)
	assert.Equal(t, int64(4*1024*1024*1024), info.AllocatedMemoryBytes)
	assert.NotEmpty(t, info.TAPDevice, "should have TAP device when network enabled")
	
	// Without network enabled
	info = BuildInstanceInfo("abc123", "my-vm", &pid, false, 4, 4*1024*1024*1024)
	assert.Empty(t, info.TAPDevice, "should not have TAP device when network disabled")
}

func ptrFloat64(v float64) *float64 {
	return &v
}
