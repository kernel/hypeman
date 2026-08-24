package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

// vgpuReconcileListRetryDelay spaces retries of the vendor VFIO sweep when
// the instance listing fails: without a retry, one transient stat error at
// startup would disable orphan recovery until the next restart.
const vgpuReconcileListRetryDelay = time.Minute

func (m *manager) liveVGPUReconcileProtection(ctx context.Context) (map[string]struct{}, time.Duration, error) {
	allInstances, err := m.listInstancesForReconcile(ctx)
	if err != nil {
		return nil, 0, err
	}
	protected := make(map[string]struct{})
	var retryAfter time.Duration
	for i := range allInstances {
		stored := &allInstances[i].StoredMetadata
		if stored.GPUDevicePath == "" {
			continue
		}
		livePID := stored.HypervisorPID != nil && hypervisorMayBeAlive(stored.HypervisorProcessIdentity, stored.SocketPath)
		live, remaining := vgpuAssignmentLiveness(stored, m.nowUTC(), livePID)
		if !live {
			continue
		}
		protected[stored.GPUDevicePath] = struct{}{}
		if remaining > 0 && (retryAfter == 0 || remaining < retryAfter) {
			retryAfter = remaining
		}
	}
	return protected, retryAfter, nil
}

// ReconcileVGPUs releases orphaned vGPU assignments.
func (m *manager) ReconcileVGPUs(ctx context.Context) {
	log := logger.FromContext(ctx)
	protected, retryAfter, err := m.liveVGPUReconcileProtection(ctx)
	sweepVendorVFIO := err == nil
	if err != nil {
		log.ErrorContext(ctx, "failed to list instances for vGPU reconcile protection; skipping vendor VFIO reconcile, mdev reconcile still runs", "error", err)
		protected = make(map[string]struct{})
		retryAfter = vgpuReconcileListRetryDelay
		if m.vgpuReconcileRetryDelay > 0 {
			retryAfter = m.vgpuReconcileRetryDelay
		}
	}
	if err := devices.ReconcileVGPUs(ctx, protected, sweepVendorVFIO); err != nil {
		log.WarnContext(ctx, "failed to reconcile vGPU devices", "error", err)
	}
	if retryAfter <= 0 {
		return
	}
	// One pending retry at a time: overlapping calls would fork parallel
	// retry chains.
	if !m.vgpuReconcileRetryPending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		timer := time.NewTimer(retryAfter)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			m.vgpuReconcileRetryPending.Store(false)
		case <-timer.C:
			m.vgpuReconcileRetryPending.Store(false)
			m.ReconcileVGPUs(ctx)
		}
	}()
}
