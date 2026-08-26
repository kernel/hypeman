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
// reconciliation entirely. A discovery failure starts the reconciler anyway:
// a transient sysfs error must not disable cleanup on a GPU host.
func (m *manager) StartVGPUReconciler(ctx context.Context) {
	discover := m.discoverVGPU
	if discover == nil {
		discover = devices.DiscoverVGPU
	}
	framework, _, err := discover()
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

// ReconcileVGPUs runs one fail-closed reconcile pass: stale instance-held
// assignments are released, then device-level leftovers not claimed by a live
// instance are swept. Failures only log; the next periodic pass retries.
func (m *manager) ReconcileVGPUs(ctx context.Context) {
	log := logger.FromContext(ctx)
	protected, err := m.reconcileVGPUAssignments(ctx)
	sweepVendorVFIO := err == nil
	if err != nil {
		m.recordVGPUReconcileFailure(ctx, vgpuReconcileStageListInstances)
		log.ErrorContext(ctx, "failed to list instances for vGPU reconcile protection; skipping vendor VFIO sweep until the next pass, mdev reconcile still runs", "error", err)
		protected = make(map[string]struct{})
	}
	reconcileDevices := m.reconcileVGPUDevices
	if reconcileDevices == nil {
		reconcileDevices = devices.ReconcileVGPUs
	}
	if err := reconcileDevices(ctx, protected, sweepVendorVFIO); err != nil {
		m.recordVGPUReconcileFailure(ctx, vgpuReconcileStageReconcileDevices)
		log.WarnContext(ctx, "failed to reconcile vGPU devices", "error", err)
	}
}

// reconcileVGPUAssignments retries releases for assignments whose owner is no
// longer live and returns the device paths still protected by live instances.
// Listing fails closed: any unreadable metadata aborts the pass so the vendor
// VFIO sweep cannot clear a VF whose claim it could not read.
func (m *manager) reconcileVGPUAssignments(ctx context.Context) (map[string]struct{}, error) {
	allInstances, err := m.listInstancesForReconcile(ctx)
	if err != nil {
		return nil, err
	}
	protected := make(map[string]struct{})
	for i := range allInstances {
		stored := &allInstances[i].StoredMetadata
		if storedVGPUDevicePath(stored) == "" {
			continue
		}
		// The socket-ownership check runs even without a persisted PID: a VMM
		// whose post-boot metadata save failed still holds its control-socket
		// listener, and releasing its device would tear the vGPU out from
		// under a live VM.
		livePID := hypervisorMayBeAlive(stored.HypervisorProcessIdentity, stored.SocketPath)
		if live, _ := vgpuAssignmentLiveness(stored, m.nowUTC(), livePID); live {
			if stored.GPUDevicePath != "" {
				protected[stored.GPUDevicePath] = struct{}{}
			}
			continue
		}
		m.releaseStaleVGPUAssignment(ctx, stored.Id)
	}
	return protected, nil
}

// releaseStaleVGPUAssignment retries a release that previously failed, under
// the instance lock. Liveness is re-verified after locking so a concurrent
// start or restore keeps its assignment. A failed release only logs and keeps
// the metadata for the next pass.
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
	livePID := hypervisorMayBeAlive(stored.HypervisorProcessIdentity, stored.SocketPath)
	if live, _ := vgpuAssignmentLiveness(stored, m.nowUTC(), livePID); live {
		return
	}
	if err := m.releaseStoredVGPU(ctx, stored); err != nil {
		m.recordVGPUStaleReleaseFailure(ctx)
		log.WarnContext(ctx, "failed to release stale vGPU assignment; retrying on the next reconcile pass", "instance_id", id, "device_path", path, "error", err)
		return
	}
	if err := m.saveMetadata(meta); err != nil {
		log.WarnContext(ctx, "failed to save metadata after stale vGPU release", "instance_id", id, "error", err)
		return
	}
	log.InfoContext(ctx, "released stale vGPU assignment", "instance_id", id, "device_path", path)
}
