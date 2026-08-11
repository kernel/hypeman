//go:build linux

package devices

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testCreatableTypes = `ID    : vGPU Name
1147  : NVIDIA L40S-1Q
1148  : NVIDIA L40S-2Q
1159  : NVIDIA L40S-48Q
`

func TestParseCreatableVGPUTypes(t *testing.T) {
	t.Parallel()

	profiles, err := parseCreatableVGPUTypes(testCreatableTypes)
	require.NoError(t, err)
	require.Len(t, profiles, 3)
	assert.Equal(t, profileMetadata{TypeName: "1147", Name: "NVIDIA L40S-1Q", FramebufferMB: 1024}, profiles[0])
	assert.Equal(t, profileMetadata{TypeName: "1159", Name: "NVIDIA L40S-48Q", FramebufferMB: 48 * 1024}, profiles[2])
}

func TestVendorVFIODiscoverSkipsUnreadableVF(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", testCreatableTypes)
	// Replace one VF's current_vgpu_type with a directory so reading it fails.
	badType := filepath.Join(sysfs.pciDevicesPath, "0000:82:00.5", "nvidia", "current_vgpu_type")
	require.NoError(t, os.Remove(badType))
	require.NoError(t, os.Mkdir(badType, 0755))

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	require.Len(t, vfs, 1)
	assert.Equal(t, "0000:82:00.4", vfs[0].PCIAddress)
}

func TestVendorVFIODiscoverFailsWhenNoVFIsReadable(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	badType := filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "current_vgpu_type")
	require.NoError(t, os.Remove(badType))
	require.NoError(t, os.Mkdir(badType, 0755))

	// With every VF unreadable, discovery must fail rather than report an
	// empty inventory that would demote the host to passthrough.
	_, err := sysfs.discoverVFs()
	require.Error(t, err)
}

func TestVendorVFIOListProfilesSkipsUnreadableVF(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", testCreatableTypes)
	badCreatable := filepath.Join(sysfs.pciDevicesPath, "0000:82:00.5", "nvidia", "creatable_vgpu_types")
	require.NoError(t, os.Remove(badCreatable))
	require.NoError(t, os.Mkdir(badCreatable, 0755))

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)

	profiles, err := sysfs.listProfiles(vfs)
	require.NoError(t, err)
	require.NotEmpty(t, profiles, "readable VFs must keep advertising capacity")
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-1Q"),
		"only the readable VF counts toward availability")
}

func TestVendorVFIOCreateSkipsUnreadableVF(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", testCreatableTypes)
	badCreatable := filepath.Join(sysfs.pciDevicesPath, "0000:82:00.5", "nvidia", "creatable_vgpu_types")
	require.NoError(t, os.Remove(badCreatable))
	require.NoError(t, os.Mkdir(badCreatable, 0755))

	// Create places on the readable VF instead of failing the placement.
	device, err := sysfs.create(context.Background(), "NVIDIA L40S-1Q", "inst-1")
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", device.VFAddress)
}

func TestVendorVFIOCreateAndDestroy(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	require.Len(t, vfs, 1)
	assert.False(t, vfs[0].Allocated)
	assert.Equal(t, "0000:82:00.0", vfs[0].ParentGPU)

	profiles, err := sysfs.listProfiles(vfs)
	require.NoError(t, err)
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-2Q"))

	device, err := sysfs.create(context.Background(), "NVIDIA L40S-2Q", "instance-1")
	require.NoError(t, err)
	assert.Equal(t, VGPUFrameworkVendorVFIO, device.Framework)
	assert.Equal(t, "0000:82:00.4", device.VFAddress)
	assert.Equal(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4"), device.SysfsPath)
	assertFileValue(t, filepath.Join(device.SysfsPath, "nvidia", "current_vgpu_type"), "1148")

	require.NoError(t, sysfs.destroy(context.Background(), device.VFAddress, "instance-1"))
	assertFileValue(t, filepath.Join(device.SysfsPath, "nvidia", "current_vgpu_type"), "0")
}

func TestVendorVFIODestroySkipsAssignmentOwnedByAnotherInstance(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)

	device, err := sysfs.create(context.Background(), "NVIDIA L40S-2Q", "instance-1")
	require.NoError(t, err)

	require.NoError(t, sysfs.destroy(context.Background(), device.VFAddress, "stale-instance"))
	assertFileValue(t, filepath.Join(device.SysfsPath, "nvidia", "current_vgpu_type"), "1148")

	require.NoError(t, sysfs.destroy(context.Background(), device.VFAddress, "instance-1"))
	assertFileValue(t, filepath.Join(device.SysfsPath, "nvidia", "current_vgpu_type"), "0")
}

