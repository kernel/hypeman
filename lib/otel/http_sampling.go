package otel

import (
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
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
	if params.Kind == trace.SpanKindServer && httpMethodFromAttrs(params.Attributes) == http.MethodGet {
		return s.getSampler.ShouldSample(params)
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (s *successfulGETSampler) Description() string {
	return fmt.Sprintf("successfulGETSampler{ratio=%.4f}", s.ratio)
}

func httpMethodFromAttrs(attrs []attribute.KeyValue) string {
	for _, attr := range attrs {
		switch string(attr.Key) {
		case "http.method", "http.request.method":
			return attr.Value.AsString()
		}
	}
	return ""
}
