package instances

import (
	"context"
	"fmt"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
)

func sharedVGPURestoreSlots(limit int) chan struct{} {
	key := restoreSlotPoolKey{hypervisorType: hypervisor.TypeQEMU, limit: limit, gpuOnly: true}
	slots, _ := restoreSlotPools.LoadOrStore(key, make(chan struct{}, limit))
	return slots.(chan struct{})
}

func (m *manager) acquireInstanceRestoreSlot(ctx context.Context, stored *StoredMetadata) (func(), error) {
	release, err := m.acquireRestoreSlot(ctx, stored.HypervisorType)
	if err != nil {
		return nil, err
	}
	if !usesVGPUSnapshot(stored) || m.vgpuRestoreSlots == nil {
		return release, nil
	}
	select {
	case m.vgpuRestoreSlots <- struct{}{}:
		return func() { <-m.vgpuRestoreSlots; release() }, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func (m *manager) shutdownFailedRestore(ctx context.Context, stored *StoredMetadata, hv hypervisor.Hypervisor) error {
	if !usesVGPUSnapshot(stored) {
		return hv.Shutdown(ctx)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return m.shutdownHypervisor(cleanupCtx, &Instance{StoredMetadata: *stored})
}

func usesVGPUSnapshot(stored *StoredMetadata) bool {
	return stored.HypervisorType == hypervisor.TypeQEMU && stored.HypervisorVersion == qemu.VGPUSnapshotVersion && stored.GPUProfile != ""
}

func validateVGPUSnapshotRequest(req CreateInstanceRequest, hvType hypervisor.Type, version string) error {
	if version != qemu.VGPUSnapshotVersion {
		return nil
	}
	if hvType != hypervisor.TypeQEMU || req.GPU == nil || req.GPU.Profile != "NVIDIA L40S-3Q" || req.SkipGuestAgent || len(req.Devices) != 0 {
		return fmt.Errorf("%w: the managed vGPU snapshot runtime requires an L40S-3Q guest with its guest agent and no additional PCI devices", ErrInvalidRequest)
	}
	return nil
}

func validateVGPUSnapshotCapture(stored *StoredMetadata) error {
	if !usesVGPUSnapshot(stored) || stored.GPUFramework != devices.VGPUFrameworkVendorVFIO || stored.GPUMdevUUID != "" || stored.GPUProfile != "NVIDIA L40S-3Q" || stored.SkipGuestAgent {
		return fmt.Errorf("%w: vGPU snapshots require the opt-in managed QEMU runtime and vendor VFIO L40S-3Q", ErrInvalidState)
	}
	return nil
}

// prepareVGPUSnapshotRestore persists a fresh claim before QEMU can open it.
// The returned rollback must only release a VF after the restoring VMM has exited.
func (m *manager) prepareVGPUSnapshotRestore(ctx context.Context, meta *metadata, original StoredMetadata) (func(), error) {
	stored := &meta.StoredMetadata
	if !usesVGPUSnapshot(stored) {
		return func() {}, nil
	}
	if storedVGPUDevicePath(stored) != "" {
		if m.vgpuCleanupGuard(ctx, stored) == "" {
			return nil, fmt.Errorf("cannot replace an uncertain live vGPU claim")
		}
		if err := m.releaseStoredVGPUPersisted(ctx, meta); err != nil {
			return nil, fmt.Errorf("release previous vGPU claim: %w", err)
		}
	}
	rollback := metadata{StoredMetadata: cloneStoredMetadataWithoutPendingStandbyCompression(original)}
	clearStoredVGPUDevice(&rollback.StoredMetadata)
	device, err := m.claimVGPU(ctx, meta, stored.GPUProfile)
	if err != nil {
		return nil, fmt.Errorf("claim vGPU for snapshot restore: %w", err)
	}
	setStoredVGPUDevice(stored, device)
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		m.cleanupStartVGPU(cleanupCtx, stored, rollback)
	}
	if err := validateVGPUSnapshotCapture(stored); err != nil {
		cleanup()
		return nil, err
	}
	if err := m.configureClaimedVGPU(ctx, device); err != nil {
		cleanup()
		return nil, err
	}
	starter, err := m.getVMStarter(stored.HypervisorType)
	if err != nil {
		cleanup()
		return nil, err
	}
	_, err = starter.PrepareFork(ctx, hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: m.paths.InstanceSnapshotConfig(stored.Id),
		InstanceID:         stored.Id,
		VGPUDevicePath:     storedVGPUDevicePath(stored),
		VsockCID:           stored.VsockCID,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("bind snapshot to fresh vGPU: %w", err)
	}
	return cleanup, nil
}
