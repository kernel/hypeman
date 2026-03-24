package resources

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type monitoringInstanceLister struct {
	mu          sync.RWMutex
	allocations []InstanceAllocation
}

func (m *monitoringInstanceLister) ListInstanceAllocations(ctx context.Context) ([]InstanceAllocation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]InstanceAllocation(nil), m.allocations...), nil
}

func (m *monitoringInstanceLister) SetAllocations(allocations []InstanceAllocation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allocations = append([]InstanceAllocation(nil), allocations...)
}

type monitoringImageLister struct {
	mu            sync.RWMutex
	totalBytes    int64
	ociCacheBytes int64
}

func (m *monitoringImageLister) TotalImageBytes(ctx context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalBytes, nil
}

func (m *monitoringImageLister) TotalOCICacheBytes(ctx context.Context) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ociCacheBytes, nil
}

func (m *monitoringImageLister) SetSizes(totalBytes, ociCacheBytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.totalBytes = totalBytes
	m.ociCacheBytes = ociCacheBytes
}

func monitoringTestManager(t *testing.T) (*Manager, *monitoringInstanceLister, *monitoringImageLister) {
	t.Helper()

	cfg := &config.Config{
		DataDir: t.TempDir(),
		Limits: config.LimitsConfig{
			MaxImageStorage: 0.2,
		},
		Oversubscription: config.OversubscriptionConfig{
			CPU: 2.0, Memory: 1.5, Disk: 1.0, Network: 1.0, DiskIO: 2.0,
		},
		Capacity: config.CapacityConfig{
			Network: "10Gbps",
			DiskIO:  "1GB/s",
		},
	}

	instanceLister := &monitoringInstanceLister{
		allocations: []InstanceAllocation{
			{
				ID:                 "vm-1",
				Name:               "test-vm",
				Vcpus:              4,
				MemoryBytes:        8 * 1024 * 1024 * 1024,
				OverlayBytes:       10 * 1024 * 1024 * 1024,
				VolumeOverlayBytes: 5 * 1024 * 1024 * 1024,
				NetworkDownloadBps: 125000000,
				NetworkUploadBps:   125000000,
				DiskIOBps:          64 * 1024 * 1024,
				State:              "Running",
			},
		},
	}
	imageLister := &monitoringImageLister{
		totalBytes:    50 * 1024 * 1024 * 1024,
		ociCacheBytes: 25 * 1024 * 1024 * 1024,
	}
	volumeLister := &mockVolumeLister{totalBytes: 100 * 1024 * 1024 * 1024}

	mgr := NewManager(cfg, paths.New(cfg.DataDir))
	mgr.SetInstanceLister(instanceLister)
	mgr.SetImageLister(imageLister)
	mgr.SetVolumeLister(volumeLister)
	require.NoError(t, mgr.Initialize(context.Background()))

	return mgr, instanceLister, imageLister
}

func TestStartMonitoringPublishesCapacityMetrics(t *testing.T) {
	mgr, _, _ := monitoringTestManager(t)

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, mgr.StartMonitoring(ctx, provider.Meter("test"), time.Hour))

	status, err := mgr.GetFullStatus(context.Background())
	require.NoError(t, err)

	rm := collectMonitoringMetrics(t, reader)

	require.Equal(t, status.CPU.Capacity, int64GaugeValue(t, rm, "hypeman_resources_capacity", map[string]string{"resource": "cpu"}))
	require.Equal(t, status.CPU.EffectiveLimit, int64GaugeValue(t, rm, "hypeman_resources_effective_limit", map[string]string{"resource": "cpu"}))
	require.Equal(t, status.CPU.Allocated, int64GaugeValue(t, rm, "hypeman_resources_allocated", map[string]string{"resource": "cpu"}))
	require.Equal(t, status.CPU.OversubRatio, float64GaugeValue(t, rm, "hypeman_resources_oversub_ratio", map[string]string{"resource": "cpu"}))

	require.NotNil(t, status.DiskDetail)
	require.Equal(t, status.DiskDetail.Images, int64GaugeValue(t, rm, "hypeman_resources_disk_breakdown_bytes", map[string]string{"component": "images"}))
	require.Equal(t, status.DiskDetail.OCICache, int64GaugeValue(t, rm, "hypeman_resources_disk_breakdown_bytes", map[string]string{"component": "oci_cache"}))
	require.Equal(t, status.DiskDetail.Volumes, int64GaugeValue(t, rm, "hypeman_resources_disk_breakdown_bytes", map[string]string{"component": "volumes"}))
	require.Equal(t, status.DiskDetail.Overlays, int64GaugeValue(t, rm, "hypeman_resources_disk_breakdown_bytes", map[string]string{"component": "overlays"}))

	currentImageStorage := status.DiskDetail.Images + status.DiskDetail.OCICache
	require.Equal(t, currentImageStorage, int64GaugeValue(t, rm, "hypeman_resources_image_storage_bytes", map[string]string{"kind": "current"}))
	require.Equal(t, mgr.MaxImageStorageBytes(), int64GaugeValue(t, rm, "hypeman_resources_image_storage_bytes", map[string]string{"kind": "max"}))
}

