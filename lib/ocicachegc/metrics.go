package ocicachegc

import (
	"context"
	"time"

	hypotel "github.com/kernel/hypeman/lib/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the OTel instruments for the OCI cache collector.
type Metrics struct {
	sweepsTotal   metric.Int64Counter
	sweepDuration metric.Float64Histogram
	deletedBlobs  metric.Int64Counter
	deletedBytes  metric.Int64Counter
	liveBlobs     metric.Int64Histogram
}

func newMetrics(meter metric.Meter) (*Metrics, error) {
	sweepsTotal, err := meter.Int64Counter(
		"hypeman_oci_cache_gc_sweeps_total",
		metric.WithDescription("Total number of OCI cache GC sweeps"),
	)
	if err != nil {
		return nil, err
	}

	sweepDuration, err := meter.Float64Histogram(
		"hypeman_oci_cache_gc_sweep_duration_seconds",
		metric.WithDescription("Duration of OCI cache GC sweeps"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	deletedBlobs, err := meter.Int64Counter(
		"hypeman_oci_cache_gc_deleted_blobs_total",
		metric.WithDescription("Total number of blobs deleted by the OCI cache GC"),
	)
	if err != nil {
		return nil, err
	}

	deletedBytes, err := meter.Int64Counter(
		"hypeman_oci_cache_gc_deleted_bytes_total",
		metric.WithDescription("Total bytes freed by the OCI cache GC"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	liveBlobs, err := meter.Int64Histogram(
		"hypeman_oci_cache_gc_live_blobs",
		metric.WithDescription("Number of live blobs observed in the OCI cache per sweep"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		sweepsTotal:   sweepsTotal,
		sweepDuration: sweepDuration,
		deletedBlobs:  deletedBlobs,
		deletedBytes:  deletedBytes,
		liveBlobs:     liveBlobs,
	}, nil
}

// RecordSweep records the outcome of one sweep.
func (m *Metrics) RecordSweep(ctx context.Context, status string, duration time.Duration, stats Stats) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("status", status))
	m.sweepsTotal.Add(ctx, 1, attrs)
	m.sweepDuration.Record(ctx, duration.Seconds(), attrs)
	if status == "success" {
		m.liveBlobs.Record(ctx, int64(stats.LiveBlobs))
	}
	if stats.DeletedBlobs > 0 {
		m.deletedBlobs.Add(ctx, int64(stats.DeletedBlobs))
		m.deletedBytes.Add(ctx, stats.DeletedBytes)
	}
}
