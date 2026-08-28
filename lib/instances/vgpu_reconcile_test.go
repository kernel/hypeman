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

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileVGPUsBoundsStartupProtection(t *testing.T) {
	dead := exec.Command("true")
	require.NoError(t, dead.Run())
	deadPID := dead.Process.Pid
	now := time.Now().UTC()
	recent := now.Add(-time.Minute)
	stale := now.Add(-devices.VGPUAssignmentGracePeriod - time.Minute)

	var protected map[string]struct{}
	var destroyed []devices.VGPUAssignment
	m := &manager{
		paths: paths.New(t.TempDir()),
		now:   func() time.Time { return now },
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			destroyed = append(destroyed, assignment)
			return nil
		},
		reconcileVGPUDevices: func(_ context.Context, p map[string]struct{}, sweepDevices bool) error {
			protected = p
			assert.True(t, sweepDevices)
			return nil
		},
	}
	instances := []StoredMetadata{
		{Id: "booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4", GPUAssignedAt: &recent},
		{Id: "orphaned", GPUProfile: "NVIDIA L40S-2Q", GPUFramework: devices.VGPUFrameworkVendorVFIO, GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.5", GPUAssignedAt: &stale},
		{Id: "legacy", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.6"},
		{Id: "dead", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.7", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &deadPID}},
		{Id: "stale-pid-booting", GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.8", HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &deadPID}, GPUAssignedAt: &recent},
		{Id: "legacy-mdev-booting", GPUMdevUUID: "test-mdev", GPUAssignedAt: &recent},
	}
	for i := range instances {
		require.NoError(t, m.ensureDirectories(instances[i].Id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: instances[i]}))
	}

	m.ReconcileVGPUs(t.Context())
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.4")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.5")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.6")
	assert.NotContains(t, protected, "/sys/bus/pci/devices/0000:82:00.7")
	assert.Contains(t, protected, "/sys/bus/pci/devices/0000:82:00.8")
	assert.Contains(t, protected, "/sys/bus/mdev/devices/test-mdev")

	for _, id := range []string{"orphaned", "legacy", "dead"} {
		stored, err := m.loadMetadata(id)
		require.NoError(t, err)
		assert.Empty(t, stored.GPUDevicePath, "stale assignment on %s must be released", id)
	}
	assert.Contains(t, destroyed, devices.VGPUAssignment{
		Framework:  devices.VGPUFrameworkVendorVFIO,
		DevicePath: "/sys/bus/pci/devices/0000:82:00.5",
		InstanceID: "orphaned",
	})
	orphaned, err := m.loadMetadata("orphaned")
	require.NoError(t, err)
	assert.Equal(t, "NVIDIA L40S-2Q", orphaned.GPUProfile)
	for _, id := range []string{"booting", "stale-pid-booting", "legacy-mdev-booting"} {
		stored, err := m.loadMetadata(id)
		require.NoError(t, err)
		assert.NotEmpty(t, storedVGPUDevicePath(&stored.StoredMetadata), "live assignment on %s must be kept", id)
	}
}

func TestReconcileVGPUsSkipsDeviceSweepWhenListingFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	var sweeps []bool
	m := &manager{
		paths: paths.New(t.TempDir()),
		reconcileVGPUDevices: func(_ context.Context, _ map[string]struct{}, sweepDevices bool) error {
			sweeps = append(sweeps, sweepDevices)
			return nil
		},
	}
	const id = "unreadable"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: id}}))
	instanceDir := filepath.Dir(m.paths.InstanceMetadata(id))
	require.NoError(t, os.Chmod(instanceDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })

	m.ReconcileVGPUs(t.Context())
	require.Equal(t, []bool{false}, sweeps,
		"a listing failure must skip the device sweep, not run it with an empty protection set")

	require.NoError(t, os.Chmod(instanceDir, 0o755))
	m.ReconcileVGPUs(t.Context())
	assert.Equal(t, []bool{false, true}, sweeps, "the next pass retries the device sweep")
}

func TestReconcileVGPUsKeepsAssignmentWhenReleaseFails(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			if attempts.Add(1) == 1 {
				return errors.New("vGPU destroy failed: 0xffffffff")
			}
			return nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}, bool) error { return nil },
	}
	const id = "wedged"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            id,
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}}))

	m.ReconcileVGPUs(t.Context())
	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath,
		"a failed release must keep the assignment metadata for the next pass")

	m.ReconcileVGPUs(t.Context())
	stored, err = m.loadMetadata(id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath, "the next pass retries the release")
	assert.Equal(t, int32(2), attempts.Load())
}

func TestReconcileVGPUsDefersToUnconfirmedClaimant(t *testing.T) {
	t.Parallel()

	var destroys atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			destroys.Add(1)
			return nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}, bool) error { return nil },
	}
	const path = "/sys/bus/pci/devices/0000:82:00.4"
	assignedAt := time.Now()
	for _, stored := range []StoredMetadata{
		{Id: "stale", GPUFramework: devices.VGPUFrameworkVendorVFIO, GPUDevicePath: path},
		{Id: "mid-boot-claimant", GPUFramework: devices.VGPUFrameworkVendorVFIO, GPUDevicePath: path, GPUAssignedAt: &assignedAt},
	} {
		require.NoError(t, m.ensureDirectories(stored.Id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))
	}

	m.ReconcileVGPUs(t.Context())

	assert.Zero(t, destroys.Load(), "no destroy may fire while the claim scan cannot clear the path")
	stale, err := m.loadMetadata("stale")
	require.NoError(t, err)
	assert.Equal(t, path, stale.GPUDevicePath, "the stale release retries once the claimant's liveness is decidable")
}

func TestReconcileVGPUsReleasesRetentionStubAssignment(t *testing.T) {
	t.Parallel()

	m := &manager{
		paths:                paths.New(t.TempDir()),
		destroyVGPU:          func(context.Context, devices.VGPUAssignment) error { return nil },
		reconcileVGPUDevices: func(context.Context, map[string]struct{}, bool) error { return nil },
	}
	const id = "retention-stub"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:                    id,
		Name:                  "failed-create",
		GPUFramework:          devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath:         "/sys/bus/pci/devices/0000:82:00.4",
		GPURetainedForCleanup: true,
	}}))

	m.ReconcileVGPUs(t.Context())

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath, "the stub's wedged assignment is released once free")
	assert.True(t, stored.GPURetainedForCleanup, "the stub stays a delete-only record of the failed create")
}

func TestStartVGPUReconcilerSkipsHostsWithoutGPUs(t *testing.T) {
	t.Parallel()

	var passes atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkNone, nil, nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}, bool) error {
			passes.Add(1)
			return nil
		},
		vgpuReconcileInterval: time.Millisecond,
	}

	m.StartVGPUReconciler(t.Context())
	time.Sleep(20 * time.Millisecond)
	assert.Zero(t, passes.Load(), "a host without GPUs must not reconcile at all")
}

func TestStartVGPUReconcilerRunsPeriodically(t *testing.T) {
	t.Parallel()

	var passes atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, nil, nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}, bool) error {
			if passes.Add(1) == 1 {
				return errors.New("transient device error")
			}
			return nil
		},
		vgpuReconcileInterval: time.Millisecond,
	}

	m.StartVGPUReconciler(t.Context())
	require.Eventually(t, func() bool { return passes.Load() >= 3 }, 5*time.Second, time.Millisecond,
		"periodic passes must keep running after a failed pass")
}
