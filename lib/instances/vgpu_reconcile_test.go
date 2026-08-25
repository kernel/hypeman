package instances

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveVGPUReconcileProtectionBoundsStartupProtection(t *testing.T) {
	dead := exec.Command("true")
	require.NoError(t, dead.Run())
	deadPID := dead.Process.Pid
	now := time.Now().UTC()
	recent := now.Add(-time.Minute)
	stale := now.Add(-VGPUAssignmentStartupGracePeriod - time.Minute)

	m := &manager{paths: paths.New(t.TempDir()), now: func() time.Time { return now }}
	instances := []StoredMetadata{
		{Id: "booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4", GPUAssignedAt: &recent},
		{Id: "orphaned", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.5", GPUAssignedAt: &stale},
		{Id: "legacy", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.6"},
		{Id: "dead", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.7", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &deadPID}},
		{Id: "stale-pid-booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.8", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &deadPID}, GPUAssignedAt: &recent},
	}
	for i := range instances {
		require.NoError(t, m.ensureDirectories(instances[i].Id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: instances[i]}))
	}

	protected, retryAfter, err := m.liveVGPUReconcileProtection(t.Context())
	require.NoError(t, err)
	assert.Equal(t, VGPUAssignmentStartupGracePeriod-time.Minute, retryAfter)
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.4")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.5")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.6")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.7")
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.8")
}

func TestReconcileVGPUsRetriesAfterListingFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	m := &manager{paths: paths.New(t.TempDir()), vgpuReconcileRetryDelay: 250 * time.Millisecond}
	const id = "unreadable"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: id}}))
	instanceDir := filepath.Dir(m.paths.InstanceMetadata(id))
	require.NoError(t, os.Chmod(instanceDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ReconcileVGPUs(ctx)
	require.True(t, m.vgpuReconcileRetryPending.Load(),
		"a listing failure must schedule a retry instead of disabling the vendor sweep until restart")

	require.NoError(t, os.Chmod(instanceDir, 0o755))
	require.Eventually(t, func() bool {
		return !m.vgpuReconcileRetryPending.Load()
	}, 5*time.Second, 10*time.Millisecond)
}

func TestReconcileVGPUsRetriesAfterDeviceFailure(t *testing.T) {
	var calls atomic.Int32
	m := &manager{
		paths:                   paths.New(t.TempDir()),
		vgpuReconcileRetryDelay: 10 * time.Millisecond,
		reconcileVGPUDevices: func(context.Context, map[string]struct{}, bool) error {
			if calls.Add(1) == 1 {
				return errors.New("transient device error")
			}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.ReconcileVGPUs(ctx)
	require.True(t, m.vgpuReconcileRetryPending.Load())
	require.Eventually(t, func() bool {
		return calls.Load() >= 2 && !m.vgpuReconcileRetryPending.Load()
	}, 5*time.Second, 10*time.Millisecond)
}
