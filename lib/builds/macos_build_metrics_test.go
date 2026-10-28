package builds

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collectBuildGauges(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &data))
	values := make(map[string]int64)
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				continue
			}
			require.Len(t, gauge.DataPoints, 1, m.Name)
			point := gauge.DataPoints[0]
			require.Zero(t, point.Attributes.Len(), m.Name)
			values[m.Name] = point.Value
		}
	}
	return values
}

func TestManagerRegistersQueueMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	config := DefaultConfig()
	config.MaxConcurrentBuilds = 1
	api, err := NewManager(paths.New(t.TempDir()), config, nil, nil, nil, nil, nil, nil, provider.Meter("test"))
	require.NoError(t, err)
	m := api.(*manager)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	m.queue.Enqueue("one", CreateBuildRequest{}, func() { <-gate })
	m.queue.Enqueue("two", CreateBuildRequest{}, func() { <-gate })
	values := collectBuildGauges(t, reader)
	require.EqualValues(t, 1, values["hypeman_build_queue_length"])
	require.EqualValues(t, 1, values["hypeman_builds_active"])
	for name := range values {
		require.False(t, strings.HasPrefix(name, "hypeman_machine_build_"), name)
	}
}

func TestMachineStorageMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	storage := testMachineStorage(t)
	var free atomic.Int64
	free.Store(1 << 40)
	var probes atomic.Int64
	storage.available = func(string) (int64, error) { probes.Add(1); return free.Load(), nil }
	config := DefaultConfig()
	config.MachineBuild = &MachineBuildBackend{Storage: storage}
	_, err := NewManager(paths.New(t.TempDir()), config, nil, nil, nil, nil, nil, nil, provider.Meter("test"))
	require.NoError(t, err)
	ctx := context.Background()
	values := collectBuildGauges(t, reader)
	require.Len(t, values, 6, "four native and two shared queue gauges")
	require.EqualValues(t, 0, values["hypeman_machine_build_reservations_active"])
	require.EqualValues(t, 0, values["hypeman_machine_build_reservations_quarantined"])
	require.EqualValues(t, 1<<40, values["hypeman_machine_build_filesystem_available_bytes"])
	require.EqualValues(t, storage.limits.MinFreeBytes, values["hypeman_machine_build_min_free_bytes"])
	require.EqualValues(t, 1, probes.Load(), "one filesystem probe per collection")
	require.NoError(t, storage.reserve(ctx, "active", storage.root))
	require.NoError(t, storage.reserve(ctx, "quarantine", storage.root))
	require.NoError(t, storage.finish("quarantine", false))
	free.Store(12345)
	values = collectBuildGauges(t, reader)
	require.EqualValues(t, 1, values["hypeman_machine_build_reservations_active"])
	require.EqualValues(t, 1, values["hypeman_machine_build_reservations_quarantined"])
	require.EqualValues(t, 12345, values["hypeman_machine_build_filesystem_available_bytes"])
	require.NoError(t, storage.finish("active", true))
	require.NoError(t, storage.reaped("quarantine"))
	values = collectBuildGauges(t, reader)
	require.Zero(t, values["hypeman_machine_build_reservations_active"])
	require.Zero(t, values["hypeman_machine_build_reservations_quarantined"])
}

func TestMachineStorageMetricsOmitUnavailableFilesystem(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	storage := testMachineStorage(t)
	require.NoError(t, registerMachineStorageMetrics(provider.Meter("test"), storage))
	values := collectBuildGauges(t, reader)
	require.Contains(t, values, "hypeman_machine_build_filesystem_available_bytes")
	probeErr := errors.New("statfs unavailable")
	storage.available = func(string) (int64, error) { return 0, probeErr }
	var data metricdata.ResourceMetrics
	// The SDK reports callback errors while retaining independent observations.
	require.ErrorIs(t, reader.Collect(context.Background(), &data), probeErr)
	foundFloor := false
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			require.NotEqual(t, "hypeman_machine_build_filesystem_available_bytes", m.Name)
			if m.Name == "hypeman_machine_build_min_free_bytes" {
				foundFloor = true
			}
		}
	}
	require.True(t, foundFloor)
}
