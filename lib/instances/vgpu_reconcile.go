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

// ReconcileVGPUs releases metadata claims whose VMM is confirmed dead. The
// device-level pass remains for mdevs; vendor VFIO leftovers are repaired only
// when the allocator claims that VF again.
func (m *manager) ReconcileVGPUs(ctx context.Context) {
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
	if err := reconcileDevices(ctx, protected, true); err != nil {
		m.recordVGPUReconcileFailure(ctx, vgpuReconcileStageReconcileDevices)
		log.WarnContext(ctx, "failed to reconcile mdev devices", "error", err)
	}
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
		pid, err := resolveLiveHypervisorPID(stored.HypervisorProcessIdentity, stored.SocketPath)
		if err != nil || pid > 0 {
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
	pid, err := resolveLiveHypervisorPID(stored.HypervisorProcessIdentity, stored.SocketPath)
	if err != nil || pid > 0 {
		return
	}
	if err := m.releaseStoredVGPUPersisted(ctx, meta); err != nil {
		m.recordVGPUStaleReleaseFailure(ctx)
		log.WarnContext(ctx, "failed to release stale vGPU assignment; retrying on the next reconcile pass", "instance_id", id, "device_path", path, "error", err)
		return
	}
	log.InfoContext(ctx, "released stale vGPU assignment", "instance_id", id, "device_path", path)
}
