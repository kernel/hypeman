package middleware

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// HTTPMetrics holds the OTel metrics for HTTP requests.
type HTTPMetrics struct {
	requestsTotal   metric.Int64Counter
	requestDuration metric.Float64Histogram
}

// NewHTTPMetrics creates new HTTP metrics instruments.
func NewHTTPMetrics(meter metric.Meter) (*HTTPMetrics, error) {
	requestsTotal, err := meter.Int64Counter(
		"hypeman_http_requests_total",
		metric.WithDescription("Total number of HTTP requests"),
	)
	if err != nil {
		return nil, err
	}

	requestDuration, err := meter.Float64Histogram(
		"hypeman_http_request_duration_seconds",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	return &HTTPMetrics{
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
	}, nil
}

// Middleware returns an HTTP middleware that records metrics.
func (m *HTTPMetrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &responseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		// Process request
		next.ServeHTTP(wrapped, r)

		// Calculate duration
		duration := time.Since(start).Seconds()

		// Get route pattern if available (chi specific)
		routePattern := chi.RouteContext(r.Context()).RoutePattern()
		if routePattern == "" {
			routePattern = r.URL.Path
		}

		// Record metrics
		attrs := []attribute.KeyValue{
			attribute.String("method", r.Method),
			attribute.String("path", routePattern),
			attribute.Int("status", wrapped.statusCode),
		}

		m.requestsTotal.Add(r.Context(), 1, metric.WithAttributes(attrs...))
		m.requestDuration.Record(r.Context(), duration, metric.WithAttributes(attrs...))
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Unwrap provides access to the underlying ResponseWriter for http.ResponseController.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// NoopHTTPMetrics returns a middleware that does nothing (for when OTel is disabled).
func NoopHTTPMetrics() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return next
	}
}