func TestVendorVFIODestroyRejectsMissingInstanceIDForOwnedVF(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)

	device, err := sysfs.create(context.Background(), "NVIDIA L40S-2Q", "instance-1")
	require.NoError(t, err)

	err = sysfs.destroy(context.Background(), device.VFAddress, "")
	require.ErrorContains(t, err, "without instance ID")
	assertFileValue(t, filepath.Join(device.SysfsPath, "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIODestroyRetainsAssignmentInUse(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", "")

	activeDevice := filepath.Join(sysfs.vfioDevicesPath, "vfio42")
	fdDir := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0755))
	require.NoError(t, os.Symlink(activeDevice, filepath.Join(fdDir, "5")))

	err := sysfs.destroy(context.Background(), vfAddress, "instance-1")
	require.Error(t, err)
	assert.ErrorContains(t, err, "still in use")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIODestroyReleasesUnboundVF(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		unbind func(t *testing.T, sysfs testVendorVFIOSysfs, vfAddress string)
	}{
		{
			name: "missing vfio device directory",
			unbind: func(t *testing.T, sysfs testVendorVFIOSysfs, vfAddress string) {
				require.NoError(t, os.RemoveAll(filepath.Join(sysfs.pciDevicesPath, vfAddress, "vfio-dev")))
			},
		},
		{
			name: "missing iommu group symlink",
			unbind: func(t *testing.T, sysfs testVendorVFIOSysfs, vfAddress string) {
				require.NoError(t, os.Remove(filepath.Join(sysfs.pciDevicesPath, vfAddress, "iommu_group")))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysfs := newTestVendorVFIOSysfs(t)
			const vfAddress = "0000:82:00.4"
			sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", "")
			tt.unbind(t, sysfs, vfAddress)

			require.NoError(t, sysfs.destroy(context.Background(), vfAddress, "instance-1"))
			assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "0")
		})
	}
}

func TestVendorVFIODestroyRetainsAssignmentWhenOneVFIOPathIsMissing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		remove func(t *testing.T, sysfs testVendorVFIOSysfs, vfAddress string)
		open   func(sysfs testVendorVFIOSysfs) string
	}{
		{
			name: "missing iommu group",
			remove: func(t *testing.T, sysfs testVendorVFIOSysfs, vfAddress string) {
				require.NoError(t, os.Remove(filepath.Join(sysfs.pciDevicesPath, vfAddress, "iommu_group")))
			},
			open: func(sysfs testVendorVFIOSysfs) string {
				return filepath.Join(sysfs.vfioDevicesPath, "vfio42")
			},
		},
		{
			name: "missing vfio device directory",
			remove: func(t *testing.T, sysfs testVendorVFIOSysfs, vfAddress string) {
				require.NoError(t, os.RemoveAll(filepath.Join(sysfs.pciDevicesPath, vfAddress, "vfio-dev")))
			},
			open: func(sysfs testVendorVFIOSysfs) string {
				return filepath.Join(filepath.Dir(sysfs.vfioDevicesPath), "42")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sysfs := newTestVendorVFIOSysfs(t)
			const vfAddress = "0000:82:00.4"
			sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", "")
			tt.remove(t, sysfs, vfAddress)

			fdDir := filepath.Join(sysfs.procPath, "123", "fd")
			require.NoError(t, os.MkdirAll(fdDir, 0755))
			require.NoError(t, os.Symlink(tt.open(sysfs), filepath.Join(fdDir, "5")))

			err := sysfs.destroy(context.Background(), vfAddress, "instance-1")
			require.ErrorContains(t, err, "still in use")
			assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
		})
	}
}

func TestVendorVFIOListProfilesCountsFreeVFs(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "44", "0", testCreatableTypes)

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	profiles, err := sysfs.listProfiles(vfs)
	require.NoError(t, err)
	assert.Equal(t, 3, profileAvailability(profiles, "NVIDIA L40S-48Q"),
		"each free VF advertising the type counts as one creatable instance")
}

func TestVendorVFIOCreateReportsCapacityWhenAllGPUsFull(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1159", "")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", "ID    : vGPU Name\n")
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "44", "1159", "")

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	profiles, err := sysfs.listProfiles(vfs)
	require.NoError(t, err)
	assert.Empty(t, profiles)

	_, err = sysfs.create(context.Background(), "NVIDIA L40S-2Q", "instance-1")
	require.Error(t, err)
	assert.ErrorContains(t, err, "GPUs may be at capacity")
}

