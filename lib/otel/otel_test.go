package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInitMetricsHandlerAlwaysAvailable(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "otel push disabled",
			cfg: Config{
				Enabled:              false,
				ServiceName:          "hypeman-test",
				ServiceInstanceID:    "test-instance",
				MetricExportInterval: "60s",
				Version:              "test",
				Env:                  "test",
			},
		},
		{
			name: "otel push enabled but misconfigured endpoint",
			cfg: Config{
				Enabled:              true,
				Endpoint:             "://bad-endpoint",
				ServiceName:          "hypeman-test",
				ServiceInstanceID:    "test-instance",
				Insecure:             true,
				MetricExportInterval: "5s",
				Version:              "test",
				Env:                  "test",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, shutdown, err := Init(context.Background(), tt.cfg)
			if err != nil {
				t.Fatalf("init telemetry: %v", err)
			}
			t.Cleanup(func() {
				_ = shutdown(context.Background())
			})

			if provider.MetricsHandler == nil {
				t.Fatalf("expected metrics handler")
			}

			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			provider.MetricsHandler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d", rr.Code)
			}
			if !strings.Contains(rr.Header().Get("Content-Type"), "text/plain") {
				t.Fatalf("expected Prometheus text content type, got %q", rr.Header().Get("Content-Type"))
			}
			if !strings.Contains(rr.Body.String(), "hypeman_uptime_seconds") {
				t.Fatalf("expected hypeman_uptime_seconds metric in output")
			}
		})
	}
}
