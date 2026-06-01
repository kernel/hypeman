package hypervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type fakeHypervisor struct{}
type fakeHypervisorGetVMInfoError struct{}
type fakeRestoredResumedHypervisor struct {
	fakeHypervisor
}

func (fakeHypervisor) DeleteVM(context.Context) error { return nil }
func (fakeHypervisor) Shutdown(context.Context) error { return nil }
func (fakeHypervisor) GetVMInfo(context.Context) (*VMInfo, error) {
	return &VMInfo{State: StateRunning}, nil
}
func (fakeHypervisor) Pause(context.Context) error               { return nil }
func (fakeHypervisor) Resume(context.Context) error              { return nil }
func (fakeHypervisor) Snapshot(context.Context, string) error    { return nil }
func (fakeHypervisor) ResizeMemory(context.Context, int64) error { return nil }
func (fakeHypervisor) ResizeMemoryAndWait(context.Context, int64, time.Duration) error {
	return nil
}
func (fakeHypervisor) SetTargetGuestMemoryBytes(context.Context, int64) error { return nil }
func (fakeHypervisor) GetTargetGuestMemoryBytes(context.Context) (int64, error) {
	return 0, nil
}
func (fakeHypervisor) Capabilities() Capabilities                   { return Capabilities{} }
func (fakeRestoredResumedHypervisor) RestoredResumed() bool         { return true }
func (fakeHypervisorGetVMInfoError) DeleteVM(context.Context) error { return nil }
func (fakeHypervisorGetVMInfoError) Shutdown(context.Context) error { return nil }
func (fakeHypervisorGetVMInfoError) GetVMInfo(context.Context) (*VMInfo, error) {
	return nil, errors.New("vm info failed")
}
func (fakeHypervisorGetVMInfoError) Pause(context.Context) error            { return nil }
func (fakeHypervisorGetVMInfoError) Resume(context.Context) error           { return nil }
func (fakeHypervisorGetVMInfoError) Snapshot(context.Context, string) error { return nil }
func (fakeHypervisorGetVMInfoError) ResizeMemory(context.Context, int64) error {
	return nil
}
func (fakeHypervisorGetVMInfoError) ResizeMemoryAndWait(context.Context, int64, time.Duration) error {
	return nil
}
func (fakeHypervisorGetVMInfoError) SetTargetGuestMemoryBytes(context.Context, int64) error {
	return nil
}
func (fakeHypervisorGetVMInfoError) GetTargetGuestMemoryBytes(context.Context) (int64, error) {
	return 0, nil
}
func (fakeHypervisorGetVMInfoError) Capabilities() Capabilities { return Capabilities{} }

type fakeStarter struct {
	returned Hypervisor
}

func (s fakeStarter) SocketName() string { return "fake.sock" }
func (s fakeStarter) GetBinaryPath(*paths.Paths, string) (string, error) {
	return "", nil
}
func (s fakeStarter) GetVersion(*paths.Paths) (string, error) { return "test", nil }
func (s fakeStarter) StartVM(context.Context, *paths.Paths, string, string, VMConfig) (int, Hypervisor, error) {
	return 42, s.returned, nil
}
func (s fakeStarter) RestoreVM(context.Context, *paths.Paths, string, string, string) (int, Hypervisor, error) {
	return 43, s.returned, nil
}
func (s fakeStarter) PrepareFork(context.Context, ForkPrepareRequest) (ForkPrepareResult, error) {
	return ForkPrepareResult{}, nil
}

func TestWrapHypervisorCreatesChildSpan(t *testing.T) {
	recorder, provider := newTestTracerProvider(t)
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	ctx = WithTraceAttributes(ctx,
		attribute.String("instance_id", "inst_123"),
		attribute.String("hypervisor", string(TypeQEMU)),
	)

	hv := WrapHypervisor(TypeQEMU, fakeHypervisor{})
	require.NoError(t, hv.Resume(ctx))
	parent.End()

	child := findSpanByName(t, recorder.Ended(), "hypervisor.resume")
	require.Equal(t, parent.SpanContext().SpanID(), child.Parent().SpanID())
	assert.Equal(t, codes.Ok, child.Status().Code)

	attrs := attrsToMap(child.Attributes())
	assert.Equal(t, "inst_123", attrs["instance_id"])
	assert.Equal(t, string(TypeQEMU), attrs["hypervisor"])
	assert.Equal(t, "resume", attrs["operation"])

	_ = provider
}

func TestWrapVMStarterWrapsReturnedHypervisor(t *testing.T) {
	recorder, _ := newTestTracerProvider(t)
	ctx, parent := otel.Tracer("test").Start(context.Background(), "parent")
	ctx = WithTraceAttributes(ctx, attribute.String("instance_id", "inst_456"))

	starter := WrapVMStarter(TypeCloudHypervisor, fakeStarter{returned: fakeHypervisor{}})
	_, hv, err := starter.StartVM(ctx, nil, "test", "/tmp/socket", VMConfig{})
	require.NoError(t, err)

	require.NoError(t, hv.Resume(ctx))
	parent.End()

	startSpan := findSpanByName(t, recorder.Ended(), "hypervisor.start_vm")
	resumeSpan := findSpanByName(t, recorder.Ended(), "hypervisor.resume")
	require.Equal(t, parent.SpanContext().SpanID(), startSpan.Parent().SpanID())
	require.Equal(t, parent.SpanContext().TraceID(), resumeSpan.SpanContext().TraceID())

	attrs := attrsToMap(resumeSpan.Attributes())
	assert.Equal(t, "inst_456", attrs["instance_id"])
	assert.Equal(t, string(TypeCloudHypervisor), attrs["hypervisor"])
}

func TestWrapHypervisorPreservesRestoredResumed(t *testing.T) {
	hv := WrapHypervisor(TypeFirecracker, fakeRestoredResumedHypervisor{})
	require.True(t, RestoredResumed(hv))
}

func TestWrapHypervisorSkipsGetVMInfoTraceByDefault(t *testing.T) {
	recorder, _ := newTestTracerProvider(t)

	hv := WrapHypervisor(TypeQEMU, fakeHypervisor{})
	_, err := hv.GetVMInfo(context.Background())
	require.NoError(t, err)

	for _, span := range recorder.Ended() {
		if span.Name() == "hypervisor.get_vm_info" {
			t.Fatalf("expected get vm info to be skipped by default")
		}
	}
}

func TestWrapHypervisorCreatesDetachedErrorSpanForGetVMInfoFailures(t *testing.T) {
	recorder, _ := newTestTracerProvider(t)

	ctx := WithTraceAttributes(context.Background(), attribute.String("instance_id", "inst_999"))
	hv := WrapHypervisor(TypeQEMU, fakeHypervisorGetVMInfoError{})
	_, err := hv.GetVMInfo(ctx)
	require.Error(t, err)

	span := findSpanByName(t, recorder.Ended(), "hypervisor.get_vm_info")
	require.False(t, span.Parent().IsValid())

	attrs := attrsToMap(span.Attributes())
	assert.Equal(t, "inst_999", attrs["instance_id"])
	assert.Equal(t, string(TypeQEMU), attrs["hypervisor"])
	assert.Equal(t, "error_only", attrs["sampled_from"])
}

func newTestTracerProvider(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return recorder, provider
}

func findSpanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func attrsToMap(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[string(attr.Key)] = attr.Value.Emit()
	}
	return out
}
