package instances

import (
	"context"
	"errors"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

const defaultVGPUReconcileInterval = time.Minute

// StartVGPUReconciler runs one reconcile pass and then keeps reconciling
// periodically until ctx is cancelled. Hosts without a vGPU framework skip
// reconciliation entirely. A discovery failure starts the reconciler anyway.
func (m *manager) StartVGPUReconciler(ctx context.Context) {
	framework, _, err := m.discoverVGPUDevices()
	if err == nil && framework == devices.VGPUFrameworkNone {
		return
	}
	if err != nil {
		logger.FromContext(ctx).WarnContext(ctx, "failed to discover vGPU framework; starting vGPU reconciler anyway", "error", err)
	}
	m.ReconcileVGPUs(ctx)
	m.vgpuReconcileOnce.Do(func() {
		interval := m.vgpuReconcileInterval
		if interval <= 0 {
			interval = defaultVGPUReconcileInterval
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.ReconcileVGPUs(ctx)
				}
			}
		}()
	})
}

// ReconcileVGPUs releases metadata claims whose VMM is confirmed dead, sweeps
// orphaned mdevs, resets vendor VFIO VFs no instance claims, and reloads the
// vendor VFIO placements /resources counts from.
func (m *manager) ReconcileVGPUs(ctx context.Context) {
	m.reconcileVGPUClaims(ctx)
	if !m.resetUnclaimedVendorVFs(ctx) {
		return
	}
	refresh := m.refreshVGPUCapacity
	if refresh == nil {
		refresh = devices.RefreshVGPUCapacity
	}
	refresh()
}

func (m *manager) reconcileVGPUClaims(ctx context.Context) {
	log := logger.FromContext(ctx)
	protected, err := m.reconcileVGPUAssignments(ctx)
	if err != nil {
		m.recordVGPUReconcileFailure(ctx, vgpuReconcileStageListInstances)
		log.ErrorContext(ctx, "failed to list instances for vGPU reconcile", "error", err)
		return
	}
	reconcileDevices := m.reconcileVGPUDevices
	if reconcileDevices == nil {
		reconcileDevices = devices.ReconcileVGPUs
	}
	if err := reconcileDevices(ctx, protected); err != nil {
		m.recordVGPUReconcileFailure(ctx, vgpuReconcileStageReconcileDevices)
		log.WarnContext(ctx, "failed to reconcile mdev devices", "error", err)
	}
}

// resetUnclaimedVendorVFs resets vendor VFIO VFs that are configured but
// claimed by no instance. A claim writes the type before it saves the claim,
// so a restart between the two, or a failed claim whose reset also failed,
// leaves such a VF holding a placement that /resources does not count. It
// runs on every pass, so a reset refused once, as while a VFIO handle is
// still open, is retried rather than waiting for the allocator to select
// that VF, which it may never do if placement skips the host. Reports
// whether the host has vendor VFIO VFs.
func (m *manager) resetUnclaimedVendorVFs(ctx context.Context) bool {
	m.vgpuAllocationMu.Lock()
	defer m.vgpuAllocationMu.Unlock()
	framework, vfs, err := m.discoverVGPUDevices()
	if err != nil || framework != devices.VGPUFrameworkVendorVFIO {
		return false
	}
	allMetadata, err := m.listMetadataForReconcile()
	if err != nil {
		m.recordVGPUReconcileFailure(ctx, vgpuReconcileStageResetUnclaimedVFs)
		logger.FromContext(ctx).WarnContext(ctx, "failed to list instances for unclaimed vGPU VF reset", "error", err)
		return true
	}
	if _, failed := m.resetDirtyUnclaimedVFs(ctx, vfs, allMetadata); failed {
		m.recordVGPUReconcileFailure(ctx, vgpuReconcileStageResetUnclaimedVFs)
	}
	return true
}

func (m *manager) reconcileVGPUAssignments(ctx context.Context) (map[string]struct{}, error) {
	allMetadata, err := m.listMetadataForReconcile()
	if err != nil {
		return nil, err
	}
	protected := make(map[string]struct{})
	for i := range allMetadata {
		stored := &allMetadata[i]
		devicePath := storedVGPUDevicePath(stored)
		if devicePath == "" {
			continue
		}
		if m.vgpuHypervisorMayBeAlive(ctx, stored) {
			protected[devicePath] = struct{}{}
			continue
		}
		m.releaseStaleVGPUAssignment(ctx, stored.Id)
	}
	return protected, nil
}

// releaseStaleVGPUAssignment rechecks the claim and VMM under the instance
// lock. Lifecycle operations take the instance lock before the allocation
// lock, so this path follows the same ordering.
func (m *manager) releaseStaleVGPUAssignment(ctx context.Context, id string) {
	lock := m.getInstanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	log := logger.FromContext(ctx)
	meta, err := m.loadMetadata(id)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			log.WarnContext(ctx, "failed to load metadata for stale vGPU release", "instance_id", id, "error", err)
		}
		return
	}
	stored := &meta.StoredMetadata
	path := storedVGPUDevicePath(stored)
	if path == "" {
		return
	}
	if m.vgpuHypervisorMayBeAlive(ctx, stored) {
		return
	}
	if err := m.releaseStoredVGPUPersisted(ctx, meta); err != nil {
		m.recordVGPUStaleReleaseFailure(ctx)
		log.WarnContext(ctx, "failed to release stale vGPU assignment; retrying on the next reconcile pass", "instance_id", id, "device_path", path, "error", err)
		return
	}
	log.InfoContext(ctx, "released stale vGPU assignment", "instance_id", id, "device_path", path)
}
