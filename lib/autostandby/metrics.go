package autostandby

import (
	"context"
	"time"

	hypotel "github.com/kernel/hypeman/lib/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Metrics struct {
	conntrackEventsTotal   metric.Int64Counter
	startupResyncDuration  metric.Float64Histogram
	standbyAttemptsTotal   metric.Int64Counter
	controllerErrorsTotal  metric.Int64Counter
	trackedInstancesGauge  metric.Int64ObservableGauge
	activeConnectionsGauge metric.Int64ObservableGauge
	tracer                 trace.Tracer
}

func newMetrics(meter metric.Meter, tracer trace.Tracer, controller *Controller) *Metrics {
	if meter == nil {
		return &Metrics{tracer: tracer}
	}

	conntrackEventsTotal, err := meter.Int64Counter(
		"hypeman_auto_standby_conntrack_events_total",
		metric.WithDescription("Total conntrack events processed by the auto-standby controller"),
	)
	if err != nil {
		return &Metrics{tracer: tracer}
	}
	startupResyncDuration, err := meter.Float64Histogram(
		"hypeman_auto_standby_startup_resync_duration_seconds",
		metric.WithDescription("Time spent rebuilding auto-standby state during controller startup"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return &Metrics{tracer: tracer}
	}
	standbyAttemptsTotal, err := meter.Int64Counter(
		"hypeman_auto_standby_standby_attempts_total",
		metric.WithDescription("Total standby attempts issued by the auto-standby controller"),
	)
	if err != nil {
		return &Metrics{tracer: tracer}
	}
	controllerErrorsTotal, err := meter.Int64Counter(
		"hypeman_auto_standby_controller_errors_total",
		metric.WithDescription("Total controller and observer errors encountered by auto-standby"),
	)
	if err != nil {
		return &Metrics{tracer: tracer}
	}
	trackedInstancesGauge, err := meter.Int64ObservableGauge(
		"hypeman_auto_standby_tracked_instances_total",
		metric.WithDescription("Tracked instances by auto-standby controller phase"),
	)
	if err != nil {
		return &Metrics{tracer: tracer}
	}
	activeConnectionsGauge, err := meter.Int64ObservableGauge(
		"hypeman_auto_standby_active_connections_total",
		metric.WithDescription("Current number of active inbound connections tracked by auto-standby"),
	)
	if err != nil {
		return &Metrics{tracer: tracer}
	}

	m := &Metrics{
		conntrackEventsTotal:   conntrackEventsTotal,
		startupResyncDuration:  startupResyncDuration,
		standbyAttemptsTotal:   standbyAttemptsTotal,
		controllerErrorsTotal:  controllerErrorsTotal,
		trackedInstancesGauge:  trackedInstancesGauge,
		activeConnectionsGauge: activeConnectionsGauge,
		tracer:                 tracer,
	}

	_, _ = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		if controller == nil {
			return nil
		}
		active, countdown, ready, ineligible, totalConnections := controller.metricSnapshot()
		observer.ObserveInt64(m.trackedInstancesGauge, int64(active), metric.WithAttributes(attribute.String("phase", "active")))
		observer.ObserveInt64(m.trackedInstancesGauge, int64(countdown), metric.WithAttributes(attribute.String("phase", "idle_countdown")))
		observer.ObserveInt64(m.trackedInstancesGauge, int64(ready), metric.WithAttributes(attribute.String("phase", "ready_for_standby")))
		observer.ObserveInt64(m.trackedInstancesGauge, int64(ineligible), metric.WithAttributes(attribute.String("phase", "ineligible")))
		observer.ObserveInt64(m.activeConnectionsGauge, int64(totalConnections))
		return nil
	}, trackedInstancesGauge, activeConnectionsGauge)

	return m
}

func (c *Controller) recordConntrackEvent(event, result string) {
	if c.metrics == nil || c.metrics.conntrackEventsTotal == nil {
		return
	}
	c.metrics.conntrackEventsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("event", event),
		attribute.String("result", result),
	))
}

func (c *Controller) recordStartupResync(start time.Time, status string) {
	if c.metrics == nil || c.metrics.startupResyncDuration == nil {
		return
	}
	c.metrics.startupResyncDuration.Record(context.Background(), time.Since(start).Seconds(), metric.WithAttributes(
		attribute.String("status", status),
	))
}

func (c *Controller) recordStandbyAttempt(status string) {
	if c.metrics == nil || c.metrics.standbyAttemptsTotal == nil {
		return
	}
	c.metrics.standbyAttemptsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("status", status),
	))
}

func (c *Controller) recordControllerError(operation string) {
	if c.metrics == nil || c.metrics.controllerErrorsTotal == nil {
		return
	}
	c.metrics.controllerErrorsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("operation", operation),
	))
}

func (c *Controller) recordObserverError(operation string) {
	c.recordControllerError(operation)
}

func (c *Controller) metricSnapshot() (active, countdown, ready, ineligible, totalConnections int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := c.now().UTC()
	for _, state := range c.states {
		if state.compiledPolicy == nil {
			ineligible++
			continue
		}
		totalConnections += len(state.activeInbound)
		switch {
		case state.standbyRequested:
			ready++
		case len(state.activeInbound) > 0:
			active++
		case state.nextStandbyAt != nil && state.nextStandbyAt.After(now):
			countdown++
		default:
			ready++
		}
	}
	return
}
