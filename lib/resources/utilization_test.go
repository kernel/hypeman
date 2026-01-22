package resources

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadProcStat(t *testing.T) {
	// Test with current process - should work
	pid := os.Getpid()
	cpuUsec, err := ReadProcStat(pid)
	require.NoError(t, err)
	assert.True(t, cpuUsec >= 0, "CPU time should be non-negative")
}

func TestReadProcStat_InvalidPID(t *testing.T) {
	_, err := ReadProcStat(999999999)
	assert.Error(t, err)
}

func TestReadProcStatm(t *testing.T) {
	// Test with current process - should work
	pid := os.Getpid()
	rssBytes, vmsBytes, err := ReadProcStatm(pid)
	require.NoError(t, err)
	assert.True(t, rssBytes > 0, "RSS should be positive")
	assert.True(t, vmsBytes > 0, "VMS should be positive")
	assert.True(t, vmsBytes >= rssBytes, "VMS should be >= RSS")
}

func TestReadProcStatm_InvalidPID(t *testing.T) {
	_, _, err := ReadProcStatm(999999999)
	assert.Error(t, err)
}

func TestReadTAPStats(t *testing.T) {
	// This test requires /sys/class/net to exist
	// We'll use loopback which should always exist
	testInterface := "lo"

	basePath := filepath.Join("/sys/class/net", testInterface, "statistics")
	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		t.Skip("skipping test: /sys/class/net not available")
	}

	rxBytes, txBytes, err := ReadTAPStats(testInterface)
	require.NoError(t, err)
	// Loopback should have some traffic (or at least zero is valid)
	assert.True(t, rxBytes >= 0 || txBytes >= 0, "should be able to read stats")
}

func TestReadTAPStats_NotExists(t *testing.T) {
	_, _, err := ReadTAPStats("nonexistent-tap-device")
	assert.Error(t, err)
}

// mockUtilizationSource implements UtilizationSource for testing
type mockUtilizationSource struct {
	instances []InstanceUtilizationInfo
}

func (m *mockUtilizationSource) ListRunningInstancesInfo(ctx context.Context) ([]InstanceUtilizationInfo, error) {
	return m.instances, nil
}

func TestCollectVMUtilization_WithMockSource(t *testing.T) {
	// Create a manager with mock source
	manager := &Manager{}

	// Test with nil source - should return nil, no error
	utils, err := manager.CollectVMUtilization(context.Background())
	require.NoError(t, err)
	assert.Nil(t, utils)

	// Set up mock source with no running instances
	mockSource := &mockUtilizationSource{
		instances: []InstanceUtilizationInfo{},
	}
	manager.SetUtilizationSource(mockSource)

	utils, err = manager.CollectVMUtilization(context.Background())
	require.NoError(t, err)
	assert.Empty(t, utils)

	// Test with instances that have no PID (simulates instances where proc reading will fail gracefully)
	mockSource.instances = []InstanceUtilizationInfo{
		{
			ID:                   "test-instance-1",
			Name:                 "test-vm",
			HypervisorPID:        nil, // No PID - proc reading skipped
			TAPDevice:            "",  // No TAP - network reading skipped
			AllocatedVcpus:       2,
			AllocatedMemoryBytes: 1024 * 1024 * 1024, // 1GB
		},
	}

	utils, err = manager.CollectVMUtilization(context.Background())
	require.NoError(t, err)
	require.Len(t, utils, 1)
	assert.Equal(t, "test-instance-1", utils[0].InstanceID)
	assert.Equal(t, "test-vm", utils[0].InstanceName)
	assert.Equal(t, 2, utils[0].AllocatedVcpus)
	assert.Equal(t, int64(1024*1024*1024), utils[0].AllocatedMemoryBytes)
	// All metrics should be 0 since we couldn't read proc
	assert.Equal(t, uint64(0), utils[0].CPUUsec)
	assert.Equal(t, uint64(0), utils[0].MemoryRSSBytes)
}

func TestCollectVMUtilization_WithCurrentProcess(t *testing.T) {
	// Test with current process PID to verify proc reading works
	manager := &Manager{}
	pid := os.Getpid()

	mockSource := &mockUtilizationSource{
		instances: []InstanceUtilizationInfo{
			{
				ID:                   "test-instance",
				Name:                 "test-vm",
				HypervisorPID:        &pid,
				AllocatedVcpus:       4,
				AllocatedMemoryBytes: 4 * 1024 * 1024 * 1024, // 4GB
			},
		},
	}
	manager.SetUtilizationSource(mockSource)

	utils, err := manager.CollectVMUtilization(context.Background())
	require.NoError(t, err)
	require.Len(t, utils, 1)

	// Should have non-zero values since we're reading from current process
	assert.True(t, utils[0].CPUUsec > 0 || utils[0].MemoryRSSBytes > 0, "should have some metrics")
	assert.True(t, utils[0].MemoryRSSBytes > 0, "RSS should be positive")
	assert.True(t, utils[0].MemoryVMSBytes > 0, "VMS should be positive")
}

func TestVMUtilization_MemoryRatio(t *testing.T) {
	// Test memory utilization ratio calculation
	util := VMUtilization{
		MemoryRSSBytes:       536870912,  // 512MB actual RSS
		AllocatedMemoryBytes: 1073741824, // 1GB allocated
	}

	// Calculate ratio as the metrics code would
	ratio := float64(util.MemoryRSSBytes) / float64(util.AllocatedMemoryBytes)
	assert.InDelta(t, 0.5, ratio, 0.001) // Should be ~50%
}

func TestUtilizationMetrics_Integration(t *testing.T) {
	// Create a manager with mock utilization source
	manager := &Manager{}

	// Create mock source with test data
	mockSource := &mockUtilizationSource{
		instances: []InstanceUtilizationInfo{
			{
				ID:                   "vm-001",
				Name:                 "web-server",
				HypervisorPID:        nil,
				TAPDevice:            "",
				AllocatedVcpus:       4,
				AllocatedMemoryBytes: 4 * 1024 * 1024 * 1024, // 4GB
			},
			{
				ID:                   "vm-002",
				Name:                 "database",
				HypervisorPID:        nil,
				TAPDevice:            "",
				AllocatedVcpus:       8,
				AllocatedMemoryBytes: 16 * 1024 * 1024 * 1024, // 16GB
			},
		},
	}
	manager.SetUtilizationSource(mockSource)

	// Collect utilization
	utils, err := manager.CollectVMUtilization(context.Background())
	require.NoError(t, err)
	require.Len(t, utils, 2)

	// Verify instance data is passed through
	assert.Equal(t, "vm-001", utils[0].InstanceID)
	assert.Equal(t, "web-server", utils[0].InstanceName)
	assert.Equal(t, 4, utils[0].AllocatedVcpus)

	assert.Equal(t, "vm-002", utils[1].InstanceID)
	assert.Equal(t, "database", utils[1].InstanceName)
	assert.Equal(t, 8, utils[1].AllocatedVcpus)
}
