package guestmemory

import (
	"context"
	"log/slog"
	"time"

	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type reconcileSummary struct {
	eligibleVMs       int
	appliedCount      int
	plannedCount      int
	unchangedCount    int
	errorCount        int
	unsupportedCount  int
	plannedReclaim    int64
	appliedReclaim    int64
	effectiveTarget   int64
	autoTarget        int64
	manualTarget      int64
	manualHoldActive  bool
	pressureChanged   bool
	previousPressure  HostPressureState
	currentPressure   HostPressureState
	hostAvailable     int64
	hostAvailablePerc float64
}

func reconcileTrigger(req reconcileRequest) string {
	if req.force {
		return "manual"
	}
	return "auto"
}

func logFromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if log := logger.FromContext(ctx); log != nil && log != slog.Default() {
		return log
	}
	if fallback != nil {
		return fallback
	}
	return slog.Default()
}

func (c *controller) startReconcileSpan(ctx context.Context, req reconcileRequest) (context.Context, trace.Span) {
	return c.tracer.Start(ctx, "guestmemory.reconcile",
		trace.WithAttributes(
			attribute.String("trigger", reconcileTrigger(req)),
			attribute.Bool("force", req.force),
			attribute.Bool("dry_run", req.dryRun),
			attribute.Int64("requested_reclaim_bytes", req.requestedReclaim),
		))
}

func (c *controller) startChildSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return c.tracer.Start(ctx, name)
}

func reconcileStatus(summary reconcileSummary) string {
	if summary.errorCount > 0 {
		if summary.appliedCount > 0 || summary.unchangedCount > 0 || summary.plannedCount > 0 {
			return "partial"
		}
		return "error"
	}
	return "success"
}

func (c *controller) recordReconcileError(ctx context.Context, trigger string, start time.Time, span trace.Span, err error) {
	if err == nil {
		return
	}

	c.metrics.RecordReconcile(ctx, trigger, "error", time.Since(start))
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func (c *controller) recordReconcileSuccess(ctx context.Context, trigger string, req reconcileRequest, span trace.Span, start time.Time, summary reconcileSummary, actions []ManualReclaimAction) {
	status := reconcileStatus(summary)
	c.metrics.RecordReconcile(ctx, trigger, status, time.Since(start))
	for _, action := range actions {
		c.metrics.RecordReclaimAction(ctx, trigger, action.Status, action.Hypervisor)
	}

	if !req.dryRun {
		c.metrics.RecordReclaimBytes(ctx, trigger, "auto_target", summary.autoTarget)
		c.metrics.RecordReclaimBytes(ctx, trigger, "manual_target", summary.manualTarget)
		c.metrics.RecordReclaimBytes(ctx, trigger, "effective_target", summary.effectiveTarget)
		c.metrics.RecordReclaimBytes(ctx, trigger, "planned", summary.plannedReclaim)
		c.metrics.RecordReclaimBytes(ctx, trigger, "applied", summary.appliedReclaim)
		c.metrics.RecordGaugeState(ctx, GaugeObservation{
			HostAvailableBytes: summary.hostAvailable,
			AutoTargetBytes:    summary.autoTarget,
			ManualTargetBytes:  summary.manualTarget,
			EffectiveTarget:    summary.effectiveTarget,
			AppliedReclaim:     summary.appliedReclaim,
			EligibleVMs:        summary.eligibleVMs,
			PressureState:      summary.currentPressure,
			ManualHoldActive:   summary.manualHoldActive,
		})
	}

	span.SetAttributes(
		attribute.String("status", status),
		attribute.Int("eligible_vms", summary.eligibleVMs),
		attribute.Int("applied_vms", summary.appliedCount),
		attribute.Int("planned_vms", summary.plannedCount),
		attribute.Int("error_vms", summary.errorCount),
		attribute.Int("unsupported_vms", summary.unsupportedCount),
		attribute.Int64("auto_target_reclaim_bytes", summary.autoTarget),
		attribute.Int64("manual_target_reclaim_bytes", summary.manualTarget),
		attribute.Int64("effective_target_reclaim_bytes", summary.effectiveTarget),
		attribute.Int64("planned_reclaim_bytes", summary.plannedReclaim),
		attribute.Int64("applied_reclaim_bytes", summary.appliedReclaim),
		attribute.Int64("host_available_bytes", summary.hostAvailable),
		attribute.Float64("host_available_percent", summary.hostAvailablePerc),
		attribute.String("pressure_state", string(summary.currentPressure)),
		attribute.Bool("manual_hold_active", summary.manualHoldActive),
	)
	span.SetStatus(codes.Ok, "")
}

func (c *controller) logPressureTransition(ctx context.Context, summary reconcileSummary) {
	if !summary.pressureChanged {
		return
	}

	logFromContext(ctx, c.log).InfoContext(ctx,
		"guest memory pressure state changed",
		"operation", "active_ballooning_reconcile",
		"from", summary.previousPressure,
		"to", summary.currentPressure,
		"host_available_bytes", summary.hostAvailable,
		"host_available_percent", summary.hostAvailablePerc,
	)
}

func (c *controller) logReconcileSummary(ctx context.Context, req reconcileRequest, summary reconcileSummary, status string) {
	if !req.force && !summary.pressureChanged && summary.appliedCount == 0 && summary.errorCount == 0 {
		return
	}

	logFromContext(ctx, c.log).InfoContext(ctx,
		"guest memory reconcile completed",
		"operation", "active_ballooning_reconcile",
		"trigger", reconcileTrigger(req),
		"dry_run", req.dryRun,
		"status", status,
		"eligible_vms", summary.eligibleVMs,
		"applied_vms", summary.appliedCount,
		"planned_vms", summary.plannedCount,
		"error_vms", summary.errorCount,
		"unsupported_vms", summary.unsupportedCount,
		"host_available_bytes", summary.hostAvailable,
		"host_available_percent", summary.hostAvailablePerc,
		"pressure_state", summary.currentPressure,
		"planned_reclaim_bytes", summary.plannedReclaim,
		"applied_reclaim_bytes", summary.appliedReclaim,
		"manual_hold_active", summary.manualHoldActive,
	)
}