func TestVendorVFIOCreateReportsAmbiguousMissingProfile(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", "ID    : vGPU Name\n1148  : NVIDIA L40S-2Q\n")

	_, err := sysfs.create(context.Background(), "NVIDIA L40S-48Q", "instance-1")
	require.Error(t, err)
	assert.ErrorContains(t, err, "not creatable on any VF")
	assert.ErrorContains(t, err, "unknown profile or insufficient capacity")
}

func TestVendorVFIOSelectsLeastLoadedGPU(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "44", "0", testCreatableTypes)

	device, err := sysfs.create(context.Background(), "NVIDIA L40S-1Q", "instance-1")
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress)
}

func TestVendorVFIOSelectsLeastLoadedGPUWithConsumedType(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1159", "")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "44", "0", testCreatableTypes)

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	_, err = sysfs.profileMetadata(vfs)
	require.NoError(t, err)
	for _, vfAddress := range []string{"0000:82:00.5", "0000:e3:00.4"} {
		creatableTypesPath := filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "creatable_vgpu_types")
		require.NoError(t, os.Chmod(creatableTypesPath, 0644))
		require.NoError(t, os.WriteFile(creatableTypesPath, []byte("1147  : NVIDIA L40S-1Q\n"), 0444))
	}

	device, err := sysfs.create(context.Background(), "NVIDIA L40S-1Q", "instance-1")
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress)
}

func TestVendorVFIOPlacementPrefersKnownLoadWhenAllocatedTypeIsUnknown(t *testing.T) {
	t.Parallel()

	// Simulates a restart: type 1159 is allocated but no longer creatable
	// anywhere, so its framebuffer size is unknown.
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1159", "")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", "1147  : NVIDIA L40S-1Q\n")
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "44", "0", "1147  : NVIDIA L40S-1Q\n")

	device, err := sysfs.create(context.Background(), "NVIDIA L40S-1Q", "instance-1")
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress, "the GPU with unknown load should be picked last")
}

func TestVendorVFIOPlacesOnGPUWithUnknownLoadWhenItHasTheOnlyCapacity(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1159", "")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", "1147  : NVIDIA L40S-1Q\n")

	device, err := sysfs.create(context.Background(), "NVIDIA L40S-1Q", "instance-1")
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
}

func TestVendorVFIOReconcile(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "")
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "43", "1148", "")

	activeDevice := filepath.Join(sysfs.vfioDevicesPath, "vfio43")
	fdDir := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0755))
	require.NoError(t, os.Symlink(activeDevice, filepath.Join(fdDir, "5")))

	require.NoError(t, sysfs.reconcile(context.Background(), nil))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "current_vgpu_type"), "0")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:e3:00.4", "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIOReconcileSkipsProtectedVF(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "")
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "43", "1148", "")

	protected := map[string]struct{}{
		filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4"): {},
	}
	require.NoError(t, sysfs.reconcile(context.Background(), protected))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "current_vgpu_type"), "1148")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:e3:00.4", "nvidia", "current_vgpu_type"), "0")
}

