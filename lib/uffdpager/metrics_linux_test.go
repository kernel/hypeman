//go:build linux

package uffdpager

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRegisterMetricsObservesStats(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))
	meter := provider.Meter("hypeman-uffd-pager-test")

	stats := Stats{
		Version:             "0.1.2",
		Draining:            true,
		ActiveSessions:      3,
		CacheBytes:          1024,
		CacheMax:            4096,
		CacheItems:          8,
		CacheHits:           100,
		CacheMisses:         5,
		CacheShards:         4,
		CacheLookupNanos:    2000,
		CacheLookupMaxNanos: 500,
		CacheAddNanos:       3000,
		CacheAddMaxNanos:    900,
		Faults:              42,
		BackingBytesRead:    16384,
		Copies:              41,
		CopyErrors:          1,
		ActiveFaults:        2,
		MaxConcurrentFaults: 7,
		FaultNanos:          9999,
		FaultMaxNanos:       333,
	}
	require.NoError(t, RegisterMetrics(meter, "0.1.2", func() Stats { return stats }))

	var got metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &got))

	values := collectInt64Values(t, got)
	require.Equal(t, int64(100), values["hypeman_uffd_cache_hits_total"])
	require.Equal(t, int64(5), values["hypeman_uffd_cache_misses_total"])
	require.Equal(t, int64(42), values["hypeman_uffd_faults_total"])
	require.Equal(t, int64(1024), values["hypeman_uffd_cache_bytes"])
	require.Equal(t, int64(4096), values["hypeman_uffd_cache_max_bytes"])
	require.Equal(t, int64(8), values["hypeman_uffd_cache_items"])
	require.Equal(t, int64(3), values["hypeman_uffd_active_sessions"])
	require.Equal(t, int64(7), values["hypeman_uffd_max_concurrent_faults"])
	require.Equal(t, int64(1), values["hypeman_uffd_draining"])
	require.Equal(t, int64(500), values["hypeman_uffd_cache_lookup_max_nanos"])
}

func TestRegisterMetricsNilMeter(t *testing.T) {
	t.Parallel()

	require.NoError(t, RegisterMetrics(nil, "0.1.2", func() Stats { return Stats{} }))
}

func collectInt64Values(t *testing.T, rm metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	out := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = dp.Value
				}
			case metricdata.Gauge[int64]:
				for _, dp := range d.DataPoints {
					out[m.Name] = dp.Value
				}
			}
		}
	}
	return out
}
