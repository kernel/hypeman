package vmm

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestMetricsRoundTripperCreatesTraceSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})

	rt := &metricsRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Status:     "204 No Content",
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}),
		tracer: otel.Tracer("hypeman/vmm"),
	}

	ctx := hypervisor.WithTraceAttributes(context.Background(),
		attribute.String("instance_id", "inst_789"),
		attribute.String("hypervisor", string(hypervisor.TypeCloudHypervisor)),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/vm.resume", nil)
	require.NoError(t, err)

	_, err = rt.RoundTrip(req)
	require.NoError(t, err)

	var found sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "hypervisor.http PUT /api/v1/vm.resume" {
			found = span
			break
		}
	}
	require.NotNil(t, found)

	attrs := make(map[string]string, len(found.Attributes()))
	for _, attr := range found.Attributes() {
		attrs[string(attr.Key)] = attr.Value.Emit()
	}

	assert.Equal(t, "inst_789", attrs["instance_id"])
	assert.Equal(t, string(hypervisor.TypeCloudHypervisor), attrs["hypervisor"])
	assert.Equal(t, "PUT /api/v1/vm.resume", attrs["operation"])
	assert.Equal(t, "204", attrs["http.status_code"])
}