func TestStartMonitoringRefreshesSnapshot(t *testing.T) {
	mgr, instanceLister, imageLister := monitoringTestManager(t)

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, mgr.StartMonitoring(ctx, provider.Meter("test"), 20*time.Millisecond))

	instanceLister.SetAllocations([]InstanceAllocation{
		{
			ID:                 "vm-2",
			Name:               "updated-vm",
			Vcpus:              7,
			MemoryBytes:        12 * 1024 * 1024 * 1024,
			OverlayBytes:       15 * 1024 * 1024 * 1024,
			VolumeOverlayBytes: 1 * 1024 * 1024 * 1024,
			NetworkDownloadBps: 200000000,
			NetworkUploadBps:   180000000,
			DiskIOBps:          128 * 1024 * 1024,
			State:              "Running",
		},
	})
	imageLister.SetSizes(60*1024*1024*1024, 10*1024*1024*1024)

	require.Eventually(t, func() bool {
		rm := collectMonitoringMetrics(t, reader)
		cpuAllocated := int64GaugeValue(t, rm, "hypeman_resources_allocated", map[string]string{"resource": "cpu"})
		imageCurrent := int64GaugeValue(t, rm, "hypeman_resources_image_storage_bytes", map[string]string{"kind": "current"})
		return cpuAllocated == 7 && imageCurrent == 70*1024*1024*1024
	}, 500*time.Millisecond, 20*time.Millisecond)
}

func TestStartMonitoringPublishesGPUMetrics(t *testing.T) {
	mgr, _, _ := monitoringTestManager(t)

	originalProvider := gpuStatusProvider
	gpuStatusProvider = func() *GPUResourceStatus {
		return &GPUResourceStatus{
			Mode:       "vgpu",
			TotalSlots: 8,
			UsedSlots:  3,
			Profiles: []devices.GPUProfile{
				{Name: "L40S-1Q", Available: 5},
				{Name: "L40S-2Q", Available: 2},
			},
		}
	}
	defer func() {
		gpuStatusProvider = originalProvider
	}()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, mgr.StartMonitoring(ctx, provider.Meter("test"), time.Hour))

	rm := collectMonitoringMetrics(t, reader)
	require.Equal(t, int64(3), int64GaugeValue(t, rm, "hypeman_resources_gpu_slots", map[string]string{"kind": "used"}))
	require.Equal(t, int64(8), int64GaugeValue(t, rm, "hypeman_resources_gpu_slots", map[string]string{"kind": "total"}))
	require.Equal(t, int64(5), int64GaugeValue(t, rm, "hypeman_resources_gpu_profile_slots", map[string]string{"profile": "L40S-1Q", "kind": "available"}))
	require.Equal(t, int64(2), int64GaugeValue(t, rm, "hypeman_resources_gpu_profile_slots", map[string]string{"profile": "L40S-2Q", "kind": "available"}))
}

func collectMonitoringMetrics(t *testing.T, reader *otelmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	return rm
}

func int64GaugeValue(t *testing.T, rm metricdata.ResourceMetrics, name string, wantAttrs map[string]string) int64 {
	t.Helper()

	metric := findMonitoringMetric(t, rm, name)
	gauge, ok := metric.Data.(metricdata.Gauge[int64])
	require.True(t, ok, "expected int64 gauge metric data for %s", name)
	for _, point := range gauge.DataPoints {
		if metricAttrsMatch(point.Attributes, wantAttrs) {
			return point.Value
		}
	}
	t.Fatalf("metric %s with attrs %v not found", name, wantAttrs)
	return 0
}

func float64GaugeValue(t *testing.T, rm metricdata.ResourceMetrics, name string, wantAttrs map[string]string) float64 {
	t.Helper()

	metric := findMonitoringMetric(t, rm, name)
	gauge, ok := metric.Data.(metricdata.Gauge[float64])
	require.True(t, ok, "expected float64 gauge metric data for %s", name)
	for _, point := range gauge.DataPoints {
		if metricAttrsMatch(point.Attributes, wantAttrs) {
			return point.Value
		}
	}
	t.Fatalf("metric %s with attrs %v not found", name, wantAttrs)
	return 0
}

func findMonitoringMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()

	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == name {
				return metric
			}
		}
	}

	t.Fatalf("metric %s not found", name)
	return metricdata.Metrics{}
}

func metricAttrsMatch(set attribute.Set, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}

	attrs := make(map[string]string, len(set.ToSlice()))
	for _, kv := range set.ToSlice() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	for key, value := range want {
		if attrs[key] != value {
			return false
		}
	}
	return true
}
