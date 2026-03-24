package resources

import (
	"context"
	"fmt"
	"sync"
	"time"

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
	capturedAt          time.Time
	status              FullResourceStatus
	imageStorageCurrent int64
	imageStorageMax     int64
}

type monitoringMetrics struct {
	capacity        metric.Int64ObservableGauge
	effectiveLimit  metric.Int64ObservableGauge
	allocated       metric.Int64ObservableGauge
	oversubRatio    metric.Float64ObservableGauge
	diskBreakdown   metric.Int64ObservableGauge
	imageStorage    metric.Int64ObservableGauge
	gpuSlots        metric.Int64ObservableGauge
	gpuProfileSlots metric.Int64ObservableGauge
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
		metrics, err := newMonitoringMetrics(meter, m)
		if err != nil {
			m.monitoring.mu.Unlock()
			return err
		}
		m.monitoring.metricsRegistered = true
		_ = metrics
	}
	m.monitoring.mu.Unlock()

	if err := m.refreshMonitoringSnapshot(ctx); err != nil {
		return err
	}

	m.monitoring.mu.Lock()
	if m.monitoring.started {
		m.monitoring.mu.Unlock()
		return nil
	}
	m.monitoring.started = true
	m.monitoring.mu.Unlock()

	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = m.refreshMonitoringSnapshot(ctx)
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
		capturedAt: time.Now(),
		status:     *status,
	}
	if status.DiskDetail != nil {
		snapshot.imageStorageCurrent = status.DiskDetail.Images + status.DiskDetail.OCICache
	}
	snapshot.imageStorageMax = m.MaxImageStorageBytes()

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

func newMonitoringMetrics(meter metric.Meter, mgr *Manager) (*monitoringMetrics, error) {
	capacity, err := meter.Int64ObservableGauge(
		"hypeman_resources_capacity",
		metric.WithDescription("Raw host capacity by resource type"),
	)
	if err != nil {
		return nil, err
	}

	effectiveLimit, err := meter.Int64ObservableGauge(
		"hypeman_resources_effective_limit",
		metric.WithDescription("Effective allocatable limit by resource type after oversubscription"),
	)
	if err != nil {
		return nil, err
	}

	allocated, err := meter.Int64ObservableGauge(
		"hypeman_resources_allocated",
		metric.WithDescription("Current allocated amount by resource type"),
	)
	if err != nil {
		return nil, err
	}

	oversubRatio, err := meter.Float64ObservableGauge(
		"hypeman_resources_oversub_ratio",
		metric.WithDescription("Oversubscription ratio by resource type"),
	)
	if err != nil {
		return nil, err
	}

	diskBreakdown, err := meter.Int64ObservableGauge(
		"hypeman_resources_disk_breakdown_bytes",
		metric.WithDescription("Disk usage broken down by component"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	imageStorage, err := meter.Int64ObservableGauge(
		"hypeman_resources_image_storage_bytes",
		metric.WithDescription("Current and maximum image storage bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	gpuSlots, err := meter.Int64ObservableGauge(
		"hypeman_resources_gpu_slots",
		metric.WithDescription("Total and used GPU slots"),
	)
	if err != nil {
		return nil, err
	}

	gpuProfileSlots, err := meter.Int64ObservableGauge(
		"hypeman_resources_gpu_profile_slots",
		metric.WithDescription("Available GPU slots by vGPU profile"),
	)
	if err != nil {
		return nil, err
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
		o.ObserveInt64(imageStorage, snapshot.imageStorageMax, metric.WithAttributes(attribute.String("kind", "max")))

		if snapshot.status.GPU != nil {
			o.ObserveInt64(gpuSlots, int64(snapshot.status.GPU.UsedSlots), metric.WithAttributes(attribute.String("kind", "used")))
			o.ObserveInt64(gpuSlots, int64(snapshot.status.GPU.TotalSlots), metric.WithAttributes(attribute.String("kind", "total")))
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
	}, capacity, effectiveLimit, allocated, oversubRatio, diskBreakdown, imageStorage, gpuSlots, gpuProfileSlots); err != nil {
		return nil, err
	}

	return &monitoringMetrics{
		capacity:        capacity,
		effectiveLimit:  effectiveLimit,
		allocated:       allocated,
		oversubRatio:    oversubRatio,
		diskBreakdown:   diskBreakdown,
		imageStorage:    imageStorage,
		gpuSlots:        gpuSlots,
		gpuProfileSlots: gpuProfileSlots,
	}, nil
}
