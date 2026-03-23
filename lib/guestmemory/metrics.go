package guestmemory

import (
	"context"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type Metrics struct {
	reconcileTotal           metric.Int64Counter
	reconcileDuration        metric.Float64Histogram
	reclaimActionsTotal      metric.Int64Counter
	pressureTransitionsTotal metric.Int64Counter
	samplerErrorsTotal       metric.Int64Counter
	reclaimBytes             metric.Int64Histogram

	hostAvailableBytes  metric.Int64Gauge
	targetReclaimBytes  metric.Int64Gauge
	appliedReclaimBytes metric.Int64Gauge
	manualHoldActive    metric.Int64Gauge
	eligibleVMsTotal    metric.Int64Gauge
	pressureState       metric.Int64Gauge
}

type GaugeObservation struct {
	HostAvailableBytes int64
	AutoTargetBytes    int64
	ManualTargetBytes  int64
	EffectiveTarget    int64
	AppliedReclaim     int64
	EligibleVMs        int
	PressureState      HostPressureState
	ManualHoldActive   bool
}

func NewMetrics(meter metric.Meter) (*Metrics, error) {
	if meter == nil {
		return nil, nil
	}

	reconcileTotal, err := meter.Int64Counter(
		"hypeman_guestmemory_reconcile_total",
		metric.WithDescription("Total number of guest memory reconcile cycles"),
	)
	if err != nil {
		return nil, err
	}

	reconcileDuration, err := meter.Float64Histogram(
		"hypeman_guestmemory_reconcile_duration_seconds",
		metric.WithDescription("Guest memory reconcile duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	reclaimActionsTotal, err := meter.Int64Counter(
		"hypeman_guestmemory_reclaim_actions_total",
		metric.WithDescription("Total number of guest memory reclaim actions"),
	)
	if err != nil {
		return nil, err
	}

	pressureTransitionsTotal, err := meter.Int64Counter(
		"hypeman_guestmemory_pressure_transitions_total",
		metric.WithDescription("Total number of guest memory pressure state transitions"),
	)
	if err != nil {
		return nil, err
	}

	samplerErrorsTotal, err := meter.Int64Counter(
		"hypeman_guestmemory_sampler_errors_total",
		metric.WithDescription("Total number of guest memory host pressure sampler errors"),
	)
	if err != nil {
		return nil, err
	}

	reclaimBytes, err := meter.Int64Histogram(
		"hypeman_guestmemory_reclaim_bytes",
		metric.WithDescription("Guest memory reclaim bytes observed per reconcile"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	hostAvailableBytes, err := meter.Int64Gauge(
		"hypeman_guestmemory_host_available_bytes",
		metric.WithDescription("Last observed host available memory"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	targetReclaimBytes, err := meter.Int64Gauge(
		"hypeman_guestmemory_target_reclaim_bytes",
		metric.WithDescription("Current guest memory reclaim target"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	appliedReclaimBytes, err := meter.Int64Gauge(
		"hypeman_guestmemory_applied_reclaim_bytes",
		metric.WithDescription("Current applied guest memory reclaim"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	manualHoldActive, err := meter.Int64Gauge(
		"hypeman_guestmemory_manual_hold_active",
		metric.WithDescription("Whether a manual guest memory reclaim hold is active"),
	)
	if err != nil {
		return nil, err
	}

	eligibleVMsTotal, err := meter.Int64Gauge(
		"hypeman_guestmemory_eligible_vms_total",
		metric.WithDescription("Number of guest VMs eligible for active ballooning"),
	)
	if err != nil {
		return nil, err
	}

	pressureState, err := meter.Int64Gauge(
		"hypeman_guestmemory_pressure_state",
		metric.WithDescription("Current guest memory host pressure state (0 healthy, 1 pressure)"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		reconcileTotal:           reconcileTotal,
		reconcileDuration:        reconcileDuration,
		reclaimActionsTotal:      reclaimActionsTotal,
		pressureTransitionsTotal: pressureTransitionsTotal,
		samplerErrorsTotal:       samplerErrorsTotal,
		reclaimBytes:             reclaimBytes,
		hostAvailableBytes:       hostAvailableBytes,
		targetReclaimBytes:       targetReclaimBytes,
		appliedReclaimBytes:      appliedReclaimBytes,
		manualHoldActive:         manualHoldActive,
		eligibleVMsTotal:         eligibleVMsTotal,
		pressureState:            pressureState,
	}, nil
}

func (m *Metrics) RecordReconcile(ctx context.Context, trigger, status string, duration time.Duration) {
	if m == nil {
		return
	}

	opts := metric.WithAttributes(
		attribute.String("trigger", trigger),
		attribute.String("status", status),
	)
	m.reconcileTotal.Add(ctx, 1, opts)
	m.reconcileDuration.Record(ctx, duration.Seconds(), opts)
}

func (m *Metrics) RecordReclaimAction(ctx context.Context, trigger, status string, hvType hypervisor.Type) {
	if m == nil {
		return
	}

	m.reclaimActionsTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("trigger", trigger),
			attribute.String("status", status),
			attribute.String("hypervisor", string(hvType)),
		))
}

func (m *Metrics) RecordPressureTransition(ctx context.Context, from, to HostPressureState) {
	if m == nil {
		return
	}

	m.pressureTransitionsTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("from", string(from)),
			attribute.String("to", string(to)),
		))
}

func (m *Metrics) RecordSamplerError(ctx context.Context, sampler string) {
	if m == nil {
		return
	}

	m.samplerErrorsTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("sampler", sampler)))
}

func (m *Metrics) RecordReclaimBytes(ctx context.Context, trigger, kind string, bytes int64) {
	if m == nil || bytes < 0 {
		return
	}

	m.reclaimBytes.Record(ctx, bytes,
		metric.WithAttributes(
			attribute.String("trigger", trigger),
			attribute.String("kind", kind),
		))
}

func (m *Metrics) RecordGaugeState(ctx context.Context, obs GaugeObservation) {
	if m == nil {
		return
	}

	m.hostAvailableBytes.Record(ctx, obs.HostAvailableBytes)
	m.targetReclaimBytes.Record(ctx, obs.AutoTargetBytes, metric.WithAttributes(attribute.String("source", "auto")))
	m.targetReclaimBytes.Record(ctx, obs.ManualTargetBytes, metric.WithAttributes(attribute.String("source", "manual")))
	m.targetReclaimBytes.Record(ctx, obs.EffectiveTarget, metric.WithAttributes(attribute.String("source", "effective")))
	m.appliedReclaimBytes.Record(ctx, obs.AppliedReclaim)
	m.manualHoldActive.Record(ctx, boolToInt64(obs.ManualHoldActive))
	m.eligibleVMsTotal.Record(ctx, int64(obs.EligibleVMs))
	m.pressureState.Record(ctx, pressureStateMetricValue(obs.PressureState))
}

func pressureStateMetricValue(state HostPressureState) int64 {
	if state == HostPressureStatePressure {
		return 1
	}
	return 0
}

func boolToInt64(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
