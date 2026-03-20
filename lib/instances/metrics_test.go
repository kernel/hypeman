package instances

import (
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSnapshotCompressionMetrics_RecordAndObserve(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	m := &manager{
		paths: paths.New(t.TempDir()),
		compressionJobs: map[string]*compressionJob{
			"job-1": {
				done: make(chan struct{}),
				target: compressionTarget{
					Key:            "job-1",
					HypervisorType: hypervisor.TypeCloudHypervisor,
					Source:         snapshotCompressionSourceStandby,
					Policy: snapshotstore.SnapshotCompressionConfig{
						Enabled:   true,
						Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
					},
				},
			},
		},
	}

	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics

	target := m.compressionJobs["job-1"].target
	m.recordSnapshotCompressionJob(t.Context(), target, snapshotCompressionResultSuccess, time.Now().Add(-2*time.Second), 1024, 256)
	m.recordSnapshotRestoreMemoryPrepare(t.Context(), hypervisor.TypeCloudHypervisor, snapshotMemoryPreparePathRaw, snapshotCompressionResultSuccess, time.Now().Add(-250*time.Millisecond))
	m.recordSnapshotCompressionPreemption(t.Context(), snapshotCompressionPreemptionRestoreInstance, target)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	assertMetricNames(t, rm, []string{
		"hypeman_snapshot_compression_jobs_total",
		"hypeman_snapshot_compression_duration_seconds",
		"hypeman_snapshot_compression_saved_bytes",
		"hypeman_snapshot_compression_ratio",
		"hypeman_snapshot_restore_memory_prepare_total",
		"hypeman_snapshot_restore_memory_prepare_duration_seconds",
		"hypeman_snapshot_compression_preemptions_total",
		"hypeman_snapshot_compression_active_total",
	})

	jobsMetric := findMetric(t, rm, "hypeman_snapshot_compression_jobs_total")
	jobs, ok := jobsMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, jobs.DataPoints, 1)
	assert.Equal(t, int64(1), jobs.DataPoints[0].Value)
	assert.Equal(t, "cloud-hypervisor", metricLabel(t, jobs.DataPoints[0].Attributes, "hypervisor"))
	assert.Equal(t, "lz4", metricLabel(t, jobs.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "standby", metricLabel(t, jobs.DataPoints[0].Attributes, "source"))
	assert.Equal(t, "success", metricLabel(t, jobs.DataPoints[0].Attributes, "result"))

	savedBytesMetric := findMetric(t, rm, "hypeman_snapshot_compression_saved_bytes")
	savedBytes, ok := savedBytesMetric.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	require.Len(t, savedBytes.DataPoints, 1)
	assert.Equal(t, uint64(1), savedBytes.DataPoints[0].Count)
	assert.Equal(t, int64(768), savedBytes.DataPoints[0].Sum)

	restorePrepMetric := findMetric(t, rm, "hypeman_snapshot_restore_memory_prepare_total")
	restorePrep, ok := restorePrepMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, restorePrep.DataPoints, 1)
	assert.Equal(t, int64(1), restorePrep.DataPoints[0].Value)
	assert.Equal(t, "raw", metricLabel(t, restorePrep.DataPoints[0].Attributes, "restore_source"))
	assert.Equal(t, "success", metricLabel(t, restorePrep.DataPoints[0].Attributes, "result"))

	preemptionsMetric := findMetric(t, rm, "hypeman_snapshot_compression_preemptions_total")
	preemptions, ok := preemptionsMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, preemptions.DataPoints, 1)
	assert.Equal(t, int64(1), preemptions.DataPoints[0].Value)
	assert.Equal(t, "restore_instance", metricLabel(t, preemptions.DataPoints[0].Attributes, "operation"))

	activeMetric := findMetric(t, rm, "hypeman_snapshot_compression_active_total")
	active, ok := activeMetric.Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, active.DataPoints, 1)
	assert.Equal(t, int64(1), active.DataPoints[0].Value)
	assert.Equal(t, "lz4", metricLabel(t, active.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "standby", metricLabel(t, active.DataPoints[0].Attributes, "source"))
}

func assertMetricNames(t *testing.T, rm metricdata.ResourceMetrics, expected []string) {
	t.Helper()

	metricNames := make(map[string]bool)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metricNames[m.Name] = true
		}
	}

	for _, name := range expected {
		assert.True(t, metricNames[name], "expected metric %s to be registered", name)
	}
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return metricdata.Metrics{}
}

func metricLabel(t *testing.T, attrs attribute.Set, key string) string {
	t.Helper()

	value, ok := attrs.Value(attribute.Key(key))
	require.True(t, ok, "expected label %s", key)
	return value.AsString()
}
