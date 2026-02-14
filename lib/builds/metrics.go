package builds

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics provides Prometheus metrics for the build system
type Metrics struct {
	buildDuration metric.Float64Histogram
	buildTotal    metric.Int64Counter
	queueLength   metric.Int64ObservableGauge
	activeBuilds  metric.Int64ObservableGauge
}

// NewMetrics creates a new Metrics instance
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	buildDuration, err := meter.Float64Histogram(
		"hypeman_build_duration_seconds",
		metric.WithDescription("Duration of builds in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	buildTotal, err := meter.Int64Counter(
		"hypeman_builds_total",
		metric.WithDescription("Total number of builds"),
	)
	if err != nil {
		return nil, err
	}

	queueLength, err := meter.Int64ObservableGauge(
		"hypeman_build_queue_length",
		metric.WithDescription("Number of builds in queue"),
	)
	if err != nil {
		return nil, err
	}

	activeBuilds, err := meter.Int64ObservableGauge(
		"hypeman_builds_active",
		metric.WithDescription("Number of currently running builds"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		buildDuration: buildDuration,
		buildTotal:    buildTotal,
		queueLength:   queueLength,
		activeBuilds:  activeBuilds,
	}, nil
}

// RecordBuild records metrics for a completed build
func (m *Metrics) RecordBuild(ctx context.Context, status string, duration time.Duration) {
	attrs := []attribute.KeyValue{
		attribute.String("status", status),
	}

	m.buildDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	m.buildTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// RegisterQueueCallbacks registers callbacks for queue metrics
func (m *Metrics) RegisterQueueCallbacks(queue *BuildQueue, meter metric.Meter) error {
	_, err := meter.RegisterCallback(
		func(ctx context.Context, observer metric.Observer) error {
			observer.ObserveInt64(m.queueLength, int64(queue.PendingCount()))
			observer.ObserveInt64(m.activeBuilds, int64(queue.ActiveCount()))
			return nil
		},
		m.queueLength,
		m.activeBuilds,
	)
	return err
}

