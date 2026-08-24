package instances

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/logger"
)

func (m *manager) liveVGPUReconcileProtection(ctx context.Context) (map[string]struct{}, time.Duration, error) {
	allInstances, err := m.ListInstancesForReconcile(ctx)
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
		livePID := stored.HypervisorPID != nil && HypervisorMayBeAlive(stored.HypervisorProcessIdentity, stored.SocketPath)
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
		retryAfter = 0
	}
	if err := devices.ReconcileVGPUs(ctx, protected, sweepVendorVFIO); err != nil {
		log.WarnContext(ctx, "failed to reconcile vGPU devices", "error", err)
	}
	if retryAfter <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(retryAfter)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			m.ReconcileVGPUs(ctx)
		}
	}()
}
