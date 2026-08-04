package builders

import (
	"context"
	"time"

	hypotel "github.com/kernel/hypeman/lib/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for builder operations.
type Metrics struct {
	createDuration metric.Float64Histogram
}

// newBuilderMetrics creates and registers all builder metrics.
func newBuilderMetrics(meter metric.Meter, m *manager) (*Metrics, error) {
	createDuration, err := meter.Float64Histogram(
		"hypeman_builders_create_duration_seconds",
		metric.WithDescription("Time to create a builder"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	buildersTotal, err := meter.Int64ObservableGauge(
		"hypeman_builders_total",
		metric.WithDescription("Total number of builders"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			builders, err := m.ListBuilders(ctx)
			if err != nil {
				return nil
			}
			counts := map[string]int64{
				StatusReady:    0,
				StatusPruning:  0,
				StatusDeleting: 0,
				StatusError:    0,
			}
			for _, builder := range builders {
				counts[builder.Status]++
			}
			for status, count := range counts {
				o.ObserveInt64(buildersTotal, count,
					metric.WithAttributes(attribute.String("status", status)))
			}
			return nil
		},
		buildersTotal,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		createDuration: createDuration,
	}, nil
}

// recordCreateDuration records the builder creation duration.
func (m *manager) recordCreateDuration(ctx context.Context, start time.Time, status string) {
	if m.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	m.metrics.createDuration.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))
}
