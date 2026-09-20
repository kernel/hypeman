package resources

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/kernel/hypeman/lib/diskutilization"
	"github.com/kernel/hypeman/lib/logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type monitoringState struct {
	mu                sync.RWMutex
	started           bool
	metricsRegistered bool
	snapshot          monitoringSnapshot
	hasSnapshot       bool
}

type monitoringSnapshot struct {
	status              FullResourceStatus
	imageStorageCurrent int64
	imageStorageMax     int64
	diskUtilization     diskutilization.Breakdown
}

func (m *Manager) StartMonitoring(ctx context.Context, meter metric.Meter, refreshInterval time.Duration) error {
	if meter == nil {
		return nil
	}
	if refreshInterval <= 0 {
		return fmt.Errorf("resource monitoring refresh interval must be positive, got %s", refreshInterval)
	}

	m.monitoring.mu.Lock()
	if m.monitoring.started {
		m.monitoring.mu.Unlock()
		return nil
	}
	if !m.monitoring.metricsRegistered {
		if err := newMonitoringMetrics(meter, m); err != nil {
			m.monitoring.mu.Unlock()
			return err
		}
		m.monitoring.metricsRegistered = true
	}
	m.monitoring.started = true
	m.monitoring.mu.Unlock()

	go func() {
		log := logger.FromContext(ctx)
		defer func() {
			if r := recover(); r != nil {
				m.monitoring.mu.Lock()
				m.monitoring.started = false
				m.monitoring.mu.Unlock()
				log.ErrorContext(ctx, "resource monitoring refresh loop panicked",
					"panic", r,
					"stack", string(debug.Stack()),
				)
			}
		}()

		initialRefreshStarted := time.Now()
		if err := m.refreshMonitoringSnapshot(ctx); err != nil {
			log.WarnContext(ctx, "initial resource monitoring snapshot failed", "error", err)
		} else {
			log.InfoContext(ctx, "initial resource monitoring snapshot complete", "duration", time.Since(initialRefreshStarted))
		}

		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.refreshMonitoringSnapshot(ctx); err != nil {
					log.WarnContext(ctx, "resource monitoring snapshot refresh failed", "error", err)
				}
			}
		}
	}()

	return nil
}

func (m *Manager) refreshMonitoringSnapshot(ctx context.Context) error {
	status, err := m.GetFullStatus(ctx)
	if err != nil {
		return err
	}

	snapshot := monitoringSnapshot{
		status: *status,
	}
	if status.DiskDetail != nil {
		snapshot.imageStorageCurrent = status.DiskDetail.Images + status.DiskDetail.OCICache
	}
	snapshot.imageStorageMax = m.MaxImageStorageBytes()

	diskUtilization, err := diskutilization.Collect(m.paths)
	if err != nil {
		return err
	}
	snapshot.diskUtilization = diskUtilization

	m.monitoring.mu.Lock()
	m.monitoring.snapshot = snapshot
	m.monitoring.hasSnapshot = true
	m.monitoring.mu.Unlock()

	return nil
}

func (m *Manager) currentMonitoringSnapshot() (monitoringSnapshot, bool) {
	m.monitoring.mu.RLock()
	defer m.monitoring.mu.RUnlock()

	if !m.monitoring.hasSnapshot {
		return monitoringSnapshot{}, false
	}

	return m.monitoring.snapshot, true
}

