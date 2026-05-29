//go:build linux

package instances

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	snapshottest "github.com/kernel/hypeman/lib/snapshot/testsupport"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const reconfigureGuestNetworkPerfEnv = "HYPEMAN_RUN_RECONFIGURE_GUEST_NETWORK_PERF"

func TestReconfigureGuestNetworkPerf(t *testing.T) {
	if os.Getenv(reconfigureGuestNetworkPerfEnv) != "1" {
		t.Skipf("set %s=1 to run snapshot fork network reconfigure perf test", reconfigureGuestNetworkPerfEnv)
	}
	requireFirecrackerIntegrationPrereqs(t)

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		_ = provider.Shutdown(context.Background())
	})

	iterations := perfIterationsFromEnv(t, 5)
	mgr, tmpDir := setupTestManagerForFirecracker(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	imageName := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, imageName)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))
	require.NoError(t, mgr.networkManager.Initialize(ctx, nil))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-reconfigure-perf-src",
		Image:          imageName,
		Size:           1024 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeFirecracker,
		Cmd:            []string{"sleep", "infinity"},
	})
	require.NoError(t, err)
	sourceID := source.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), sourceID) })

	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, integrationTestTimeout(45*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 45*time.Second))

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "fc-reconfigure-perf-snap",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteSnapshot(context.Background(), snapshot.Id) })

	for i := 1; i <= iterations; i++ {
		before := len(recorder.Ended())
		start := time.Now()
		fork, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
			Name:        fmt.Sprintf("fc-reconfigure-perf-fork-%02d", i),
			TargetState: StateRunning,
		})
		forkElapsed := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, StateRunning, fork.State)
		require.NoError(t, waitForExecAgent(ctx, mgr, fork.Id, 45*time.Second))

		spans := append([]sdktrace.ReadOnlySpan(nil), recorder.Ended()[before:]...)
		t.Log(formatReconfigurePerfLine(i, forkElapsed, spans))
		_ = mgr.DeleteInstance(ctx, fork.Id)
	}
}

func perfIterationsFromEnv(t *testing.T, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("HYPEMAN_RECONFIGURE_GUEST_NETWORK_PERF_ITERS"))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.Positive(t, n)
	return n
}

func formatReconfigurePerfLine(iter int, forkElapsed time.Duration, spans []sdktrace.ReadOnlySpan) string {
	reconfigure := lastSpanNamed(spans, "reconfigure_guest_network")
	if reconfigure == nil {
		return fmt.Sprintf("PERF iter=%d fork_total_ms=%d reconfigure_guest_network_ms=missing spans=%d", iter, forkElapsed.Milliseconds(), len(spans))
	}

	desc := descendantSpans(spans, reconfigure)
	exec := lastSpanNamed(desc, "guest.exec")
	networkRPC := lastSpanNamed(desc, "guest.reconfigure_network")
	getConn := lastSpanNamed(desc, "guest.exec.get_conn")
	openStream := lastSpanNamed(desc, "guest.exec.open_stream")
	sendStart := lastSpanNamed(desc, "guest.exec.send_start")
	recvUntilExit := lastSpanNamed(desc, "guest.exec.recv_until_exit")

	return fmt.Sprintf(
		"PERF iter=%d fork_total_ms=%d reconfigure_guest_network_ms=%d guest_exec_ms=%d guest_network_rpc_ms=%d get_conn_ms=%d open_stream_ms=%d send_start_ms=%d recv_until_exit_ms=%d attempts=%d retryable_attempts=%d network_rpc_attempts=%d network_rpc_retryable_attempts=%d first_retryable_error=%s last_retryable_error=%s wait_elapsed_ms=%d open_stream_attempts=%s recv_attempts=%s unary_attempts=%s",
		iter,
		forkElapsed.Milliseconds(),
		spanDurationMS(reconfigure),
		spanDurationMS(exec),
		spanDurationMS(networkRPC),
		spanDurationMS(getConn),
		spanDurationMS(openStream),
		spanDurationMS(sendStart),
		spanDurationMS(recvUntilExit),
		spanAttrInt(exec, "attempts"),
		spanAttrInt(exec, "retryable_attempts"),
		spanAttrInt(networkRPC, "attempts"),
		spanAttrInt(networkRPC, "retryable_attempts"),
		firstNonEmpty(spanAttrString(exec, "first_retryable_error_type"), spanAttrString(networkRPC, "first_retryable_error_type")),
		firstNonEmpty(spanAttrString(exec, "last_retryable_error_type"), spanAttrString(networkRPC, "last_retryable_error_type")),
		firstNonNegative(spanAttrInt(exec, "wait_elapsed_ms"), spanAttrInt(networkRPC, "wait_elapsed_ms")),
		spanDurationsByName(desc, "guest.exec.open_stream"),
		spanDurationsByName(desc, "guest.exec.recv_until_exit"),
		spanDurationsByName(desc, "guest.reconfigure_network.rpc"),
	)
}

func descendantSpans(spans []sdktrace.ReadOnlySpan, root sdktrace.ReadOnlySpan) []sdktrace.ReadOnlySpan {
	if root == nil {
		return nil
	}
	descendantIDs := map[string]bool{root.SpanContext().SpanID().String(): true}
	var out []sdktrace.ReadOnlySpan
	for {
		added := false
		for _, span := range spans {
			id := span.SpanContext().SpanID().String()
			if descendantIDs[id] {
				continue
			}
			parent := span.Parent().SpanID().String()
			if descendantIDs[parent] {
				descendantIDs[id] = true
				out = append(out, span)
				added = true
			}
		}
		if !added {
			return out
		}
	}
}

func lastSpanNamed(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name() == name {
			return spans[i]
		}
	}
	return nil
}

func spanDurationMS(span sdktrace.ReadOnlySpan) int64 {
	if span == nil {
		return -1
	}
	return span.EndTime().Sub(span.StartTime()).Milliseconds()
}

func spanAttrInt(span sdktrace.ReadOnlySpan, key string) int64 {
	if span == nil {
		return -1
	}
	for _, attr := range span.Attributes() {
		if string(attr.Key) != key {
			continue
		}
		switch attr.Value.Type() {
		case attribute.INT64:
			return attr.Value.AsInt64()
		case attribute.INT64SLICE:
			return -1
		default:
			return -1
		}
	}
	return -1
}

func spanAttrString(span sdktrace.ReadOnlySpan, key string) string {
	if span == nil {
		return "missing"
	}
	for _, attr := range span.Attributes() {
		if string(attr.Key) == key {
			return attr.Value.AsString()
		}
	}
	return ""
}

func spanDurationsByName(spans []sdktrace.ReadOnlySpan, name string) string {
	var parts []string
	for _, span := range spans {
		if span.Name() != name {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d/%s", spanDurationMS(span), span.Status().Code.String()))
	}
	if len(parts) == 0 {
		return "[]"
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" && value != "missing" {
			return value
		}
	}
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

func firstNonNegative(values ...int64) int64 {
	for _, value := range values {
		if value >= 0 {
			return value
		}
	}
	return -1
}
