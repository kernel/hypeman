package instances

import (
	"path/filepath"

	"github.com/kernel/hypeman/lib/devices"
)

func setStoredVGPUDevice(stored *StoredMetadata, device *devices.VGPUDevice) {
	stored.GPUFramework = device.Framework
	stored.GPUDevicePath = device.SysfsPath
	stored.GPUMdevUUID = device.MdevUUID
}

func clearStoredVGPUDevice(stored *StoredMetadata) {
	stored.GPUFramework = devices.VGPUFrameworkNone
	stored.GPUDevicePath = ""
	stored.GPUMdevUUID = ""
}

func storedVGPUDevicePath(stored *StoredMetadata) string {
	if stored.GPUDevicePath != "" {
		return stored.GPUDevicePath
	}
	if stored.GPUMdevUUID != "" {
		return filepath.Join("/sys/bus/mdev/devices", stored.GPUMdevUUID)
	}
	return ""
}
