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
const VGPUAssignmentStartupGracePeriod = devices.VGPUAssignmentGracePeriod

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

func vgpuAssignmentLiveness(stored *StoredMetadata, now time.Time, livePID bool) (live bool, graceRemaining time.Duration) {
	if livePID {
		return true, 0
	}
	if stored.GPUAssignedAt == nil {
		return false, 0
	}
	remaining := VGPUAssignmentStartupGracePeriod - now.Sub(*stored.GPUAssignedAt)
	if remaining <= 0 {
		return false, 0
	}
	return true, remaining
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
	assignment := devices.VGPUAssignment{
		Framework:  device.Framework,
		DevicePath: device.SysfsPath,
		MdevUUID:   device.MdevUUID,
		InstanceID: instanceID,
	}
	cleanupMeta, err := m.loadMetadata(instanceID)
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to load current metadata for vGPU cleanup; restoring rollback snapshot", "instance_id", instanceID, "error", err)
		cleanupMeta = &rollbackMeta
	} else {
		restoreStartMutatedFields(&cleanupMeta.StoredMetadata, &rollbackMeta.StoredMetadata)
	}
	releaseErr := m.destroyVGPUAssignment(ctx, assignment)
	if releaseErr != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to destroy vGPU on cleanup", "instance_id", instanceID, "uuid", device.MdevUUID, "error", releaseErr)
		setStoredVGPUDevice(&cleanupMeta.StoredMetadata, device, assignedAt)
		retained = true
	}
	if err := m.saveMetadata(cleanupMeta); err != nil {
		message := "failed to save metadata after vGPU cleanup"
		if releaseErr != nil {
			message = "failed to retain vGPU assignment metadata after cleanup failure"
		}
		logger.FromContext(ctx).ErrorContext(ctx, message, "instance_id", instanceID, "error", err)
		if !retained {
			return false, false
		}
		if meta, loadErr := m.loadMetadata(instanceID); loadErr == nil && storedVGPUDevicePath(&meta.StoredMetadata) == device.SysfsPath {
			return true, true
		}
		return true, false
	}
	return retained, retained
}

func restoreStartMutatedFields(dst, src *StoredMetadata) {
	dst.HypervisorPID = src.HypervisorPID
	dst.HypervisorStartTime = src.HypervisorStartTime
	dst.HypervisorBootID = src.HypervisorBootID
	dst.ExitCode = src.ExitCode
	dst.ExitMessage = src.ExitMessage
	dst.ProgramStartedAt = src.ProgramStartedAt
	dst.GuestAgentReadyAt = src.GuestAgentReadyAt
	dst.Entrypoint = src.Entrypoint
	dst.Cmd = src.Cmd
	dst.IP = src.IP
	dst.MAC = src.MAC
	dst.GPUFramework = src.GPUFramework
	dst.GPUDevicePath = src.GPUDevicePath
	dst.GPUMdevUUID = src.GPUMdevUUID
	dst.GPUAssignedAt = src.GPUAssignedAt
	dst.StartedAt = src.StartedAt
}

func (m *manager) releaseStoredVGPU(ctx context.Context, stored *StoredMetadata) error {
	return m.releaseStoredVGPUExcluding(ctx, stored, stored.Id)
}

func (m *manager) releaseStoredVGPUExcluding(ctx context.Context, stored *StoredMetadata, excludeID string) error {
	path := storedVGPUDevicePath(stored)
	if path != "" {
		// Vendor VFIO VFs are reusable, so release fails closed on an incomplete inventory.
		claimed := false
		if stored.GPUFramework == devices.VGPUFrameworkVendorVFIO {
			var err error
			claimed, err = m.vgpuAssignmentClaimedByLiveInstance(ctx, excludeID, path)
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
				continue
			}
			return false, fmt.Errorf("load metadata for vGPU release check: instance %s: %w", id, err)
		}
		stored := &meta.StoredMetadata
		if storedVGPUDevicePath(stored) != devicePath {
			continue
		}
		// Resolve even without a persisted PID: the socket-ownership scan can
		// still prove a claimant whose post-boot metadata save failed is live.
		pid, err := resolveLiveHypervisorPID(stored.HypervisorProcessIdentity, stored.SocketPath)
		if err != nil {
			return false, fmt.Errorf("cannot confirm liveness of vGPU claimant %s on %s: %w", id, devicePath, err)
		}
		live, remaining := vgpuAssignmentLiveness(stored, m.nowUTC(), pid > 0)
		if pid > 0 && live {
			return true, nil
		}
		if remaining > 0 {
			if stored.HypervisorPID == nil {
				return false, fmt.Errorf("cannot confirm liveness of recent vGPU claimant %s on %s: no persisted hypervisor PID", id, devicePath)
			}
			return false, fmt.Errorf("cannot confirm liveness of recent vGPU claimant %s on %s: recorded hypervisor is not running", id, devicePath)
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
