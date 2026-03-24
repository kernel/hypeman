package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/riandyrn/otelchi"
	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestSuccessfulGETSamplerDropsSuccessfulGETRequests(t *testing.T) {
	recorder, router, shutdown := newHTTPTraceTestHarness(t, 0)
	defer shutdown()

	router.Get("/instances", func(w http.ResponseWriter, r *http.Request) {})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/instances", nil)
	router.ServeHTTP(rr, req)

	if got := len(recorder.Ended()); got != 0 {
		t.Fatalf("expected no spans for sampled-out successful GET, got %d", got)
	}
}

func TestSuccessfulGETSamplerKeepsSuccessfulPOSTRequests(t *testing.T) {
	recorder, router, shutdown := newHTTPTraceTestHarness(t, 0)
	defer shutdown()

	router.Post("/instances", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/instances", nil)
	router.ServeHTTP(rr, req)

	span := findEndedSpanByName(t, recorder.Ended(), "/instances")
	if got := attrValue(span.Attributes(), "http.method"); got != http.MethodPost {
		t.Fatalf("expected POST span, got attrs %v", span.Attributes())
	}
}

func newHTTPTraceTestHarness(t *testing.T, getRatio float64) (*tracetest.SpanRecorder, *chi.Mux, func()) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(newSuccessfulGETSampler(getRatio)),
		sdktrace.WithSpanProcessor(recorder),
	)
	previous := otelapi.GetTracerProvider()
	otelapi.SetTracerProvider(provider)

	router := chi.NewRouter()
	router.Use(otelchi.Middleware("hypeman-test", otelchi.WithChiRoutes(router)))

	return recorder, router, func() {
		otelapi.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	}
}

func findEndedSpanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func attrValue(attrs []attribute.KeyValue, key string) string {
	for _, attr := range attrs {
		if string(attr.Key) == key {
			return attr.Value.Emit()
		}
	}
	return ""
}
