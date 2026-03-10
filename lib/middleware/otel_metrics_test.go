package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestHTTPMetrics_UnmatchedRouteUsesSentinelPathLabel(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	meter := provider.Meter("test")

	httpMetrics, err := NewHTTPMetrics(meter)
	require.NoError(t, err)

	handler := httpMetrics.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/dynamic/path/123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)

	var rm metricdata.ResourceMetrics
	err = reader.Collect(t.Context(), &rm)
	require.NoError(t, err)

	var pathLabel string
	foundMetric := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "hypeman_http_requests_total" {
				continue
			}
			foundMetric = true
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected sum metric data")
			require.NotEmpty(t, sum.DataPoints)
			v, ok := sum.DataPoints[0].Attributes.Value(attribute.Key("path"))
			require.True(t, ok, "expected path label in metric attributes")
			pathLabel = v.AsString()
		}
	}

	require.True(t, foundMetric, "expected hypeman_http_requests_total metric")
	require.Equal(t, unmatchedRouteLabel, pathLabel)
}
