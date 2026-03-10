//go:build linux

package vm_metrics

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
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
		"hypeman_vm_metrics_instances_observed",
		"hypeman_vm_metrics_label_budget_exceeded_total",
	}

	for _, expected := range expectedMetrics {
		assert.True(t, metricNames[expected], "should have metric %s", expected)
	}
	assert.False(t, metricNames["hypeman_vm_memory_utilization_ratio"], "should not have denormalized ratio metric")

	// Validate per-VM labels are still present on VM series.
	var foundCPUMetric bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "hypeman_vm_cpu_seconds_total" {
				continue
			}
			foundCPUMetric = true
			sum, ok := m.Data.(metricdata.Sum[float64])
			require.True(t, ok, "expected cpu metric to be sum")
			require.NotEmpty(t, sum.DataPoints)
			_, hasInstanceID := sum.DataPoints[0].Attributes.Value(attribute.Key("instance_id"))
			_, hasInstanceName := sum.DataPoints[0].Attributes.Value(attribute.Key("instance_name"))
			assert.True(t, hasInstanceID, "cpu series should include instance_id label")
			assert.True(t, hasInstanceName, "cpu series should include instance_name label")
		}
	}
	require.True(t, foundCPUMetric, "expected cpu metric datapoint")
}

func TestOTelMetrics_NilMeter(t *testing.T) {
	mgr := NewManager()
	err := mgr.InitializeOTel(nil)
	require.NoError(t, err, "nil meter should not error")
}

func TestOTelMetrics_LabelBudgetGuardrails(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	meter := provider.Meter("test")

	source := &mockInstanceSource{
		instances: []InstanceInfo{
			{ID: "vm-1", Name: "vm-one", AllocatedVcpus: 2, AllocatedMemoryBytes: 1024},
			{ID: "vm-2", Name: "vm-two", AllocatedVcpus: 2, AllocatedMemoryBytes: 1024},
		},
	}
	mgr := NewManager()
	mgr.SetInstanceSource(source)
	mgr.SetVMLabelBudget(1)
	require.NoError(t, mgr.InitializeOTel(meter))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	// Reduce cardinality to trigger recovery and ensure counter does not increment again.
	source.instances = source.instances[:1]
	require.NoError(t, reader.Collect(t.Context(), &rm))

	var observed int64
	var budgetExceededTransitions int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "hypeman_vm_metrics_instances_observed":
				g, ok := m.Data.(metricdata.Gauge[int64])
				require.True(t, ok)
				require.NotEmpty(t, g.DataPoints)
				observed = g.DataPoints[0].Value
			case "hypeman_vm_metrics_label_budget_exceeded_total":
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				require.NotEmpty(t, sum.DataPoints)
				budgetExceededTransitions = sum.DataPoints[0].Value
			}
		}
	}

	assert.Equal(t, int64(1), observed, "observed instances gauge should reflect latest scrape")
	assert.Equal(t, int64(1), budgetExceededTransitions, "counter should increment only on over-budget transition")
}
