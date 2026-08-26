package instances

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

// VGPUCleanupPendingError reports a failed rollback that left a vGPU assigned.
type VGPUCleanupPendingError struct {
	InstanceID string
	Retained   bool
	Err        error
}

func (e *VGPUCleanupPendingError) Error() string {
	if e.Retained {
		return fmt.Sprintf("%v; vGPU release failed during rollback, instance %s retains the assignment", e.Err, e.InstanceID)
	}
	return fmt.Sprintf("%v; vGPU release failed during rollback and the retention record for instance %s could not be saved; the periodic vGPU reconcile retries the release", e.Err, e.InstanceID)
}

func (e *VGPUCleanupPendingError) Unwrap() error { return e.Err }

var errVGPURetentionStub = fmt.Errorf("%w: instance retains a vGPU assignment from a failed create and has no boot configuration; delete it to release the assignment", ErrInvalidState)

func (m *manager) createVGPUDevice(ctx context.Context, profileName, instanceID string) (*devices.VGPUDevice, error) {
	create := m.createVGPU
	if create == nil {
		create = devices.CreateVGPU
	}
	return create(ctx, profileName, instanceID)
}

func vgpuDevicePendingCleanup(err error) (*devices.VGPUDevice, bool) {
	var pending *devices.VGPUCreateCleanupPendingError
	if !errors.As(err, &pending) {
		return nil, false
	}
	return &pending.Device, true
}

func vgpuAssignmentMayBeLive(stored *StoredMetadata, now time.Time, hypervisorLive bool) bool {
	return hypervisorLive ||
		stored.GPUAssignedAt != nil && now.Sub(*stored.GPUAssignedAt) < devices.VGPUAssignmentGracePeriod
}

func (m *manager) destroyVGPUAssignment(ctx context.Context, assignment devices.VGPUAssignment) error {
	destroy := m.destroyVGPU
	if destroy == nil {
		destroy = devices.DestroyVGPU
	}
	return destroy(ctx, assignment)
}

func setStoredVGPUDevice(stored *StoredMetadata, device *devices.VGPUDevice, assignedAt time.Time) {
	stored.GPUFramework = device.Framework
	stored.GPUDevicePath = device.SysfsPath
	stored.GPUMdevUUID = device.MdevUUID
	stored.GPUAssignedAt = &assignedAt
}

func clearStoredVGPUDevice(stored *StoredMetadata) {
	stored.GPUFramework = devices.VGPUFrameworkNone
	stored.GPUDevicePath = ""
	stored.GPUMdevUUID = ""
	stored.GPUAssignedAt = nil
}

func (m *manager) cleanupStartVGPU(ctx context.Context, instanceID string, device *devices.VGPUDevice, assignedAt time.Time, rollbackMeta metadata) (retained, persisted bool) {
	logger.FromContext(ctx).DebugContext(ctx, "destroying vGPU on cleanup", "instance_id", instanceID, "uuid", device.MdevUUID)
	releaseErr := m.destroyVGPUAssignment(ctx, devices.VGPUAssignment{
		Framework:  device.Framework,
		DevicePath: device.SysfsPath,
		MdevUUID:   device.MdevUUID,
		InstanceID: instanceID,
	})
	if releaseErr != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to destroy vGPU on cleanup", "instance_id", instanceID, "uuid", device.MdevUUID, "error", releaseErr)
		setStoredVGPUDevice(&rollbackMeta.StoredMetadata, device, assignedAt)
		retained = true
	}
	if err := m.saveMetadata(&rollbackMeta); err != nil {
		message := "failed to save metadata after vGPU cleanup"
		if retained {
			message = "failed to retain vGPU assignment metadata after cleanup failure"
		}
		logger.FromContext(ctx).ErrorContext(ctx, message, "instance_id", instanceID, "error", err)
		if !retained {
			return false, false
		}
		meta, loadErr := m.loadMetadata(instanceID)
		return true, loadErr == nil && storedVGPUDevicePath(&meta.StoredMetadata) == device.SysfsPath
	}
	return retained, retained
}

func (m *manager) releaseStoredVGPU(ctx context.Context, stored *StoredMetadata) error {
	path := storedVGPUDevicePath(stored)
	if path != "" {
		// Vendor VFIO VFs are reusable, so release fails closed on an incomplete inventory.
		claimed := false
		if stored.GPUFramework == devices.VGPUFrameworkVendorVFIO {
			var err error
			claimed, err = m.vgpuAssignmentClaimedByLiveInstance(stored.Id, path)
			if err != nil {
				return err
			}
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
			if err := m.destroyVGPUAssignment(ctx, assignment); err != nil {
				return err
			}
		}
	}
	clearStoredVGPUDevice(stored)
	return nil
}

func (m *manager) vgpuAssignmentClaimedByLiveInstance(excludeID, devicePath string) (bool, error) {
	allMetadata, err := m.listMetadataForReconcile()
	if err != nil {
		return false, fmt.Errorf("list instances for vGPU release check: %w", err)
	}
	for i := range allMetadata {
		stored := &allMetadata[i]
		if stored.Id == excludeID || storedVGPUDevicePath(stored) != devicePath {
			continue
		}
		pid, err := resolveLiveHypervisorPID(stored.HypervisorProcessIdentity, stored.SocketPath)
		if err != nil {
			return false, fmt.Errorf("cannot confirm liveness of vGPU claimant %s on %s: %w", stored.Id, devicePath, err)
		}
		if pid > 0 {
			return true, nil
		}
		if vgpuAssignmentMayBeLive(stored, m.nowUTC(), false) {
			if stored.HypervisorPID == nil {
				return false, fmt.Errorf("cannot confirm liveness of recent vGPU claimant %s on %s: no persisted hypervisor PID", stored.Id, devicePath)
			}
			return false, fmt.Errorf("cannot confirm liveness of recent vGPU claimant %s on %s: recorded hypervisor is not running", stored.Id, devicePath)
		}
	}
	return false, nil
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
