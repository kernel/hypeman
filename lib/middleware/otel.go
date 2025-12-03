package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/onkernel/hypeman/lib/logger"
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

// AccessLogger returns a middleware that logs HTTP requests using slog with trace context.
// This replaces chi's middleware.Logger to get logs into OTel/Loki with trace correlation.
func AccessLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code and bytes
			wrapped := &accessLogWriter{
				ResponseWriter: w,
				statusCode:     http.StatusOK,
			}

			// Process request
			next.ServeHTTP(wrapped, r)

			// Get route pattern
			routePattern := chi.RouteContext(r.Context()).RoutePattern()
			if routePattern == "" {
				routePattern = r.URL.Path
			}

			// Log with trace context from request context
			duration := time.Since(start)
			log.InfoContext(r.Context(),
				fmt.Sprintf("%s %s %d %dB %dms", r.Method, routePattern, wrapped.statusCode, wrapped.bytesWritten, duration.Milliseconds()),
				"method", r.Method,
				"path", routePattern,
				"status", wrapped.statusCode,
				"bytes", wrapped.bytesWritten,
				"duration_ms", duration.Milliseconds(),
				"remote_addr", r.RemoteAddr,
			)
		})
	}
}

// accessLogWriter wraps http.ResponseWriter to capture status and bytes.
type accessLogWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
}

func (w *accessLogWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessLogWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytesWritten += n
	return n, err
}

func (w *accessLogWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// NewAccessLogger creates an access logger with OTel handler if available.
func NewAccessLogger(otelHandler slog.Handler) *slog.Logger {
	cfg := logger.NewConfig()
	return logger.NewSubsystemLogger(logger.SubsystemAPI, cfg, otelHandler)
}

// InjectLogger returns middleware that adds the logger to the request context.
// This enables handlers to use logger.FromContext(ctx) with trace correlation.
func InjectLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := logger.AddToContext(r.Context(), log)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