func newMonitoringMetrics(meter metric.Meter, mgr *Manager) error {
	capacity, err := meter.Int64ObservableGauge(
		"hypeman_resources_capacity",
		metric.WithDescription("Raw host capacity by resource type"),
	)
	if err != nil {
		return err
	}

	effectiveLimit, err := meter.Int64ObservableGauge(
		"hypeman_resources_effective_limit",
		metric.WithDescription("Effective allocatable limit by resource type after oversubscription"),
	)
	if err != nil {
		return err
	}

	allocated, err := meter.Int64ObservableGauge(
		"hypeman_resources_allocated",
		metric.WithDescription("Current allocated amount by resource type"),
	)
	if err != nil {
		return err
	}

	oversubRatio, err := meter.Float64ObservableGauge(
		"hypeman_resources_oversub_ratio",
		metric.WithDescription("Oversubscription ratio by resource type"),
	)
	if err != nil {
		return err
	}

	diskBreakdown, err := meter.Int64ObservableGauge(
		"hypeman_resources_disk_breakdown_bytes",
		metric.WithDescription("Disk usage broken down by component"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	diskUtilization, err := meter.Int64ObservableGauge(
		"hypeman_disk_utilization_bytes",
		metric.WithDescription("Actual disk bytes allocated on the filesystem by component"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	imageStorage, err := meter.Int64ObservableGauge(
		"hypeman_resources_image_storage_bytes",
		metric.WithDescription("Current and maximum image storage bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return err
	}

	gpuSlots, err := meter.Int64ObservableGauge(
		"hypeman_resources_gpu_slots",
		metric.WithDescription("Total, used, allocatable, and quarantined GPU slots"),
	)
	if err != nil {
		return err
	}

	gpuProfileSlots, err := meter.Int64ObservableGauge(
		"hypeman_resources_gpu_profile_slots",
		metric.WithDescription("Virtual functions able to create each vGPU profile (best-effort snapshot)"),
	)
	if err != nil {
		return err
	}

	gpuPlacementDisabled, err := meter.Int64ObservableGauge(
		"hypeman_resources_gpu_placement_disabled",
		metric.WithDescription("1 while vGPU placement is refused because the VF health state is unavailable, otherwise 0"),
	)
	if err != nil {
		return err
	}

	if _, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		snapshot, ok := mgr.currentMonitoringSnapshot()
		if !ok {
			return nil
		}

		resourceStatuses := []ResourceStatus{
			snapshot.status.CPU,
			snapshot.status.Memory,
			snapshot.status.Disk,
			snapshot.status.Network,
			snapshot.status.DiskIO,
		}
		for _, status := range resourceStatuses {
			attrs := metric.WithAttributes(attribute.String("resource", string(status.Type)))
			o.ObserveInt64(capacity, status.Capacity, attrs)
			o.ObserveInt64(effectiveLimit, status.EffectiveLimit, attrs)
			o.ObserveInt64(allocated, status.Allocated, attrs)
			o.ObserveFloat64(oversubRatio, status.OversubRatio, attrs)
		}

		if snapshot.status.DiskDetail != nil {
			o.ObserveInt64(diskBreakdown, snapshot.status.DiskDetail.Images, metric.WithAttributes(attribute.String("component", "images")))
			o.ObserveInt64(diskBreakdown, snapshot.status.DiskDetail.OCICache, metric.WithAttributes(attribute.String("component", "oci_cache")))
			o.ObserveInt64(diskBreakdown, snapshot.status.DiskDetail.Volumes, metric.WithAttributes(attribute.String("component", "volumes")))
			o.ObserveInt64(diskBreakdown, snapshot.status.DiskDetail.Overlays, metric.WithAttributes(attribute.String("component", "overlays")))
			o.ObserveInt64(imageStorage, snapshot.imageStorageCurrent, metric.WithAttributes(attribute.String("kind", "current")))
		}

		for component, value := range snapshot.diskUtilization.Components() {
			o.ObserveInt64(diskUtilization, value, metric.WithAttributes(attribute.String("component", component)))
		}
		o.ObserveInt64(imageStorage, snapshot.imageStorageMax, metric.WithAttributes(attribute.String("kind", "max")))

		if snapshot.status.GPU != nil {
			o.ObserveInt64(gpuSlots, int64(snapshot.status.GPU.UsedSlots), metric.WithAttributes(attribute.String("kind", "used")))
			o.ObserveInt64(gpuSlots, int64(snapshot.status.GPU.TotalSlots), metric.WithAttributes(attribute.String("kind", "total")))
			o.ObserveInt64(gpuSlots, int64(snapshot.status.GPU.AllocatableSlots), metric.WithAttributes(attribute.String("kind", "allocatable")))
			o.ObserveInt64(gpuSlots, int64(snapshot.status.GPU.QuarantinedSlots), metric.WithAttributes(attribute.String("kind", "quarantined")))
			var placementDisabled int64
			if snapshot.status.GPU.PlacementDisabledReason != "" {
				placementDisabled = 1
			}
			o.ObserveInt64(gpuPlacementDisabled, placementDisabled)
			for _, profile := range snapshot.status.GPU.Profiles {
				o.ObserveInt64(gpuProfileSlots, int64(profile.Available),
					metric.WithAttributes(
						attribute.String("profile", profile.Name),
						attribute.String("kind", "available"),
					),
				)
			}
		}

		return nil
	}, capacity, effectiveLimit, allocated, oversubRatio, diskBreakdown, diskUtilization, imageStorage, gpuSlots, gpuProfileSlots, gpuPlacementDisabled); err != nil {
		return err
	}

	return nil
}
