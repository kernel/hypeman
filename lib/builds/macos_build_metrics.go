package builds

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

func registerMachineStorageMetrics(meter metric.Meter, storage *MachineBuildStorage) error {
	active, err := meter.Int64ObservableGauge("hypeman_machine_build_reservations_active",
		metric.WithDescription("Active machine build storage reservations, excluding quarantines"))
	if err != nil {
		return err
	}
	quarantined, err := meter.Int64ObservableGauge("hypeman_machine_build_reservations_quarantined",
		metric.WithDescription("Machine build storage reservations retained after interruption or unconfirmed cleanup"))
	if err != nil {
		return err
	}
	available, err := meter.Int64ObservableGauge("hypeman_machine_build_filesystem_available_bytes",
		metric.WithDescription("Available bytes on the admitted data filesystem; not a reserved quota"), metric.WithUnit("By"))
	if err != nil {
		return err
	}
	floor, err := meter.Int64ObservableGauge("hypeman_machine_build_min_free_bytes",
		metric.WithDescription("Configured machine build free-space admission floor"), metric.WithUnit("By"))
	if err != nil {
		return err
	}
	_, err = meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		var activeCount, quarantineCount int64
		storage.mu.Lock()
		for _, job := range storage.jobs {
			if job.quarantined {
				quarantineCount++
			} else {
				activeCount++
			}
		}
		storage.mu.Unlock()
		observer.ObserveInt64(active, activeCount)
		observer.ObserveInt64(quarantined, quarantineCount)
		observer.ObserveInt64(floor, storage.limits.MinFreeBytes)
		// A single statfs call, without walking build directories or holding the
		// admission lock during filesystem I/O. Errors must not look like zero free.
		free, err := storage.available(storage.root)
		if err != nil {
			return err
		}
		observer.ObserveInt64(available, free)
		return nil
	}, active, quarantined, available, floor)
	return err
}
