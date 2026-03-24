package instances

import (
	"context"

	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func (m *manager) tracerOrDefault() trace.Tracer {
	if m != nil && m.tracer != nil {
		return m.tracer
	}
	return otel.Tracer("hypeman/instances")
}

func (m *manager) startLifecycleSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	ctx = hypervisor.WithTraceAttributes(ctx, propagatedTraceAttributes(attrs...)...)
	return startInstancesSpan(ctx, m.tracerOrDefault(), name, attrs...)
}

func (m *manager) startLifecycleStep(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, span := startInstancesSpan(ctx, m.tracerOrDefault(), name, attrs...)
	return ctx, func(err error) {
		finishInstancesSpan(span, err)
	}
}

func startInstancesSpan(ctx context.Context, tracer trace.Tracer, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if len(attrs) == 0 {
		return tracer.Start(ctx, name)
	}
	return tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

func propagatedTraceAttributes(attrs ...attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		switch string(attr.Key) {
		case "instance_id", "hypervisor", "snapshot_id":
			out = append(out, attr)
		}
	}
	return out
}

func finishInstancesSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}
