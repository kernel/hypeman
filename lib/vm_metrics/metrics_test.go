package vm_metrics

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestOTelMetrics_Registration(t *testing.T) {
	// Create a test meter
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	meter := provider.Meter("test")

	// Create manager with mock source
	mgr := NewManager()
	pid := os.Getpid()
	mgr.SetInstanceSource(&mockInstanceSource{
		instances: []InstanceInfo{
			{
				ID:                   "test-vm",
				Name:                 "Test VM",
				HypervisorPID:        &pid,
				AllocatedVcpus:       2,
				AllocatedMemoryBytes: 2 * 1024 * 1024 * 1024,
			},
		},
	})

	// Initialize OTel
	err := mgr.InitializeOTel(meter)
	require.NoError(t, err)

	// Collect metrics
	var rm metricdata.ResourceMetrics
	err = reader.Collect(t.Context(), &rm)
	require.NoError(t, err)

	// Verify we have scope metrics
	require.NotEmpty(t, rm.ScopeMetrics, "should have scope metrics")

	// Find our metrics
	metricNames := make(map[string]bool)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metricNames[m.Name] = true
		}
	}

	// Check expected metrics are present
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

	for _, expected := range expectedMetrics {
		assert.True(t, metricNames[expected], "should have metric %s", expected)
	}
}

func TestOTelMetrics_NilMeter(t *testing.T) {
	mgr := NewManager()
	err := mgr.InitializeOTel(nil)
	require.NoError(t, err, "nil meter should not error")
}
