package images

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestImageBuildPhaseMetrics(t *testing.T) {
	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))
	m := &manager{
		paths: paths.New(t.TempDir()),
		queue: NewBuildQueue(1),
	}

	metrics, err := newMetrics(provider.Meter("test"), m)
	require.NoError(t, err)
	m.metrics = metrics

	m.recordPullResultMetrics(t.Context(), "sha256:test", &pullResult{
		Metadata:        &containerMetadata{},
		CacheHit:        true,
		LayerCount:      74,
		CompressedBytes: 2_467_319_902,
		Phases: []imageBuildPhaseMeasurement{{
			Phase:    "layer_unpack",
			Duration: 1500 * time.Millisecond,
			Status:   "success",
		}},
	})

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	phase := findImageMetric(t, rm, "hypeman_images_build_phase_duration_seconds")
	phaseHistogram, ok := phase.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, phaseHistogram.DataPoints, 1)
	require.Equal(t, uint64(1), phaseHistogram.DataPoints[0].Count)
	require.InDelta(t, 1.5, phaseHistogram.DataPoints[0].Sum, 0.001)
	requireMetricAttributes(t, phaseHistogram.DataPoints[0].Attributes, map[string]string{
		"phase":        "layer_unpack",
		"status":       "success",
		"cache_status": "hit",
	})

	layers := findImageMetric(t, rm, "hypeman_images_oci_layer_count")
	layerHistogram, ok := layers.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	require.Equal(t, int64(74), layerHistogram.DataPoints[0].Sum)
	requireMetricAttributes(t, layerHistogram.DataPoints[0].Attributes, map[string]string{"cache_status": "hit"})

	bytes := findImageMetric(t, rm, "hypeman_images_oci_compressed_bytes")
	bytesHistogram, ok := bytes.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	require.Equal(t, int64(2_467_319_902), bytesHistogram.DataPoints[0].Sum)
	require.Equal(t, []float64{
		1 << 20,
		4 << 20,
		16 << 20,
		64 << 20,
		256 << 20,
		1 << 30,
		2 << 30,
		4 << 30,
		8 << 30,
		16 << 30,
	}, bytesHistogram.DataPoints[0].Bounds)
	requireMetricAttributes(t, bytesHistogram.DataPoints[0].Attributes, map[string]string{"cache_status": "hit"})
}

func findImageMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
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

func requireMetricAttributes(t *testing.T, attrs attribute.Set, want map[string]string) {
	t.Helper()
	for key, value := range want {
		got, ok := attrs.Value(attribute.Key(key))
		require.True(t, ok, "attribute %s not found", key)
		require.Equal(t, value, got.AsString())
	}
}
