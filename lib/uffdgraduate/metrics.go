package uffdgraduate

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type Metrics struct {
	attemptsTotal metric.Int64Counter
	errorsTotal   metric.Int64Counter
	inFlightGauge metric.Int64ObservableGauge
	trackedGauge  metric.Int64ObservableGauge
}

func newMetrics(meter metric.Meter, controller *Controller) *Metrics {
	if meter == nil {
		return &Metrics{}
	}

	attemptsTotal, err := meter.Int64Counter(
		"hypeman_uffd_graduation_attempts_total",
		metric.WithDescription("Total UFFD pager graduation attempts"),
	)
	if err != nil {
		return &Metrics{}
	}
	errorsTotal, err := meter.Int64Counter(
		"hypeman_uffd_graduation_errors_total",
		metric.WithDescription("Total UFFD graduation controller errors"),
	)
	if err != nil {
		return &Metrics{}
	}
	inFlightGauge, err := meter.Int64ObservableGauge(
		"hypeman_uffd_graduation_in_flight",
		metric.WithDescription("Graduations currently in flight"),
	)
	if err != nil {
		return &Metrics{}
	}
	trackedGauge, err := meter.Int64ObservableGauge(
		"hypeman_uffd_graduation_tracked_sessions",
		metric.WithDescription("Pager-backed sessions currently tracked by the graduation controller"),
	)
	if err != nil {
		return &Metrics{}
	}

	m := &Metrics{
		attemptsTotal: attemptsTotal,
		errorsTotal:   errorsTotal,
		inFlightGauge: inFlightGauge,
		trackedGauge:  trackedGauge,
	}

	_, _ = meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		if controller == nil {
			return nil
		}
		observer.ObserveInt64(m.inFlightGauge, int64(controller.inFlightCount()))
		observer.ObserveInt64(m.trackedGauge, int64(controller.trackedSessions()))
		return nil
	}, inFlightGauge, trackedGauge)

	return m
}

func (c *Controller) recordAttempt(status string) {
	if c.metrics == nil || c.metrics.attemptsTotal == nil {
		return
	}
	c.metrics.attemptsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("status", status),
	))
}

func (c *Controller) recordError(operation string) {
	if c.metrics == nil || c.metrics.errorsTotal == nil {
		return
	}
	c.metrics.errorsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("operation", operation),
	))
}
