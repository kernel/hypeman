package resources

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// mockUtilizationSourceWithData returns instances with simulated cgroup data
type mockUtilizationSourceWithData struct {
	utilizations []VMUtilization
}

func (m *mockUtilizationSourceWithData) ListRunningInstancesInfo(ctx context.Context) ([]InstanceUtilizationInfo, error) {
	// Return info that will be used by CollectVMUtilization
	// Since we can't easily mock the cgroup files in this test, we'll use a different approach
	infos := make([]InstanceUtilizationInfo, len(m.utilizations))
	for i, u := range m.utilizations {
		infos[i] = InstanceUtilizationInfo{
			ID:                   u.InstanceID,
			Name:                 u.InstanceName,
			AllocatedVcpus:       u.AllocatedVcpus,
			AllocatedMemoryBytes: u.AllocatedMemoryBytes,
		}
	}
	return infos, nil
}

func TestUtilizationMetrics_OTelIntegration(t *testing.T) {
	// Create an in-memory metric reader
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	meter := provider.Meter("test")

	// Create manager
	manager := &Manager{}

	// Initialize metrics
	err := manager.InitializeMetrics(meter)
	require.NoError(t, err)

	// Set up mock source
	mockSource := &mockUtilizationSource{
		instances: []InstanceUtilizationInfo{
			{
				ID:                   "test-vm-1",
				Name:                 "web-app",
				AllocatedVcpus:       2,
				AllocatedMemoryBytes: 2 * 1024 * 1024 * 1024,
			},
		},
	}
	manager.SetUtilizationSource(mockSource)

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err = reader.Collect(context.Background(), &rm)
	require.NoError(t, err)

	// Verify we have scope metrics
	require.NotEmpty(t, rm.ScopeMetrics, "should have scope metrics")

	// Find our metrics
	var foundCPU, foundAllocVcpus, foundMemoryRSS, foundMemoryVMS, foundAllocMem, foundMemRatio bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "hypeman_vm_cpu_seconds_total":
				foundCPU = true
			case "hypeman_vm_allocated_vcpus":
				foundAllocVcpus = true
			case "hypeman_vm_memory_rss_bytes":
				foundMemoryRSS = true
			case "hypeman_vm_memory_vms_bytes":
				foundMemoryVMS = true
			case "hypeman_vm_allocated_memory_bytes":
				foundAllocMem = true
			case "hypeman_vm_memory_utilization_ratio":
				foundMemRatio = true
			}
		}
	}

	assert.True(t, foundCPU, "should have CPU metric registered")
	assert.True(t, foundAllocVcpus, "should have allocated vCPUs metric registered")
	assert.True(t, foundMemoryRSS, "should have memory RSS metric registered")
	assert.True(t, foundMemoryVMS, "should have memory VMS metric registered")
	assert.True(t, foundAllocMem, "should have allocated memory metric registered")
	assert.True(t, foundMemRatio, "should have memory utilization ratio metric registered")
}

func TestUtilizationMetrics_NilMeter(t *testing.T) {
	manager := &Manager{}

	// Should not error with nil meter
	err := manager.InitializeMetrics(nil)
	require.NoError(t, err)
}

func TestUtilizationMetrics_MetricNames(t *testing.T) {
	// Verify all expected metric names are correct when data is present
	expectedMetrics := []string{
		"hypeman_vm_cpu_seconds_total",
		"hypeman_vm_allocated_vcpus",
		"hypeman_vm_memory_rss_bytes",
		"hypeman_vm_memory_vms_bytes",
		"hypeman_vm_allocated_memory_bytes",
		"hypeman_vm_network_rx_bytes_total",
		"hypeman_vm_network_tx_bytes_total",
		"hypeman_vm_memory_utilization_ratio",
	}

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	meter := provider.Meter("test")

	manager := &Manager{}
	// Use mock source with actual data so metrics will be emitted
	manager.SetUtilizationSource(&mockUtilizationSource{
		instances: []InstanceUtilizationInfo{
			{
				ID:                   "test-vm",
				Name:                 "test",
				AllocatedVcpus:       2,
				AllocatedMemoryBytes: 1024 * 1024 * 1024, // 1GB
			},
		},
	})
	err := manager.InitializeMetrics(meter)
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	err = reader.Collect(context.Background(), &rm)
	require.NoError(t, err)

	// Collect all metric names
	var metricNames []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metricNames = append(metricNames, m.Name)
		}
	}

	// Check all expected metrics are present
	for _, expected := range expectedMetrics {
		assert.Contains(t, metricNames, expected, "should have metric %s", expected)
	}
}
