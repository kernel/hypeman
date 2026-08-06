package instances

import (
	"context"
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

func (m *manager) releaseStoredVGPU(ctx context.Context, stored *StoredMetadata) error {
	path := storedVGPUDevicePath(stored)
	if path != "" {
		claimed, err := m.vgpuAssignmentClaimedByLiveInstance(ctx, stored.Id, path)
		if err != nil {
			return err
		}
		if claimed {
			logger.FromContext(ctx).WarnContext(ctx, "dropping stale vGPU assignment claimed by another live instance",
				"instance_id", stored.Id, "device_path", path)
		} else {
			assignment := devices.VGPUAssignment{
				Framework:  stored.GPUFramework,
				DevicePath: path,
				MdevUUID:   stored.GPUMdevUUID,
				InstanceID: stored.Id,
			}
			if err := devices.DestroyVGPU(ctx, assignment); err != nil {
				return err
			}
		}
	}
	clearStoredVGPUDevice(stored)
	return nil
}

func (m *manager) vgpuAssignmentClaimedByLiveInstance(ctx context.Context, excludeID, devicePath string) (bool, error) {
	instances, err := m.listInstances(ctx)
	if err != nil {
		return false, fmt.Errorf("list instances for vGPU release check: %w", err)
	}
	for i := range instances {
		inst := &instances[i]
		if inst.Id == excludeID || inst.GPUDevicePath != devicePath || inst.HypervisorPID == nil {
			continue
		}
		if HypervisorProcessExists(*inst.HypervisorPID, inst.SocketPath) {
			return true, nil
		}
	}
	return false, nil
}

// releaseRetainedVGPULocked releases a vGPU assignment retained on a stopped
// instance after a failed release during the original stop. It is a no-op
// when no assignment is retained, and a failed retry only logs so the
// metadata stays for the next retry. The caller must hold the instance lock.
func (m *manager) releaseRetainedVGPULocked(ctx context.Context, id string) {
	log := logger.FromContext(ctx)
	meta, err := m.loadMetadata(id)
	if err != nil {
		log.WarnContext(ctx, "failed to load metadata for retained vGPU release", "instance_id", id, "error", err)
		return
	}
	stored := &meta.StoredMetadata
	if storedVGPUDevicePath(stored) == "" {
		return
	}
	if err := m.releaseStoredVGPU(ctx, stored); err != nil {
		log.WarnContext(ctx, "failed to destroy retained vGPU; retaining assignment metadata", "instance_id", id, "error", err)
		return
	}
	if err := m.saveMetadata(meta); err != nil {
		log.WarnContext(ctx, "failed to save metadata after retained vGPU release", "instance_id", id, "error", err)
	}
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
