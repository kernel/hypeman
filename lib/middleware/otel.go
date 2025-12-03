package middleware

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
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

		// Wrap response writer to capture status code and bytes
		wrapped := wrapResponseWriter(w)

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
			attribute.Int("status", wrapped.Status()),
		}

		m.requestsTotal.Add(r.Context(), 1, metric.WithAttributes(attrs...))
		m.requestDuration.Record(r.Context(), duration, metric.WithAttributes(attrs...))
	})
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
			wrapped := wrapResponseWriter(w)

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
				fmt.Sprintf("%s %s %d %dB %dms", r.Method, routePattern, wrapped.Status(), wrapped.BytesWritten(), duration.Milliseconds()),
				"method", r.Method,
				"path", routePattern,
				"status", wrapped.Status(),
				"bytes", wrapped.BytesWritten(),
				"duration_ms", duration.Milliseconds(),
				"remote_addr", r.RemoteAddr,
			)
		})
	}
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

// responseWriter wraps http.ResponseWriter to capture status code and bytes written.
// It also implements http.Flusher and http.Hijacker when the underlying writer supports them.
type responseWriter struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int
	wroteHeader  bool
}

// wrapResponseWriter creates a new responseWriter wrapper.
func wrapResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}
}

// Status returns the HTTP status code.
func (rw *responseWriter) Status() int {
	return rw.statusCode
}

// BytesWritten returns the number of bytes written.
func (rw *responseWriter) BytesWritten() int {
	return rw.bytesWritten
}

// WriteHeader captures the status code before calling the underlying WriteHeader.
func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.statusCode = code
		rw.wroteHeader = true
		rw.ResponseWriter.WriteHeader(code)
	}
}

// Write captures bytes written and calls the underlying Write.
func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += n
	return n, err
}

// Unwrap provides access to the underlying ResponseWriter for http.ResponseController.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Flush implements http.Flusher. It delegates to the underlying writer if supported.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker. It delegates to the underlying writer if supported.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not implement http.Hijacker")
}
