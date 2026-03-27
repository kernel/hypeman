package imageretention

import (
	"context"
	"time"

	hypotel "github.com/kernel/hypeman/lib/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the OTel instruments for image retention.
type Metrics struct {
	sweepsTotal          metric.Int64Counter
	sweepDuration        metric.Float64Histogram
	deletesTotal         metric.Int64Counter
	staleReferencesTotal metric.Int64Counter
	pendingImages        metric.Int64ObservableGauge
}

func newMetrics(meter metric.Meter, c *Controller) (*Metrics, error) {
	sweepsTotal, err := meter.Int64Counter(
		"hypeman_image_retention_sweeps_total",
		metric.WithDescription("Total number of image retention sweeps"),
	)
	if err != nil {
		return nil, err
	}

	sweepDuration, err := meter.Float64Histogram(
		"hypeman_image_retention_sweep_duration_seconds",
		metric.WithDescription("Duration of image retention sweeps"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	deletesTotal, err := meter.Int64Counter(
		"hypeman_image_retention_deletes_total",
		metric.WithDescription("Total number of image retention delete attempts"),
	)
	if err != nil {
		return nil, err
	}

	staleReferencesTotal, err := meter.Int64Counter(
		"hypeman_image_retention_stale_references_total",
		metric.WithDescription("Total number of stale image references skipped during retention sweeps"),
	)
	if err != nil {
		return nil, err
	}

	pendingImages, err := meter.Int64ObservableGauge(
		"hypeman_image_retention_pending_images",
		metric.WithDescription("Number of images currently tracked by retention state"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			states, now, err := c.listStatesSnapshot()
			if err != nil {
				return nil
			}

			var tracked int64
			var expired int64
			for _, state := range states {
				tracked++
				if !state.UnusedSince.IsZero() && !state.UnusedSince.UTC().Add(c.unusedFor).After(now) {
					expired++
				}
			}

			o.ObserveInt64(pendingImages, tracked, metric.WithAttributes(attribute.String("state", "tracked")))
			o.ObserveInt64(pendingImages, expired, metric.WithAttributes(attribute.String("state", "expired")))
			return nil
		},
		pendingImages,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		sweepsTotal:          sweepsTotal,
		sweepDuration:        sweepDuration,
		deletesTotal:         deletesTotal,
		staleReferencesTotal: staleReferencesTotal,
		pendingImages:        pendingImages,
	}, nil
}

// RecordSweep records the outcome and duration of a retention sweep.
func (m *Metrics) RecordSweep(ctx context.Context, status string, duration time.Duration, staleReferences int) {
	if m == nil {
		return
	}

	attrs := metric.WithAttributes(attribute.String("status", status))
	m.sweepsTotal.Add(ctx, 1, attrs)
	m.sweepDuration.Record(ctx, duration.Seconds(), attrs)
	if staleReferences > 0 {
		m.staleReferencesTotal.Add(ctx, int64(staleReferences))
	}
}

// RecordDelete records the outcome of an image delete attempt.
func (m *Metrics) RecordDelete(ctx context.Context, status string) {
	if m == nil {
		return
	}

	m.deletesTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
}
