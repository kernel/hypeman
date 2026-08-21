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

// VGPUAssignmentStartupGracePeriod bounds how long an assignment without a
// persisted hypervisor PID is treated as potentially live.
const VGPUAssignmentStartupGracePeriod = 5 * time.Minute

// VGPUCleanupPendingError reports a failed create whose vGPU release also
// failed during rollback. When Retained is true, deleting the retained instance
// retries the release; otherwise startup reconciliation recovers the assignment.
type VGPUCleanupPendingError struct {
	InstanceID string
	Retained   bool
	Err        error
}

func (e *VGPUCleanupPendingError) Error() string {
	if e.Retained {
		return fmt.Sprintf("%v; vGPU release failed during rollback, instance %s retains the assignment", e.Err, e.InstanceID)
	}
	return fmt.Sprintf("%v; vGPU release failed during rollback and the retention record for instance %s could not be saved; the assignment is recovered on the next startup reconcile", e.Err, e.InstanceID)
}

func (e *VGPUCleanupPendingError) Unwrap() error { return e.Err }

// errVGPURetentionStub rejects every lifecycle verb except delete on a
// retention stub from a failed create: the record has no boot configuration,
// and only delete retries the release of its retained assignment.
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

// retainedVGPUFromCreateError fills stub with the pending device's assignment
// fields when err carries a failed device-layer cleanup. The caller provides
// identity fields on stub so the retained record lists as a recognizable,
// deletable instance.
func retainedVGPUFromCreateError(stub StoredMetadata, assignedAt time.Time, err error) *StoredMetadata {
	device, ok := vgpuDevicePendingCleanup(err)
	if !ok {
		return nil
	}
	return retainedVGPUFromDevice(stub, device, assignedAt)
}

// retainedVGPUFromDevice fills stub with device's assignment fields so a
// failed rollback release retains a recognizable, deletable record.
func retainedVGPUFromDevice(stub StoredMetadata, device *devices.VGPUDevice, assignedAt time.Time) *StoredMetadata {
	stub.GPUProfile = device.ProfileName
	setStoredVGPUDevice(&stub, device, assignedAt)
	return &stub
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

// cleanupStartVGPU wholesale-restores the pre-start metadata snapshot. The
// cleanup stack is LIFO, so cleanups registered after this one run before it
// and this restore would clobber anything they persisted; it is safe only
// while no such cleanup writes metadata and the instance lock serializes
// start. Violating that requires switching to targeted field restores.
//
// It reports whether the assignment was retained after a failed destroy and
// whether that retention record was persisted, so start can surface the
// pending cleanup as a typed error like create does.
func (m *manager) cleanupStartVGPU(ctx context.Context, instanceID string, device *devices.VGPUDevice, assignedAt time.Time, rollbackMeta metadata) (retained, persisted bool) {
	logger.FromContext(ctx).DebugContext(ctx, "destroying vGPU on cleanup", "instance_id", instanceID, "uuid", device.MdevUUID)
	assignment := devices.VGPUAssignment{
		Framework:  device.Framework,
		DevicePath: device.SysfsPath,
		MdevUUID:   device.MdevUUID,
		InstanceID: instanceID,
	}
	cleanupMeta := rollbackMeta
	releaseErr := m.destroyVGPUAssignment(ctx, assignment)
	if releaseErr != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to destroy vGPU on cleanup", "instance_id", instanceID, "uuid", device.MdevUUID, "error", releaseErr)
		setStoredVGPUDevice(&cleanupMeta.StoredMetadata, device, assignedAt)
		retained = true
	}
	if err := m.saveMetadata(&cleanupMeta); err != nil {
		message := "failed to save metadata after vGPU cleanup"
		if releaseErr != nil {
			message = "failed to retain vGPU assignment metadata after cleanup failure"
		}
		logger.FromContext(ctx).ErrorContext(ctx, message, "instance_id", instanceID, "error", err)
		if !retained {
			return false, false
		}
		// The mid-start save may already have persisted this assignment, in
		// which case the on-disk record still points at the device and
		// delete or a retried start can release it (matching create's
		// retention-survives check).
		if meta, loadErr := m.loadMetadata(instanceID); loadErr == nil && storedVGPUDevicePath(&meta.StoredMetadata) == device.SysfsPath {
			return true, true
		}
		return true, false
	}
	return retained, retained
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

// vgpuAssignmentClaimedByLiveInstance reports whether another live instance's
// stored metadata claims devicePath. It reads raw metadata instead of
// hydrating full instances: the scan runs on every vendor VFIO release, and
// deriving state would query the hypervisor of every instance on the host.
// A confirmed live claimant returns true. Unreadable metadata, a recent
// assignment without a PID, or unverifiable process ownership returns an error
// so the requester retains its assignment for a later retry.
func (m *manager) vgpuAssignmentClaimedByLiveInstance(ctx context.Context, excludeID, devicePath string) (bool, error) {
	files, err := m.listMetadataFilesStrict()
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
			if errors.Is(err, ErrNotFound) {
				// Deleted between listing and load; a vanished record cannot
				// be a live claimant.
				continue
			}
			return false, fmt.Errorf("load metadata for vGPU release check: instance %s: %w", id, err)
		}
		stored := &meta.StoredMetadata
		if storedVGPUDevicePath(stored) != devicePath {
			continue
		}
		if stored.HypervisorPID == nil {
			if stored.GPUAssignedAt == nil || time.Since(*stored.GPUAssignedAt) >= VGPUAssignmentStartupGracePeriod {
				continue
			}
			return false, fmt.Errorf("cannot confirm liveness of recent vGPU claimant %s on %s: no persisted hypervisor PID", id, devicePath)
		}
		pid, err := resolveLiveHypervisorPID(stored.HypervisorProcessIdentity, stored.SocketPath)
		if err != nil {
			return false, fmt.Errorf("cannot confirm liveness of vGPU claimant %s on %s: %w", id, devicePath, err)
		}
		if pid > 0 {
			return true, nil
		}
		// A dead PID with a recent assignment gets the same bounded grace as
		// startup reconcile protection, so the two guards agree in the
		// fail-closed direction while a mid-boot claimant hydrates.
		if stored.GPUAssignedAt != nil && time.Since(*stored.GPUAssignedAt) < VGPUAssignmentStartupGracePeriod {
			return false, fmt.Errorf("cannot confirm liveness of recent vGPU claimant %s on %s: recorded hypervisor is not running", id, devicePath)
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
	if stored.GPURetainedForCleanup {
		// Delete-only retention stubs release through delete. Releasing here
		// would leave a stub whose start/fork/snapshot errors still claim a
		// retained assignment that no longer exists.
		return
	}
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
