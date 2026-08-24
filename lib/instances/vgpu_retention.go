package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

type vgpuRetention struct {
	instanceID string
	stub       *StoredMetadata
	retained   bool
	persisted  bool
}

func (r *vgpuRetention) retainFromCreateError(stub StoredMetadata, assignedAt time.Time, err error) {
	device, ok := vgpuDevicePendingCleanup(err)
	if !ok {
		return
	}
	r.retainFromDevice(stub, device, assignedAt)
}

func (r *vgpuRetention) retainFromDevice(stub StoredMetadata, device *devices.VGPUDevice, assignedAt time.Time) {
	stub.GPUProfile = device.ProfileName
	setStoredVGPUDevice(&stub, device, assignedAt)
	r.stub = &stub
	r.retained = true
}

func (r *vgpuRetention) markRetained(persisted bool) {
	r.retained = true
	r.persisted = persisted
}

func (r *vgpuRetention) wrapPending(err error) error {
	if err == nil || !r.retained {
		return err
	}
	return &VGPUCleanupPendingError{InstanceID: r.instanceID, Retained: r.persisted, Err: err}
}

// deferWrapPending must be deferred before cleanup so it observes retention
// state recorded by rollback.
func (r *vgpuRetention) deferWrapPending(retErr *error) {
	*retErr = r.wrapPending(*retErr)
}

func (m *manager) persistVGPURetention(ctx context.Context, retention *vgpuRetention) {
	if retention.stub == nil {
		m.deleteInstanceData(retention.instanceID)
		return
	}

	id := retention.instanceID
	retainedVGPU := retention.stub
	log := logger.FromContext(ctx)
	retentionSurvives := func() bool {
		meta, err := m.loadMetadata(id)
		if err == nil && storedVGPUDevicePath(&meta.StoredMetadata) != "" {
			return true
		}
		if err := m.deleteInstanceData(id); err != nil {
			log.ErrorContext(ctx, "failed to delete stale instance data after retention failure", "instance_id", id, "error", err)
		}
		// No on-disk record points at the device; retry the release in the
		// background instead of waiting for the next startup reconcile.
		m.scheduleOrphanedVGPURelease(ctx, *retainedVGPU)
		return false
	}
	if err := m.deleteInstanceData(id); err != nil {
		log.ErrorContext(ctx, "failed to clean instance data before retaining vGPU assignment", "instance_id", id, "error", err)
		retention.persisted = retentionSurvives()
		return
	}
	if err := m.ensureDirectories(id); err != nil {
		log.ErrorContext(ctx, "failed to retain instance data after vGPU cleanup failure", "instance_id", id, "error", err)
		retention.persisted = retentionSurvives()
		return
	}
	retained := StoredMetadata{
		Id:                    id,
		Name:                  retainedVGPU.Name,
		Image:                 retainedVGPU.Image,
		ResolvedImage:         retainedVGPU.ResolvedImage,
		Platform:              retainedVGPU.Platform,
		CreatedAt:             retainedVGPU.CreatedAt,
		HypervisorType:        retainedVGPU.HypervisorType,
		HypervisorVersion:     retainedVGPU.HypervisorVersion,
		SocketPath:            retainedVGPU.SocketPath,
		DataDir:               retainedVGPU.DataDir,
		GPUProfile:            retainedVGPU.GPUProfile,
		GPUFramework:          retainedVGPU.GPUFramework,
		GPUDevicePath:         retainedVGPU.GPUDevicePath,
		GPUMdevUUID:           retainedVGPU.GPUMdevUUID,
		GPUAssignedAt:         retainedVGPU.GPUAssignedAt,
		GPURetainedForCleanup: true,
	}
	if err := m.saveMetadata(&metadata{StoredMetadata: retained}); err != nil {
		log.ErrorContext(ctx, "failed to retain vGPU assignment metadata after cleanup failure", "instance_id", id, "error", err)
		retention.persisted = retentionSurvives()
		return
	}
	retention.persisted = true
}
