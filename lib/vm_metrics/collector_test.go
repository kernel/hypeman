//go:build linux

package vm_metrics

import (
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

func TestReadProcStatm_PageSize(t *testing.T) {
	// Verify that the page size is being used correctly
	// The returned values should be multiples of the page size
	pid := os.Getpid()
	rssBytes, vmsBytes, err := ReadProcStatm(pid)
	require.NoError(t, err)

	pageSize := uint64(os.Getpagesize())
	// RSS and VMS should be exact multiples of page size
	assert.Equal(t, uint64(0), rssBytes%pageSize, "RSS should be a multiple of page size")
	assert.Equal(t, uint64(0), vmsBytes%pageSize, "VMS should be a multiple of page size")
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
