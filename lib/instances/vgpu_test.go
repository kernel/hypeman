package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testVGPUProfile = "NVIDIA L40S-2Q"

func newVGPUAllocationManager(t *testing.T, vfs []devices.VirtualFunction) *manager {
	t.Helper()
	return &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			profiles := make(map[string][]devices.VGPUProfileType, len(vfs))
			for _, vf := range vfs {
				profiles[vf.PCIAddress] = []devices.VGPUProfileType{{
					TypeName:      "1148",
					Name:          testVGPUProfile,
					FramebufferMB: 2048,
				}}
			}
			return profiles, nil
		},
	}
}

func saveTestVGPUInstance(t *testing.T, m *manager, id string) *metadata {
	t.Helper()
	require.NoError(t, m.ensureDirectories(id))
	meta := &metadata{StoredMetadata: StoredMetadata{Id: id, Name: id, GPUProfile: testVGPUProfile}}
	require.NoError(t, m.saveMetadata(meta))
	return meta
}

func TestConcurrentVGPUClaimsUseDistinctVFs(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	saveTestVGPUInstance(t, m, "instance-a")
	saveTestVGPUInstance(t, m, "instance-b")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"instance-a", "instance-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			meta, err := m.loadMetadata(id)
			if err == nil {
				_, err = m.claimVGPU(context.Background(), meta, testVGPUProfile)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	claims := make(map[string]struct{})
	for _, id := range []string{"instance-a", "instance-b"} {
		meta, err := m.loadMetadata(id)
		require.NoError(t, err)
		claims[meta.GPUDevicePath] = struct{}{}
	}
	assert.Len(t, claims, 2)
}

func TestVGPUClaimUsesLeastLoadedGPU(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: "1148"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	claimed := saveTestVGPUInstance(t, m, "claimed")
	claimed.GPUFramework = devices.VGPUFrameworkVendorVFIO
	claimed.GPUDevicePath = devices.GetDeviceSysfsPath("0000:82:00.4")
	require.NoError(t, m.saveMetadata(claimed))
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress)
}

func TestVGPUClaimCanSelectDirtyUnclaimedVF(t *testing.T) {
	vfs := []devices.VirtualFunction{{
		PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: "1148",
	}}
	m := newVGPUAllocationManager(t, vfs)
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", device.VFAddress)
}

func TestVGPUCrashAfterClaimIsReconciled(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	meta := saveTestVGPUInstance(t, m, "mid-create")
	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)

	var destroyed []devices.VGPUAssignment
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		destroyed = append(destroyed, assignment)
		return nil
	}
	m.reconcileVGPUDevices = func(context.Context, map[string]struct{}, bool) error { return nil }
	m.ReconcileVGPUs(context.Background())

	require.Equal(t, []devices.VGPUAssignment{{
		Framework: devices.VGPUFrameworkVendorVFIO, DevicePath: device.SysfsPath, InstanceID: "mid-create",
	}}, destroyed)
	stored, err := m.loadMetadata("mid-create")
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath)
}

func TestReleaseStoredVGPUKeepsClaimWhenResetFails(t *testing.T) {
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			return errors.New("reset failed")
		},
	}
	meta := saveTestVGPUInstance(t, m, "claimed")
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = devices.GetDeviceSysfsPath("0000:82:00.4")
	require.NoError(t, m.saveMetadata(meta))

	err := m.releaseStoredVGPUPersisted(context.Background(), meta)
	require.ErrorContains(t, err, "reset failed")
	stored, loadErr := m.loadMetadata(meta.Id)
	require.NoError(t, loadErr)
	assert.Equal(t, meta.GPUDevicePath, stored.GPUDevicePath)
}

func TestCreateCleanupResetsWhileClaimIsStillPersisted(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	meta := saveTestVGPUInstance(t, m, "failed-create")
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = devices.GetDeviceSysfsPath("0000:82:00.4")
	require.NoError(t, m.saveMetadata(meta))
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		onDisk, err := m.loadMetadata(meta.Id)
		require.NoError(t, err)
		assert.Equal(t, meta.GPUDevicePath, onDisk.GPUDevicePath)
		return errors.New("reset failed")
	}

	assert.True(t, m.cleanupCreateVGPU(context.Background(), &meta.StoredMetadata))
	require.NoError(t, m.deleteInstanceData(meta.Id))
	_, err := m.loadMetadata(meta.Id)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCreateCleanupPreservesClaimForLiveVMM(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	meta := saveTestVGPUInstance(t, m, "live-create")
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = devices.GetDeviceSysfsPath("0000:82:00.4")
	meta.HypervisorProcessIdentity.Set(os.Getpid())
	require.NoError(t, m.saveMetadata(meta))
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		t.Fatal("must not reset a VF owned by a live VMM")
		return nil
	}

	assert.False(t, m.cleanupCreateVGPU(context.Background(), &meta.StoredMetadata))
	stored, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	assert.Equal(t, meta.GPUDevicePath, stored.GPUDevicePath)
}

func TestStartCleanupRemovesClaimAfterResetFailure(t *testing.T) {
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			return errors.New("reset failed")
		},
	}
	meta := saveTestVGPUInstance(t, m, "failed-start")
	rollback := *meta
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = devices.GetDeviceSysfsPath("0000:82:00.4")
	require.NoError(t, m.saveMetadata(meta))

	m.cleanupStartVGPU(context.Background(), &meta.StoredMetadata, rollback)
	stored, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Equal(t, testVGPUProfile, stored.GPUProfile)
}

func TestStoredVGPUDevicePath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", storedVGPUDevicePath(&StoredMetadata{
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		GPUMdevUUID:   "legacy-uuid",
	}))
	assert.Equal(t, "/sys/bus/mdev/devices/legacy-uuid", storedVGPUDevicePath(&StoredMetadata{
		GPUMdevUUID: "legacy-uuid",
	}))
	assert.Empty(t, storedVGPUDevicePath(&StoredMetadata{}))
}

func TestSelectVendorVFIOVFFailsClosedOnClaimedVF(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	profiles := map[string][]devices.VGPUProfileType{
		"0000:82:00.4": {{TypeName: "1148", Name: testVGPUProfile, FramebufferMB: 2048}},
	}
	_, _, err := selectVendorVFIOVF(vfs, profiles, []StoredMetadata{{
		GPUDevicePath: filepath.Join("/sys/bus/pci/devices", "0000:82:00.4"),
	}}, testVGPUProfile)
	require.ErrorContains(t, err, "no available VF")
}
