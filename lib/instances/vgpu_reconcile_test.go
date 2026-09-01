package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileVGPUsReleasesOnlyDeadClaims(t *testing.T) {
	var destroyed []devices.VGPUAssignment
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			destroyed = append(destroyed, assignment)
			return nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}) error { return nil },
	}
	liveIdentity := HypervisorProcessIdentity{}
	liveIdentity.Set(os.Getpid())
	instances := []StoredMetadata{
		{Id: "dead-vendor", GPUProfile: testVGPUProfile, GPUFramework: devices.VGPUFrameworkVendorVFIO, GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4"},
		{Id: "dead-mdev", GPUProfile: testVGPUProfile, GPUFramework: devices.VGPUFrameworkMdev, GPUDevicePath: "/sys/bus/mdev/devices/test-mdev", GPUMdevUUID: "test-mdev"},
		{Id: "live", GPUFramework: devices.VGPUFrameworkVendorVFIO, GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.5", HypervisorProcessIdentity: liveIdentity},
	}
	for i := range instances {
		require.NoError(t, m.ensureDirectories(instances[i].Id))
		require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: instances[i]}))
	}

	m.ReconcileVGPUs(t.Context())
	assert.Len(t, destroyed, 2)
	for _, id := range []string{"dead-vendor", "dead-mdev"} {
		stored, err := m.loadMetadata(id)
		require.NoError(t, err)
		assert.Empty(t, storedVGPUDevicePath(&stored.StoredMetadata))
	}
	live, err := m.loadMetadata("live")
	require.NoError(t, err)
	assert.NotEmpty(t, live.GPUDevicePath)
}

func TestReconcileVGPUsKeepsAssignmentWhenReleaseFails(t *testing.T) {
	var attempts atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			if attempts.Add(1) == 1 {
				return errors.New("reset failed")
			}
			return nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}) error { return nil },
	}
	meta := saveTestVGPUInstance(t, m, "wedged")
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	require.NoError(t, m.saveMetadata(meta))

	m.ReconcileVGPUs(t.Context())
	stored, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	assert.NotEmpty(t, stored.GPUDevicePath)

	m.ReconcileVGPUs(t.Context())
	stored, err = m.loadMetadata(meta.Id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Equal(t, int32(2), attempts.Load())
}

func TestReconcileVGPUsSkipsDevicePassWhenListingFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	var passes atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		reconcileVGPUDevices: func(context.Context, map[string]struct{}) error {
			passes.Add(1)
			return nil
		},
	}
	meta := saveTestVGPUInstance(t, m, "unreadable")
	instanceDir := filepath.Dir(m.paths.InstanceMetadata(meta.Id))
	require.NoError(t, os.Chmod(instanceDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })

	m.ReconcileVGPUs(t.Context())
	assert.Zero(t, passes.Load())
}

func TestStartVGPUReconcilerSkipsHostsWithoutGPUs(t *testing.T) {
	var passes atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkNone, nil, nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}) error {
			passes.Add(1)
			return nil
		},
		vgpuReconcileInterval: time.Millisecond,
	}

	m.StartVGPUReconciler(t.Context())
	time.Sleep(20 * time.Millisecond)
	assert.Zero(t, passes.Load())
}

func TestStartVGPUReconcilerRunsPeriodically(t *testing.T) {
	var passes atomic.Int32
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, nil, nil
		},
		reconcileVGPUDevices: func(context.Context, map[string]struct{}) error {
			passes.Add(1)
			return nil
		},
		vgpuReconcileInterval: time.Millisecond,
	}

	m.StartVGPUReconciler(t.Context())
	require.Eventually(t, func() bool { return passes.Load() >= 3 }, 5*time.Second, time.Millisecond)
}
