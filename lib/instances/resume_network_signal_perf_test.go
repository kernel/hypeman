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

const resumeNetworkSignalPerfEnv = "HYPEMAN_RUN_RESUME_NETWORK_SIGNAL_PERF"
const resumeNetworkSignalPerfItersEnv = "HYPEMAN_RESUME_NETWORK_SIGNAL_PERF_ITERS"
const resumeNetworkSignalPerfWaitEnv = "HYPEMAN_RESUME_NETWORK_SIGNAL_PERF_WAIT_FOR_NETWORK"
const resumeNetworkSignalGuestEnv = "HYPEMAN_RESUME_NETWORK_SIGNAL"
const resumeNetworkAckStagesGuestEnv = "HYPEMAN_RESUME_NETWORK_ACK_STAGES"

func TestResumeNetworkSignalPerf(t *testing.T) {
	if os.Getenv(resumeNetworkSignalPerfEnv) != "1" {
		t.Skipf("set %s=1 to run resume network signal perf test", resumeNetworkSignalPerfEnv)
	}
	requireFirecrackerIntegrationPrereqs(t)

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		_ = provider.Shutdown(context.Background())
	})

	ctx := context.Background()
	mgr, tmpDir := setupTestManagerForFirecracker(t)
	p := paths.New(tmpDir)
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	imageName := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, imageName)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))
	require.NoError(t, mgr.networkManager.Initialize(ctx, nil))

	signal := strings.TrimSpace(os.Getenv(resumeNetworkSignalGuestEnv))
	if signal == "" {
		signal = "auto"
	}
	waitForNetwork := strings.TrimSpace(os.Getenv(resumeNetworkSignalPerfWaitEnv)) != "0"
	env := map[string]string{
		guestResumeNetworkMailboxEnv:      "1",
		guestResumeNetworkMailboxTokenEnv: fmt.Sprintf("perf-%d", time.Now().UnixNano()),
		resumeNetworkSignalGuestEnv:       signal,
		resumeNetworkAckStagesGuestEnv:    "1",
	}

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-rn-signal-src",
		Image:          imageName,
		Size:           1024 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeFirecracker,
		Cmd:            []string{"sleep", "infinity"},
		Env:            env,
	})
	require.NoError(t, err)
	sourceID := source.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), sourceID) })

	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, integrationTestTimeout(45*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 45*time.Second))

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "fc-rn-signal-snap",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteSnapshot(context.Background(), snapshot.Id) })

	iterations := resumeNetworkSignalPerfIterations(t, 10)
	for i := 1; i <= iterations; i++ {
		before := len(recorder.Ended())
		start := time.Now()
		fork, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
			Name:           fmt.Sprintf("fc-rn-signal-%02d", i),
			TargetState:    StateRunning,
			WaitForNetwork: &waitForNetwork,
		})
		forkElapsed := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, StateRunning, fork.State)

		spans := append([]sdktrace.ReadOnlySpan(nil), recorder.Ended()[before:]...)
		t.Log(formatResumeNetworkSignalPerfLine(i, signal, waitForNetwork, forkElapsed, spans))

		require.NoError(t, waitForExecAgent(ctx, mgr, fork.Id, 45*time.Second))
		_ = mgr.DeleteInstance(context.Background(), fork.Id)
	}
}

func resumeNetworkSignalPerfIterations(t *testing.T, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(resumeNetworkSignalPerfItersEnv))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.Positive(t, n)
	return n
}

func formatResumeNetworkSignalPerfLine(iter int, signal string, waitForNetwork bool, forkElapsed time.Duration, spans []sdktrace.ReadOnlySpan) string {
	ackWait := lastSpanNamed(spans, "guest.resume_network.udp_ack_wait")
	mailboxAckMS := spanAttrInt64(ackWait, "guest_resume_network_ack_mailbox_ms")
	appliedAckMS := spanAttrInt64(ackWait, "guest_resume_network_ack_applied_ms")
	applyAfterMailboxMS := int64(-1)
	if mailboxAckMS >= 0 && appliedAckMS >= 0 {
		applyAfterMailboxMS = appliedAckMS - mailboxAckMS
	}
	return fmt.Sprintf(
		"PERF_SIGNAL iter=%d signal=%s wait_for_network=%t fork_total_ms=%d restore_from_snapshot_ms=%d resume_vm_ms=%d reconfigure_guest_network_ms=%d guest_resume_network_udp_ack_wait_ms=%d guest_mailbox_ack_ms=%d guest_applied_ack_ms=%d guest_apply_after_mailbox_ms=%d",
		iter,
		signal,
		waitForNetwork,
		forkElapsed.Milliseconds(),
		spanDurationMS(lastSpanNamed(spans, "restore_from_snapshot")),
		spanDurationMS(lastSpanNamed(spans, "resume_vm")),
		spanDurationMS(lastSpanNamed(spans, "reconfigure_guest_network")),
		spanDurationMS(ackWait),
		mailboxAckMS,
		appliedAckMS,
		applyAfterMailboxMS,
	)
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

func spanAttrInt64(span sdktrace.ReadOnlySpan, key string) int64 {
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
		}
	}
	return -1
}
