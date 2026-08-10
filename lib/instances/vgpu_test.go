package instances

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
)

func TestValidateVGPUHypervisor(t *testing.T) {
	t.Parallel()

	assert.NoError(t, validateVGPUHypervisor(hypervisor.TypeQEMU))
	assert.EqualError(t, validateVGPUHypervisor(hypervisor.TypeCloudHypervisor), "vGPU is only supported with qemu, got cloud-hypervisor")
}

func TestStoredVGPUDevicePath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/sys/bus/mdev/devices/new-uuid", storedVGPUDevicePath(&StoredMetadata{
		GPUDevicePath: "/sys/bus/mdev/devices/new-uuid",
		GPUMdevUUID:   "legacy-uuid",
	}))
	assert.Equal(t, "/sys/bus/mdev/devices/legacy-uuid", storedVGPUDevicePath(&StoredMetadata{
		GPUMdevUUID: "legacy-uuid",
	}))
	assert.Empty(t, storedVGPUDevicePath(&StoredMetadata{}))
}

func TestReleaseStoredVGPURetainsMetadataOnFailure(t *testing.T) {
	t.Parallel()

	stored := &StoredMetadata{
		GPUFramework:  devices.VGPUFramework("future-framework"),
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	err := releaseStoredVGPU(context.Background(), stored)
	assert.Error(t, err)
	assert.Equal(t, devices.VGPUFramework("future-framework"), stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
}

func TestSetAndClearStoredVGPUDevice(t *testing.T) {
	t.Parallel()

	stored := &StoredMetadata{}
	setStoredVGPUDevice(stored, &devices.VGPUDevice{
		Framework: devices.VGPUFrameworkMdev,
		SysfsPath: "/sys/bus/mdev/devices/new-uuid",
		MdevUUID:  "new-uuid",
	})
	assert.Equal(t, devices.VGPUFrameworkMdev, stored.GPUFramework)
	assert.Equal(t, "/sys/bus/mdev/devices/new-uuid", stored.GPUDevicePath)
	assert.Equal(t, "new-uuid", stored.GPUMdevUUID)

	clearStoredVGPUDevice(stored)
	assert.Empty(t, stored.GPUFramework)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Empty(t, stored.GPUMdevUUID)
}
