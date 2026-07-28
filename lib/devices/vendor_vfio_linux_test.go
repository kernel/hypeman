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

const testCreatableTypes = `NVIDIA L40S-1Q           1147
NVIDIA L40S-2Q           1148
NVIDIA L40S-48Q          1159
`

func TestParseCreatableVGPUTypes(t *testing.T) {
	t.Parallel()

	profiles, err := parseCreatableVGPUTypes(testCreatableTypes)
	require.NoError(t, err)
	require.Len(t, profiles, 3)
	assert.Equal(t, profileMetadata{TypeName: "1147", Name: "NVIDIA L40S-1Q", FramebufferMB: 1024}, profiles[0])
	assert.Equal(t, profileMetadata{TypeName: "1159", Name: "NVIDIA L40S-48Q", FramebufferMB: 48 * 1024}, profiles[2])
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

	require.NoError(t, sysfs.destroy(context.Background(), device.VFAddress))
	assertFileValue(t, filepath.Join(device.SysfsPath, "nvidia", "current_vgpu_type"), "0")
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

func TestVendorVFIOReconcile(t *testing.T) {
	t.Parallel()

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "")
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "43", "1148", "")

	activeDevice := filepath.Join(sysfs.vfioDevicesPath, "vfio43")
	fdDir := filepath.Join(sysfs.procPath, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0755))
	require.NoError(t, os.Symlink(activeDevice, filepath.Join(fdDir, "5")))

	require.NoError(t, sysfs.reconcile(context.Background()))
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:82:00.4", "nvidia", "current_vgpu_type"), "0")
	assertFileValue(t, filepath.Join(sysfs.pciDevicesPath, "0000:e3:00.4", "nvidia", "current_vgpu_type"), "1148")
}

func TestParseCreatableVGPUTypesRejectsMalformedLine(t *testing.T) {
	t.Parallel()

	_, err := parseCreatableVGPUTypes("NVIDIA L40S-1Q not-an-id")
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
}

func assertFileValue(t *testing.T, path, expected string) {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, expected, string(value))
}
