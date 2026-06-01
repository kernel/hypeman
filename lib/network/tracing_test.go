package network

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestDetachedNetworkStepStartsNewTrace(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	ctx, parent := otel.Tracer("test").Start(context.Background(), "root")
	ctx = hypervisor.WithTraceAttributes(ctx, attribute.String("instance_id", "inst-1"))

	detachedCtx, detachedEnd := startDetachedNetworkStep(ctx, "network.rate_limit.wait_for_tc_mutex",
		attribute.String("operation", "wait_for_tc_mutex"),
	)
	_, childEnd := startNetworkStep(detachedCtx, "network.rate_limit.apply",
		attribute.String("operation", "apply_rate_limit"),
	)
	childEnd(nil)
	detachedEnd(nil)
	parent.End()

	root := networkSpanByName(t, recorder.Ended(), "root")
	detached := networkSpanByName(t, recorder.Ended(), "network.rate_limit.wait_for_tc_mutex")
	child := networkSpanByName(t, recorder.Ended(), "network.rate_limit.apply")

	require.False(t, detached.Parent().IsValid())
	assert.NotEqual(t, root.SpanContext().TraceID(), detached.SpanContext().TraceID())
	assert.Equal(t, detached.SpanContext().TraceID(), child.SpanContext().TraceID())
	assert.Equal(t, detached.SpanContext().SpanID(), child.Parent().SpanID())

	attrs := networkAttrsToMap(child.Attributes())
	assert.Equal(t, "inst-1", attrs["instance_id"])
	assert.Equal(t, "apply_rate_limit", attrs["operation"])
}

func networkSpanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func networkAttrsToMap(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[string(attr.Key)] = attr.Value.AsString()
	}
	return out
}
