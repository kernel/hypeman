package network

import (
	"context"

	"github.com/kernel/hypeman/lib/hypervisor"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func startNetworkStep(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	return startNetworkStepWithOptions(ctx, name, nil, attrs...)
}

func startDetachedNetworkStep(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	return startNetworkStepWithOptions(context.WithoutCancel(ctx), name, []trace.SpanStartOption{
		trace.WithNewRoot(),
	}, attrs...)
}

func startNetworkStepWithOptions(ctx context.Context, name string, options []trace.SpanStartOption, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	inherited := hypervisor.TraceAttributesFromContext(ctx)
	if len(inherited) > 0 {
		merged := make([]attribute.KeyValue, 0, len(inherited)+len(attrs))
		merged = append(merged, inherited...)
		merged = append(merged, attrs...)
		attrs = merged
	}

	opts := append([]trace.SpanStartOption(nil), options...)
	if len(attrs) > 0 {
		opts = append(opts, trace.WithAttributes(attrs...))
	}
	ctx, span := otel.Tracer("hypeman/network").Start(ctx, name, opts...)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}
