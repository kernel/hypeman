package otel

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type successfulGETSampler struct {
	getSampler sdktrace.Sampler
	ratio      float64
}

func newSuccessfulGETSampler(ratio float64) sdktrace.Sampler {
	return sdktrace.ParentBased(&successfulGETSampler{
		getSampler: sdktrace.TraceIDRatioBased(ratio),
		ratio:      ratio,
	})
}

func (s *successfulGETSampler) ShouldSample(params sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if attrValueFromAttrs(params.Attributes, "sampled_from") != "" {
		return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
	}
	if params.Kind == trace.SpanKindServer && httpMethodFromAttrs(params.Attributes) == http.MethodGet {
		return s.getSampler.ShouldSample(params)
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (s *successfulGETSampler) Description() string {
	return fmt.Sprintf("ParentBased{successful_get_ratio=%.4f}", s.ratio)
}

func httpMethodFromAttrs(attrs []attribute.KeyValue) string {
	return attrValueFromAttrs(attrs, "http.method", "http.request.method")
}

func attrValueFromAttrs(attrs []attribute.KeyValue, keys ...string) string {
	for _, attr := range attrs {
		for _, key := range keys {
			if string(attr.Key) == key {
				return attr.Value.AsString()
			}
		}
	}
	return ""
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusCapturingResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusCapturingResponseWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

// NewSuccessfulGETErrorTraceMiddleware records a compact fallback span for GET
// requests that were sampled out but still returned an error status.
func NewSuccessfulGETErrorTraceMiddleware(serviceName string) func(http.Handler) http.Handler {
	tracer := otelapi.Tracer(serviceName + "/http")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				next.ServeHTTP(w, r)
				return
			}

			rw := &statusCapturingResponseWriter{
				ResponseWriter: w,
				status:         http.StatusOK,
			}
			next.ServeHTTP(rw, r)

			if rw.status < http.StatusBadRequest {
				return
			}

			spanCtx := trace.SpanContextFromContext(r.Context())
			if spanCtx.IsValid() && spanCtx.IsSampled() {
				return
			}

			route := ""
			if routeCtx := chi.RouteContext(r.Context()); routeCtx != nil {
				route = routeCtx.RoutePattern()
			}
			if route == "" {
				route = r.URL.Path
			}
			if route == "" {
				route = "/"
			}

			_, span := tracer.Start(
				context.Background(),
				route,
				trace.WithNewRoot(),
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.route", route),
					attribute.String("url.path", r.URL.Path),
					attribute.Int("http.status_code", rw.status),
					attribute.String("sampled_from", "unsampled_get_error"),
				),
			)
			span.SetStatus(codes.Error, http.StatusText(rw.status))
			span.End()
		})
	}
}
