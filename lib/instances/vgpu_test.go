package instances

import (
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/stretchr/testify/assert"
)

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
