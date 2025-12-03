package volumes

import (
	"context"
	"os"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for volume operations.
type Metrics struct {
	createDuration metric.Float64Histogram
}

// newVolumeMetrics creates and registers all volume metrics.
func newVolumeMetrics(meter metric.Meter, m *manager) (*Metrics, error) {
	createDuration, err := meter.Float64Histogram(
		"hypeman_volumes_create_duration_seconds",
		metric.WithDescription("Time to create a volume"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	// Register observable gauges
	volumesTotal, err := meter.Int64ObservableGauge(
		"hypeman_volumes_total",
		metric.WithDescription("Total number of volumes"),
	)
	if err != nil {
		return nil, err
	}

	allocatedBytes, err := meter.Int64ObservableGauge(
		"hypeman_volumes_allocated_bytes",
		metric.WithDescription("Total allocated/provisioned volume size in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	usedBytes, err := meter.Int64ObservableGauge(
		"hypeman_volumes_used_bytes",
		metric.WithDescription("Actual disk space consumed by volumes in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			volumes, err := m.ListVolumes(ctx)
			if err != nil {
				return nil
			}

			o.ObserveInt64(volumesTotal, int64(len(volumes)))

			var totalAllocated, totalUsed int64
			for _, vol := range volumes {
				// Allocated = provisioned size in GB
				totalAllocated += int64(vol.SizeGb) * 1024 * 1024 * 1024

				// Used = actual disk blocks consumed (for sparse files)
				diskPath := m.paths.VolumeData(vol.Id)
				if stat, err := os.Stat(diskPath); err == nil {
					// Get actual blocks used via syscall
					if sysStat, ok := stat.Sys().(*syscall.Stat_t); ok {
						totalUsed += sysStat.Blocks * 512 // Blocks are in 512-byte units
					}
				}
			}

			o.ObserveInt64(allocatedBytes, totalAllocated)
			o.ObserveInt64(usedBytes, totalUsed)
			return nil
		},
		volumesTotal,
		allocatedBytes,
		usedBytes,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		createDuration: createDuration,
	}, nil
}

// recordCreateDuration records the volume creation duration.
func (m *manager) recordCreateDuration(ctx context.Context, start time.Time, status string) {
	if m.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	m.metrics.createDuration.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))
}
