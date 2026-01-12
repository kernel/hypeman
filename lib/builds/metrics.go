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

	return &Metrics{
		buildDuration: buildDuration,
		buildTotal:    buildTotal,
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
