package images

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for image operations.
type Metrics struct {
	buildDuration metric.Float64Histogram
	pullsTotal    metric.Int64Counter
}

// newMetrics creates and registers all image metrics.
func newMetrics(meter metric.Meter, m *manager) (*Metrics, error) {
	buildDuration, err := meter.Float64Histogram(
		"hypeman_images_build_duration_seconds",
		metric.WithDescription("Time to build an image"),
		metric.WithUnit("s"),
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
			metas, err := listAllTags(m.paths)
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
		buildDuration: buildDuration,
		pullsTotal:    pullsTotal,
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
