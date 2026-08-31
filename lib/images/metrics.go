package images

import (
	"context"
	"time"

	hypotel "github.com/kernel/hypeman/lib/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for image operations.
type Metrics struct {
	buildDuration         metric.Float64Histogram
	buildPhaseDuration    metric.Float64Histogram
	ociLayerCount         metric.Int64Histogram
	ociCompressedBytes    metric.Int64Histogram
	pullsTotal            metric.Int64Counter
	layerArtifactsEvicted metric.Int64Counter
}

// newMetrics creates and registers all image metrics.
func newMetrics(meter metric.Meter, m *manager) (*Metrics, error) {
	buildDuration, err := meter.Float64Histogram(
		"hypeman_images_build_duration_seconds",
		metric.WithDescription("Time to build an image"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.BuildDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	buildPhaseDuration, err := meter.Float64Histogram(
		"hypeman_images_build_phase_duration_seconds",
		metric.WithDescription("Time spent in each image build phase"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.BuildDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	ociLayerCount, err := meter.Int64Histogram(
		"hypeman_images_oci_layer_count",
		metric.WithDescription("Number of layers in an OCI image being converted"),
		metric.WithUnit("{layer}"),
	)
	if err != nil {
		return nil, err
	}

	ociCompressedBytes, err := meter.Int64Histogram(
		"hypeman_images_oci_compressed_bytes",
		metric.WithDescription("Total compressed bytes in OCI image layers being converted"),
		metric.WithUnit("By"),
		metric.WithExplicitBucketBoundaries(
			1<<20,
			4<<20,
			16<<20,
			64<<20,
			256<<20,
			1<<30,
			2<<30,
			4<<30,
			8<<30,
			16<<30,
		),
	)
	if err != nil {
		return nil, err
	}

	pullsTotal, err := meter.Int64Counter(
		"hypeman_images_pulls_total",
		metric.WithDescription("Total number of image pulls from registries"),
	)
	if err != nil {
		return nil, err
	}

	layerArtifactsEvicted, err := meter.Int64Counter(
		"hypeman_images_layer_artifacts_evicted_total",
		metric.WithDescription("Total number of shared layer artifacts evicted after their last reference was removed"),
	)
	if err != nil {
		return nil, err
	}

	// Register observable gauges for queue length and total images
	buildQueueLength, err := meter.Int64ObservableGauge(
		"hypeman_images_build_queue_length",
		metric.WithDescription("Current number of images in the build queue"),
	)
	if err != nil {
		return nil, err
	}

	imagesTotal, err := meter.Int64ObservableGauge(
		"hypeman_images_total",
		metric.WithDescription("Total number of cached images"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			// Report queue length
			o.ObserveInt64(buildQueueLength, int64(m.queue.QueueLength()))

			// Count images by status
			metas, err := listAllMetadata(m.paths)
			if err != nil {
				return nil
			}
			statusCounts := make(map[string]int64)
			for _, meta := range metas {
				statusCounts[meta.Status]++
			}
			for status, count := range statusCounts {
				o.ObserveInt64(imagesTotal, count,
					metric.WithAttributes(attribute.String("status", status)))
			}
			return nil
		},
		buildQueueLength,
		imagesTotal,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		buildDuration:         buildDuration,
		buildPhaseDuration:    buildPhaseDuration,
		ociLayerCount:         ociLayerCount,
		ociCompressedBytes:    ociCompressedBytes,
		pullsTotal:            pullsTotal,
		layerArtifactsEvicted: layerArtifactsEvicted,
	}, nil
}

// recordBuildMetrics records the build duration metric.
func (m *manager) recordBuildMetrics(ctx context.Context, start time.Time, status string) {
	if m.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	m.metrics.buildDuration.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))
}

// recordPullMetrics records the pull counter metric.
func (m *manager) recordPullMetrics(ctx context.Context, status string) {
	if m.metrics == nil {
		return
	}
	m.metrics.pullsTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("status", status)))
}

func (m *manager) recordBuildPhaseMetrics(ctx context.Context, phase string, duration time.Duration, status, cacheStatus string) {
	if m.metrics == nil {
		return
	}
	m.metrics.buildPhaseDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(
		attribute.String("phase", phase),
		attribute.String("status", status),
		attribute.String("cache_status", cacheStatus),
	))
}

func (m *manager) recordOCIImageMetrics(ctx context.Context, layerCount int, compressedBytes int64, cacheStatus string) {
	if m.metrics == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("cache_status", cacheStatus))
	m.metrics.ociLayerCount.Record(ctx, int64(layerCount), attrs)
	m.metrics.ociCompressedBytes.Record(ctx, compressedBytes, attrs)
}
