package instances

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
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

// releaseRetainedVGPULocked releases a vGPU assignment retained on a stopped
// instance after a failed release during the original stop. It is a no-op
// when no assignment is retained. The caller must hold the instance lock.
func (m *manager) releaseRetainedVGPULocked(ctx context.Context, id string) error {
	meta, err := m.loadMetadata(id)
	if err != nil {
		return err
	}
	stored := &meta.StoredMetadata
	if storedVGPUDevicePath(stored) == "" {
		return nil
	}
	if err := releaseStoredVGPU(ctx, stored); err != nil {
		logger.FromContext(ctx).ErrorContext(ctx, "failed to destroy retained vGPU; retaining assignment metadata", "instance_id", id, "error", err)
		return fmt.Errorf("destroy vGPU: %w", err)
	}
	return m.saveMetadata(meta)
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
