package instances

import (
	"context"
	"path/filepath"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
)

func validateVGPUHypervisor(hvType hypervisor.Type) error {
	if hvType != hypervisor.TypeQEMU {
		return fmt.Errorf("vGPU is only supported with qemu, got %s", hvType)
	}
	return nil
}

// VGPUCleanupPendingError reports a failed create whose vGPU release also
// failed during rollback. The instance record identified by InstanceID is
// retained so the release can be retried; deleting the instance retries it.
type VGPUCleanupPendingError struct {
	InstanceID string
	Err        error
}

func (e *VGPUCleanupPendingError) Error() string {
	return fmt.Sprintf("%v; vGPU release failed during rollback, instance %s retains the assignment", e.Err, e.InstanceID)
}

func (e *VGPUCleanupPendingError) Unwrap() error { return e.Err }

func (m *manager) createVGPUDevice(ctx context.Context, profileName, instanceID string) (*devices.VGPUDevice, error) {
	create := m.createVGPU
	if create == nil {
		create = devices.CreateVGPU
	}
	return create(ctx, profileName, instanceID)
}

func (m *manager) destroyVGPUAssignment(ctx context.Context, assignment devices.VGPUAssignment) error {
	destroy := m.destroyVGPU
	if destroy == nil {
		destroy = devices.DestroyVGPU
	}
	return destroy(ctx, assignment)
}

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
		// Vendor VFIO VFs are reused across instances, so stale metadata can
		// point at a path claimed by a live instance and the release must fail
		// closed on an incomplete inventory. mdev UUIDs are unique and never
		// reused, so skip the scan there — it would let one unreadable
		// metadata file block every mdev release on the host.
		claimed := false
		if stored.GPUFramework == devices.VGPUFrameworkVendorVFIO {
			var err error
			claimed, err = m.vgpuAssignmentClaimedByLiveInstance(ctx, stored.Id, path)
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

// vgpuAssignmentClaimedByLiveInstance reports whether another instance's
// stored metadata claims devicePath. It reads raw metadata instead of
// hydrating full instances: the scan runs on every vendor VFIO release, and
// deriving state would query the hypervisor of every instance on the host.
// It fails closed: unreadable metadata is an error, and a matching claim
// without a persisted PID counts as live because the PID is only persisted
// after the claimant's hypervisor starts.
func (m *manager) vgpuAssignmentClaimedByLiveInstance(ctx context.Context, excludeID, devicePath string) (bool, error) {
	files, err := m.listMetadataFilesWithStatErrors(true)
	if err != nil {
		return false, fmt.Errorf("list instances for vGPU release check: %w", err)
	}
	for _, file := range files {
		id := filepath.Base(filepath.Dir(file))
		if id == excludeID {
			continue
		}
		meta, err := m.loadMetadata(id)
		if err != nil {
			return false, fmt.Errorf("load metadata for vGPU release check: instance %s: %w", id, err)
		}
		stored := &meta.StoredMetadata
		if storedVGPUDevicePath(stored) != devicePath {
			continue
		}
		if stored.HypervisorPID == nil {
			return true, nil
		}
		if HypervisorProcessExists(*stored.HypervisorPID, stored.SocketPath) {
			return true, nil
		}
		// The stored PID can be stale after a hypeman restart; a live owner
		// of the claimant's socket still marks the claim as live.
		if stored.SocketPath != "" && !ProcessExists(*stored.HypervisorPID) {
			if owner, _, err := hypervisor.ResolveProcessPID(stored.SocketPath); err == nil && ProcessExists(owner) {
				return true, nil
			}
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
