package instances

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	_ "unsafe"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateVGPUHypervisor(t *testing.T) {
	t.Parallel()

	assert.NoError(t, validateVGPUHypervisor(hypervisor.TypeQEMU))
	assert.EqualError(t, validateVGPUHypervisor(hypervisor.TypeCloudHypervisor), "vGPU is only supported with qemu, got cloud-hypervisor")
}

func TestCleanupFailedCreateRetainsVGPUAssignment(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	stored := &StoredMetadata{
		Id:             "failed-create",
		Name:           "failed-create",
		GPUProfile:     "NVIDIA L40S-2Q",
		GPUFramework:   devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath:  "/sys/bus/pci/devices/0000:82:00.4",
		GPUMdevUUID:    "mdev-uuid",
		NetworkEnabled: true,
		IP:             "192.0.2.1",
		Volumes:        []VolumeAttachment{{VolumeID: "volume"}},
		HypervisorType: "qemu",
		DataDir:        m.paths.InstanceDir("failed-create"),
	}

	m.cleanupFailedCreate(context.Background(), stored.Id, stored)

	retained, err := m.loadMetadata(stored.Id)
	require.NoError(t, err)
	assert.Equal(t, stored.Id, retained.Id)
	assert.Equal(t, stored.GPUFramework, retained.GPUFramework)
	assert.Equal(t, stored.GPUDevicePath, retained.GPUDevicePath)
	assert.Equal(t, stored.GPUMdevUUID, retained.GPUMdevUUID)
	assert.Empty(t, retained.Name)
	assert.Empty(t, retained.GPUProfile)
	assert.False(t, retained.NetworkEnabled)
	assert.Empty(t, retained.IP)
	assert.Empty(t, retained.Volumes)
	assert.Empty(t, retained.DataDir)
}

//go:linkname hostVendorVFIO github.com/kernel/hypeman/lib/devices.hostVendorVFIO
var hostVendorVFIO vendorVFIOSysfs

type vendorVFIOSysfs struct {
	pciDevicesPath  string
	procPath        string
	vfioDevicesPath string
	owners          map[string]string
}

func TestStartRollbackClearsVGPUAssignmentAfterSuccessfulDestroy(t *testing.T) {
	root := t.TempDir()
	pciDevicesPath := filepath.Join(root, "sys", "bus", "pci", "devices")
	vfAddress := "0000:82:00.4"
	nvidiaPath := filepath.Join(pciDevicesPath, vfAddress, "nvidia")
	require.NoError(t, os.MkdirAll(nvidiaPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nvidiaPath, "current_vgpu_type"), []byte("0"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(nvidiaPath, "creatable_vgpu_types"), []byte("ID    : vGPU Name\n1148  : NVIDIA L40S-2Q\n"), 0o644))

	originalVendorVFIO := hostVendorVFIO
	hostVendorVFIO = vendorVFIOSysfs{
		pciDevicesPath:  pciDevicesPath,
		procPath:        filepath.Join(root, "proc"),
		vfioDevicesPath: filepath.Join(root, "dev", "vfio", "devices"),
		owners:          make(map[string]string),
	}
	t.Cleanup(func() { hostVendorVFIO = originalVendorVFIO })
	require.NoError(t, os.MkdirAll(hostVendorVFIO.procPath, 0o755))

	m := &manager{
		paths:           paths.New(t.TempDir()),
		imageManager:    readyFixtureImageManager{name: "test-image"},
		instanceLocks:   sync.Map{},
		bootMarkerScans: sync.Map{},
	}
	const id = "start-rollback"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             id,
		Name:           id,
		Image:          "test-image",
		GPUProfile:     "NVIDIA L40S-2Q",
		HypervisorType: lifecycleNoopHypervisorType,
		SocketPath:     m.paths.InstanceSocket(id, "noop.sock"),
		DataDir:        m.paths.InstanceDir(id),
	}}))

	t.Setenv("TMPDIR", filepath.Join(root, "missing"))
	_, err := m.startInstance(context.Background(), id, StartInstanceRequest{})
	require.Error(t, err)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "NVIDIA L40S-2Q", stored.GPUProfile)
	assert.Empty(t, stored.GPUFramework)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Empty(t, stored.GPUMdevUUID)
	assertFileContents(t, filepath.Join(nvidiaPath, "current_vgpu_type"), "0")
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

func TestVGPUAssignmentClaimedByLiveInstanceFailsOnInvalidMetadata(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("invalid-instance"))
	require.NoError(t, os.WriteFile(m.paths.InstanceMetadata("invalid-instance"), []byte("{"), 0o644))

	_, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.Error(t, err)
}

func TestVGPUAssignmentClaimedByLiveInstanceNormalizesLegacyMdevPath(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("legacy-claimant"))
	pid := os.Getpid()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "legacy-claimant",
		Name:          "legacy-claimant",
		GPUMdevUUID:   "legacy-uuid",
		HypervisorPID: &pid,
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/mdev/devices/legacy-uuid")
	require.NoError(t, err)
	assert.True(t, claimed)
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

func TestReleaseStoredVGPURetainsMetadataOnFailure(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	stored := &StoredMetadata{
		GPUFramework:  devices.VGPUFramework("future-framework"),
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	err := m.releaseStoredVGPU(context.Background(), stored)
	assert.Error(t, err)
	assert.Equal(t, devices.VGPUFramework("future-framework"), stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
}

func TestSetAndClearStoredVGPUDevice(t *testing.T) {
	t.Parallel()

	stored := &StoredMetadata{}
	setStoredVGPUDevice(stored, &devices.VGPUDevice{
		Framework: devices.VGPUFrameworkVendorVFIO,
		SysfsPath: "/sys/bus/pci/devices/0000:82:00.4",
	})
	assert.Equal(t, devices.VGPUFrameworkVendorVFIO, stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)

	clearStoredVGPUDevice(stored)
	assert.Empty(t, stored.GPUFramework)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Empty(t, stored.GPUMdevUUID)
}
