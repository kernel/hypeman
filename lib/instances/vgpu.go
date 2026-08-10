package instances

import (
	"context"
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

func releaseStoredVGPU(ctx context.Context, stored *StoredMetadata) error {
	path := storedVGPUDevicePath(stored)
	if path != "" {
		if err := devices.DestroyVGPU(ctx, stored.GPUFramework, path, stored.GPUMdevUUID); err != nil {
			return err
		}
	}
	clearStoredVGPUDevice(stored)
	return nil
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
