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

func (m *manager) persistVGPURetention(ctx context.Context, retention *vgpuRetention) {
	if retention.stub == nil {
		m.deleteInstanceData(retention.instanceID)
		return
	}

	id := retention.instanceID
	retainedVGPU := retention.stub
	log := logger.FromContext(ctx)
	defer func() {
		m.recordVGPURetainedAssignment(ctx, vgpuRetentionOperationCreate, retention.persisted)
	}()

	// An unpersisted retention leaves no metadata claim. The periodic reconciler releases
	// the VF after its grace period once no open VFIO handles remain.
	if err := m.deleteInstanceData(id); err != nil {
		log.ErrorContext(ctx, "failed to clean instance data before retaining vGPU assignment", "instance_id", id, "error", err)
		return
	}
	if err := m.ensureDirectories(id); err != nil {
		log.ErrorContext(ctx, "failed to retain instance data after vGPU cleanup failure", "instance_id", id, "error", err)
		return
	}
	if err := m.saveVGPURetentionStub(retainedVGPU); err != nil {
		log.ErrorContext(ctx, "failed to retain vGPU assignment metadata after cleanup failure", "instance_id", id, "error", err)
		return
	}
	retention.persisted = true
}

func vgpuRetentionMetadata(source *StoredMetadata) *metadata {
	return &metadata{StoredMetadata: StoredMetadata{
		Id:                    source.Id,
		Name:                  source.Name,
		Image:                 source.Image,
		ResolvedImage:         source.ResolvedImage,
		Platform:              source.Platform,
		CreatedAt:             source.CreatedAt,
		HypervisorType:        source.HypervisorType,
		HypervisorVersion:     source.HypervisorVersion,
		SocketPath:            source.SocketPath,
		DataDir:               source.DataDir,
		GPUProfile:            source.GPUProfile,
		GPUFramework:          source.GPUFramework,
		GPUDevicePath:         source.GPUDevicePath,
		GPUMdevUUID:           source.GPUMdevUUID,
		GPUAssignedAt:         source.GPUAssignedAt,
		GPURetainedForCleanup: true,
	}}
}

func (m *manager) saveVGPURetentionStub(source *StoredMetadata) error {
	return m.saveMetadata(vgpuRetentionMetadata(source))
}
