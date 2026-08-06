package instances

import (
	"context"
	"os"
	"testing"

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
		HypervisorType: "qemu",
		DataDir:        m.paths.InstanceDir("failed-create"),
	}

	m.cleanupFailedCreate(context.Background(), stored.Id, stored)

	retained, err := m.loadMetadata(stored.Id)
	require.NoError(t, err)
	assert.Equal(t, stored.GPUProfile, retained.GPUProfile)
	assert.Equal(t, stored.GPUFramework, retained.GPUFramework)
	assert.Equal(t, stored.GPUDevicePath, retained.GPUDevicePath)
}

func TestVGPUAssignmentClaimedByLiveInstanceFailsOnInvalidMetadata(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("invalid-instance"))
	require.NoError(t, os.WriteFile(m.paths.InstanceMetadata("invalid-instance"), []byte("{"), 0o644))

	_, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.Error(t, err)
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