func TestVendorVFIOReconcilePreservesLegacyGroupFD(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "")
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "43", "1148", "")

	legacyGroup := filepath.Join(filepath.Dir(sysfs.vfioDevicesPath), "43")
	fdDir := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0755))
	require.NoError(t, os.Symlink(legacyGroup, filepath.Join(fdDir, "5")))

	require.NoError(t, sysfs.reconcile(context.Background(), nil))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "current_vgpu_type"), "0")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:e3:00.4", "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIOReconcilePreservesVFWhenVFIODeviceProbeFails(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", "")
	vfioDevPath := filepath.Join(sysfs.pciDevicesPath, vfAddress, "vfio-dev")
	require.NoError(t, os.RemoveAll(vfioDevPath))
	require.NoError(t, os.WriteFile(vfioDevPath, nil, 0644))

	require.NoError(t, sysfs.reconcile(context.Background(), nil))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIOReconcilePreservesVFWhenIOMMUGroupProbeFails(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", "")
	iommuGroupPath := filepath.Join(sysfs.pciDevicesPath, vfAddress, "iommu_group")
	require.NoError(t, os.Remove(iommuGroupPath))
	require.NoError(t, os.WriteFile(iommuGroupPath, nil, 0644))

	require.NoError(t, sysfs.reconcile(context.Background(), nil))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIOReconcilePreservesVFWhenProcFDDirectoryScanFails(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", "")

	processPath := filepath.Join(sysfs.procPath, "123")
	require.NoError(t, os.MkdirAll(processPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(processPath, "fd"), nil, 0644))

	err := sysfs.reconcile(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "read process 123 file descriptors")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIOReconcilePreservesVFWhenProcFDLinkScanFails(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", "")

	fdPath := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(fdPath, "5"), nil, 0644))

	err := sysfs.reconcile(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "read process 123 file descriptor 5")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
}

func TestParseCreatableVGPUTypesHeaderOnly(t *testing.T) {
	t.Parallel()

	profiles, err := parseCreatableVGPUTypes("ID    : vGPU Name\n")
	require.NoError(t, err)
	assert.Empty(t, profiles)
}

func TestParseCreatableVGPUTypesRejectsMalformedLine(t *testing.T) {
	t.Parallel()

	_, err := parseCreatableVGPUTypes("NVIDIA")
	require.Error(t, err)

	_, err = parseCreatableVGPUTypes("not-an-id : NVIDIA L40S-1Q")
	require.Error(t, err)
}

func TestRollbackVendorVFIOCreate(t *testing.T) {
	t.Parallel()

	verifyErr := errors.New("verification failed")
	device := VGPUDevice{
		Framework: VGPUFrameworkVendorVFIO,
		VFAddress: "0000:82:00.4",
		SysfsPath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	t.Run("preserves verification error", func(t *testing.T) {
		currentTypePath := filepath.Join(t.TempDir(), "current_vgpu_type")
		require.NoError(t, os.WriteFile(currentTypePath, []byte("1148"), 0644))
		sysfs := vendorVFIOSysfs{owners: make(map[string]string)}

		err := sysfs.rollbackCreate(currentTypePath, device.VFAddress, "instance-1", device, verifyErr)
		require.ErrorIs(t, err, verifyErr)
		assertFileValue(t, currentTypePath, "0")
		assert.Empty(t, sysfs.owners)
	})

	t.Run("retains assignment when rollback fails", func(t *testing.T) {
		currentTypePath := filepath.Join(t.TempDir(), "missing", "current_vgpu_type")
		sysfs := vendorVFIOSysfs{owners: make(map[string]string)}

		err := sysfs.rollbackCreate(currentTypePath, device.VFAddress, "instance-1", device, verifyErr)
		require.ErrorIs(t, err, verifyErr)
		assert.ErrorContains(t, err, "roll back vGPU on VF 0000:82:00.4")
		var pending *VGPUCreateCleanupPendingError
		require.ErrorAs(t, err, &pending)
		assert.Equal(t, device, pending.Device)
		assert.Equal(t, "instance-1", sysfs.owners[device.VFAddress])
	})
}

func profileAvailability(profiles []GPUProfile, name string) int {
	for _, profile := range profiles {
		if profile.Name == name {
			return profile.Available
		}
	}
	return -1
}

type testVendorVFIOSysfs struct {
	vendorVFIOSysfs
}

func newTestVendorVFIOSysfs(t *testing.T) testVendorVFIOSysfs {
	t.Helper()
	root := t.TempDir()
	pci := filepath.Join(root, "sys", "bus", "pci", "devices")
	proc := filepath.Join(root, "proc")
	vfio := filepath.Join(root, "dev", "vfio", "devices")
	require.NoError(t, os.MkdirAll(pci, 0755))
	require.NoError(t, os.MkdirAll(proc, 0755))
	require.NoError(t, os.MkdirAll(vfio, 0755))
	return testVendorVFIOSysfs{vendorVFIOSysfs{
		pciDevicesPath:    pci,
		procPath:          proc,
		vfioDevicesPath:   vfio,
		owners:            make(map[string]string),
		framebufferByType: make(map[string]int),
	}}
}

func (s testVendorVFIOSysfs) addVF(t *testing.T, parent, address, vfioID, currentType, creatableTypes string) {
	t.Helper()
	parentPath := filepath.Join(s.pciDevicesPath, parent)
	vfPath := filepath.Join(s.pciDevicesPath, address)
	nvidiaPath := filepath.Join(vfPath, "nvidia")
	require.NoError(t, os.MkdirAll(parentPath, 0755))
	require.NoError(t, os.MkdirAll(nvidiaPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(nvidiaPath, "current_vgpu_type"), []byte(currentType), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(nvidiaPath, "creatable_vgpu_types"), []byte(creatableTypes), 0444))
	require.NoError(t, os.Symlink(parentPath, filepath.Join(vfPath, "physfn")))
	vfioName := "vfio" + vfioID
	require.NoError(t, os.MkdirAll(filepath.Join(vfPath, "vfio-dev", vfioName), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(s.vfioDevicesPath, vfioName), nil, 0600))
	require.NoError(t, os.Symlink(filepath.Join("..", "..", "..", "kernel", "iommu_groups", vfioID), filepath.Join(vfPath, "iommu_group")))
}

func assertFileValue(t *testing.T, path, expected string) {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, expected, string(value))
}
