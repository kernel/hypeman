//go:build linux

package instances

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
					TypeName:      testVFProfileType,
					Name:          testVGPUProfile,
					FramebufferMB: 2048,
				}}
			}
			return profiles, nil
		},
	}
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

func TestVGPUClaimSkipsQuarantinedVF(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	m.quarantinedVFs = func() (map[string]struct{}, error) {
		return map[string]struct{}{"0000:82:00.4": {}}, nil
	}
	m.pickVFIndex = pickFirst
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
}

func TestVGPUClaimFailsClosedWhenVFHealthUnavailable(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	m.quarantinedVFs = func() (map[string]struct{}, error) {
		return nil, errors.New("VF health state unavailable: read failed")
	}
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.ErrorContains(t, err, "VF health state unavailable")
	stored, loadErr := m.loadMetadata("new")
	require.NoError(t, loadErr)
	assert.Empty(t, stored.GPUDevicePath, "placement must not claim while quarantine state is unknown")
}

func TestVGPUClaimRecordsClaimTime(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	claimedAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return claimedAt }
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error { return nil }
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	stored, err := m.loadMetadata("new")
	require.NoError(t, err)
	require.NotNil(t, stored.GPUClaimedAt)
	assert.True(t, claimedAt.Equal(*stored.GPUClaimedAt))

	require.NoError(t, m.releaseStoredVGPUPersisted(context.Background(), stored))
	stored, err = m.loadMetadata("new")
	require.NoError(t, err)
	assert.Nil(t, stored.GPUClaimedAt, "release must clear the assignment identity with the claim")
}

func TestVGPUClaimUsesLeastLoadedGPU(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	claimed := saveTestVGPUInstance(t, m, "claimed")
	claimed.GPUFramework = devices.VGPUFrameworkVendorVFIO
	claimed.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(claimed))
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress)
}

func TestVGPUClaimResetsDirtySameTypeVFBeforeClaim(t *testing.T) {
	// The leftover type matches the requested profile, so configure would
	// no-op; the reset must happen at claim time, before the claim exists.
	vfs := []devices.VirtualFunction{{
		PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType,
	}}
	m := newVGPUAllocationManager(t, vfs)
	var resets []string
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		onDisk, err := m.loadMetadata("new")
		require.NoError(t, err)
		assert.Empty(t, onDisk.GPUDevicePath, "reset must run before the claim is persisted")
		resets = append(resets, assignment.DevicePath)
		return nil
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", device.VFAddress)
	assert.Equal(t, []string{testVFDevicePath}, resets)
}

func TestVGPUClaimFailsWhenDirtyVFStillInUse(t *testing.T) {
	vfs := []devices.VirtualFunction{{
		PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType,
	}}
	m := newVGPUAllocationManager(t, vfs)
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		return errors.New("vendor VFIO vGPU on VF 0000:82:00.4 is still in use")
	}
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.ErrorContains(t, err, "repair dirty VF 0000:82:00.4 before claim")
	stored, loadErr := m.loadMetadata("new")
	require.NoError(t, loadErr)
	assert.Empty(t, stored.GPUDevicePath, "a failed repair must not leave a claim")
}

func TestVGPUClaimRepairsDirtyVFWhenProfileNotAdvertised(t *testing.T) {
	// The dirty VF's leftover type consumes the framebuffer that would make
	// the requested profile creatable, so no VF advertises it until the VF
	// is reset.
	dirty := true
	var resets []string
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			vf := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
			if dirty {
				vf.Allocated = true
				vf.ProfileType = "999"
			}
			return devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{vf}, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			if dirty {
				return map[string][]devices.VGPUProfileType{}, nil
			}
			return map[string][]devices.VGPUProfileType{
				"0000:82:00.4": {{TypeName: testVFProfileType, Name: testVGPUProfile, FramebufferMB: 2048}},
			}, nil
		},
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			resets = append(resets, assignment.DevicePath)
			dirty = false
			return nil
		},
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", device.VFAddress)
	assert.Equal(t, []string{testVFDevicePath}, resets)
	stored, err := m.loadMetadata("new")
	require.NoError(t, err)
	assert.Equal(t, testVFDevicePath, stored.GPUDevicePath)
}

func TestVGPUClaimRepairSkipsClaimedVFs(t *testing.T) {
	vfs := []devices.VirtualFunction{{
		PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: "999",
	}}
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			return map[string][]devices.VGPUProfileType{}, nil
		},
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			t.Fatal("must not reset a VF claimed by another instance")
			return nil
		},
	}
	owner := saveTestVGPUInstance(t, m, "owner")
	owner.GPUFramework = devices.VGPUFrameworkVendorVFIO
	owner.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(owner))
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.Error(t, err)
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
	m.reconcileVGPUDevices = func(context.Context, map[string]struct{}) error { return nil }
	m.ReconcileVGPUs(context.Background())

	require.Equal(t, []devices.VGPUAssignment{{
		Framework: devices.VGPUFrameworkVendorVFIO, DevicePath: device.SysfsPath, InstanceID: "mid-create",
	}}, destroyed)
	stored, err := m.loadMetadata("mid-create")
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath)
}

func TestVGPUClaimPrefersCleanVFOnSameGPU(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		t.Fatal("a clean VF must be claimed without resetting a dirty sibling")
		return nil
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
}

func TestVGPUClaimFallsBackWhenDirtyVFRefusesReset(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
	}
	m := newVGPUAllocationManager(t, vfs)
	// Both VFs are equally ranked; pin the tiebreak so the fallback order is fixed.
	m.pickVFIndex = pickFirst
	var resets []string
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		resets = append(resets, assignment.DevicePath)
		if assignment.DevicePath == testVFDevicePath {
			return errors.New("vendor VFIO vGPU on VF 0000:82:00.4 is still in use")
		}
		return nil
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
	assert.Equal(t, []string{testVFDevicePath, "/sys/bus/pci/devices/0000:82:00.5"}, resets)
}
