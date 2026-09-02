//go:build linux

package devices

import (
	"context"
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
	badType := filepath.Join(sysfs.pciDevicesPath, "0000:82:00.5", "nvidia", "current_vgpu_type")
	require.NoError(t, os.Remove(badType))
	require.NoError(t, os.Mkdir(badType, 0755))

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	require.Len(t, vfs, 1)
	assert.Equal(t, "0000:82:00.4", vfs[0].PCIAddress)
}

func TestVendorVFIOListProfilesCountsFreeVFs(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "1148", testCreatableTypes)

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	profiles, err := sysfs.listProfiles(vfs)
	require.NoError(t, err)
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-2Q"))
}

func TestVendorVFIOConfigure(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "0", testCreatableTypes)

	require.NoError(t, sysfs.configure(context.Background(), vfAddress, "1148"))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
	require.NoError(t, sysfs.configure(context.Background(), vfAddress, "1148"), "configuration must be idempotent")
}

func TestVendorVFIOConfigureRepairsDirtyVF(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1159", testCreatableTypes)

	require.NoError(t, sysfs.configure(context.Background(), vfAddress, "1148"))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIOConfigurePreservesDirtyVFInUse(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1159", testCreatableTypes)
	activeDevice := filepath.Join(sysfs.vfioDevicesPath, "vfio42")
	fdDir := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0755))
	require.NoError(t, os.Symlink(activeDevice, filepath.Join(fdDir, "5")))

	err := sysfs.configure(context.Background(), vfAddress, "1148")
	require.ErrorContains(t, err, "still in use")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1159")
}

func TestVendorVFIODestroy(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", testCreatableTypes)

	require.NoError(t, sysfs.destroy(context.Background(), vfAddress, "instance-1"))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "0")
}

func TestVendorVFIODestroyRetainsAssignmentInUse(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	const vfAddress = "0000:82:00.4"
	sysfs.addVF(t, "0000:82:00.0", vfAddress, "42", "1148", testCreatableTypes)
	activeDevice := filepath.Join(sysfs.vfioDevicesPath, "vfio42")
	fdDir := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0755))
	require.NoError(t, os.Symlink(activeDevice, filepath.Join(fdDir, "5")))

	err := sysfs.destroy(context.Background(), vfAddress, "instance-1")
	require.ErrorContains(t, err, "still in use")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, vfAddress, "nvidia", "current_vgpu_type"), "1148")
}

func TestVendorVFIOOpenPathsFailsClosed(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	fdPath := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdPath, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(fdPath, "5"), nil, 0644))

	_, err := sysfs.openVFIOPaths()
	require.ErrorContains(t, err, "read process 123 file descriptor 5")
}

func TestParseCreatableVGPUTypesRejectsMalformedLine(t *testing.T) {
	_, err := parseCreatableVGPUTypes("NVIDIA")
	require.Error(t, err)
	_, err = parseCreatableVGPUTypes("not-an-id : NVIDIA L40S-1Q")
	require.Error(t, err)
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
		pciDevicesPath:  pci,
		procPath:        proc,
		vfioDevicesPath: vfio,
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
