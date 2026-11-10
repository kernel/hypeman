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

// acquireInstanceRestoreSlot takes the vGPU slot before the hypervisor-wide
// one, so queued vGPU restores do not hold capacity other guests could use.
func (m *manager) acquireInstanceRestoreSlot(ctx context.Context, stored *StoredMetadata) (func(), error) {
	if !usesVGPUSnapshot(stored) || m.vgpuRestoreSlots == nil {
		return m.acquireRestoreSlot(ctx, stored.HypervisorType)
	}
	select {
	case m.vgpuRestoreSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release, err := m.acquireRestoreSlot(ctx, stored.HypervisorType)
	if err != nil {
		<-m.vgpuRestoreSlots
		return nil, err
	}
	return func() { release(); <-m.vgpuRestoreSlots }, nil
}

func (m *manager) shutdownFailedRestore(ctx context.Context, stored *StoredMetadata, hv hypervisor.Hypervisor) error {
	if !usesVGPUSnapshot(stored) {
		return hv.Shutdown(ctx)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return m.shutdownHypervisor(cleanupCtx, &Instance{StoredMetadata: *stored})
}

// vgpuRestoreAgentTimeout bounds how long a restored vGPU guest's agent may
// take to apply its new network identity. The agent was already running when
// the snapshot was taken, so it answers in milliseconds or not at all; failing
// here leaves the caller time to boot a fresh guest instead.
const vgpuRestoreAgentTimeout = 10 * time.Second

func guestReconfigureContext(ctx context.Context, stored *StoredMetadata) (context.Context, context.CancelFunc) {
	if !usesVGPUSnapshot(stored) {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, vgpuRestoreAgentTimeout)
}

// vgpuSnapshotProfile is the only vGPU profile whose device state has been
// qualified for capture and restore.
const vgpuSnapshotProfile = "NVIDIA L40S-3Q"

func usesVGPUSnapshot(stored *StoredMetadata) bool {
	return stored.HypervisorType == hypervisor.TypeQEMU && stored.HypervisorVersion == qemu.VGPUSnapshotVersion && stored.GPUProfile != ""
}

func validateVGPUSnapshotRequest(req CreateInstanceRequest, hvType hypervisor.Type, version string) error {
	if version != qemu.VGPUSnapshotVersion {
		return nil
	}
	if hvType != hypervisor.TypeQEMU || req.GPU == nil || req.GPU.Profile != vgpuSnapshotProfile || req.SkipGuestAgent || len(req.Devices) != 0 {
		return fmt.Errorf("%w: the managed vGPU snapshot runtime requires a %s guest with its guest agent and no additional PCI devices", ErrInvalidRequest, vgpuSnapshotProfile)
	}
	return nil
}

func validateVGPUSnapshotCapture(stored *StoredMetadata) error {
	if !usesVGPUSnapshot(stored) || stored.GPUFramework != devices.VGPUFrameworkVendorVFIO || stored.GPUMdevUUID != "" || stored.GPUProfile != vgpuSnapshotProfile || stored.SkipGuestAgent {
		return fmt.Errorf("%w: vGPU snapshots require the opt-in managed QEMU runtime and a vendor VFIO %s", ErrInvalidState, vgpuSnapshotProfile)
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
