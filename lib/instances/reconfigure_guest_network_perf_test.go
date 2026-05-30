//go:build linux

package instances

import (
	"context"
	"fmt"
	"net"
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
const guestInitiatedResumeNetworkPerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_INITIATED"
const guestInitiatedResumeNetworkPollPerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_POLL_MS"
const guestInitiatedResumeNetworkTriggerPerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_TRIGGER"
const guestInitiatedResumeNetworkPrearmPerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_PREARM"
const guestInitiatedResumeNetworkStartArmedPerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_START_ARMED"
const guestInitiatedResumeNetworkArmedPollPerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_ARMED_POLL_MS"
const guestInitiatedResumeNetworkPrearmSettlePerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_PREARM_SETTLE_MS"
const guestInitiatedResumeNetworkMailboxPerfEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_GUEST_MAILBOX"
const reconfigureGuestNetworkPerfVCPUsEnv = "HYPEMAN_RECONFIGURE_GUEST_NETWORK_PERF_VCPUS"

type resumeNetworkAck struct {
	received time.Time
	text     string
}

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

	var ackCh <-chan resumeNetworkAck
	expectMailboxAck := false
	env := map[string]string(nil)
	if os.Getenv(guestInitiatedResumeNetworkPerfEnv) == "1" {
		env = map[string]string{guestResumeNetworkPortEnv: "2224"}
		ackPort, ch := startResumeNetworkAckListener(t)
		env[guestResumeNetworkAckPortEnv] = strconv.Itoa(ackPort)
		ackCh = ch
		if os.Getenv(guestInitiatedResumeNetworkMailboxPerfEnv) == "1" {
			env["HYPEMAN_RESUME_NETWORK_MAILBOX"] = "1"
			env["HYPEMAN_RESUME_NETWORK_MAILBOX_TOKEN"] = fmt.Sprintf("perf-%d", time.Now().UnixNano())
			expectMailboxAck = true
		}
		if trigger := strings.TrimSpace(os.Getenv(guestInitiatedResumeNetworkTriggerPerfEnv)); trigger != "" {
			env["HYPEMAN_RESUME_NETWORK_TRIGGER"] = trigger
		}
		if os.Getenv(guestInitiatedResumeNetworkPrearmPerfEnv) == "1" {
			env["HYPEMAN_RESUME_NETWORK_PREARM"] = "1"
			env["HYPEMAN_RESUME_NETWORK_SLOW_POLL_INTERVAL_MS"] = "1000"
			env["HYPEMAN_RESUME_NETWORK_ARMED_POLL_INTERVAL_MS"] = "1"
			if armedPollMS := strings.TrimSpace(os.Getenv(guestInitiatedResumeNetworkArmedPollPerfEnv)); armedPollMS != "" {
				env["HYPEMAN_RESUME_NETWORK_ARMED_POLL_INTERVAL_MS"] = armedPollMS
			}
			if os.Getenv(guestInitiatedResumeNetworkStartArmedPerfEnv) == "1" {
				env["HYPEMAN_RESUME_NETWORK_START_ARMED"] = "1"
			}
			if settleMS := strings.TrimSpace(os.Getenv(guestInitiatedResumeNetworkPrearmSettlePerfEnv)); settleMS != "" {
				env["HYPEMAN_RESUME_NETWORK_PREARM_SETTLE_MS"] = settleMS
			}
		}
		if pollMS := strings.TrimSpace(os.Getenv(guestInitiatedResumeNetworkPollPerfEnv)); pollMS != "" {
			env["HYPEMAN_RESUME_NETWORK_POLL_INTERVAL_MS"] = pollMS
		}
	}

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-reconfigure-perf-src",
		Image:          imageName,
		Size:           1024 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          perfVCPUsFromEnv(t, 1),
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

	var sourceResumeNetworkServer *guestResumeNetworkServer
	if os.Getenv(guestInitiatedResumeNetworkPerfEnv) == "1" &&
		os.Getenv(guestInitiatedResumeNetworkPollPerfEnv) == "" &&
		os.Getenv(guestInitiatedResumeNetworkPrearmPerfEnv) != "1" &&
		strings.TrimSpace(os.Getenv(guestInitiatedResumeNetworkTriggerPerfEnv)) == "" {
		port := guestInitiatedResumeNetworkPort(&source.StoredMetadata)
		require.NotZero(t, port)
		sourceResumeNetworkServer, err = startGuestResumeNetworkServer(ctx, source.VsockSocket, port, nil)
		require.NoError(t, err)
		t.Cleanup(sourceResumeNetworkServer.Close)
		armCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		require.NoError(t, sourceResumeNetworkServer.WaitArmed(armCtx))
		cancel()
	}

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
		var mailboxAck *resumeNetworkAck
		var appliedAck *resumeNetworkAck
		if ackCh != nil {
			if expectMailboxAck {
				mailboxAck = waitForResumeNetworkAck(t, ackCh, fork, "mailbox", 2*time.Second)
			}
			appliedAck = waitForResumeNetworkAck(t, ackCh, fork, "applied", 2*time.Second)
		}
		require.NoError(t, waitForExecAgent(ctx, mgr, fork.Id, 45*time.Second))
		requireGuestNetworkIdentity(t, fork)

		spans := append([]sdktrace.ReadOnlySpan(nil), recorder.Ended()[before:]...)
		t.Log(formatReconfigurePerfLine(i, forkElapsed, spans))
		if mailboxAck != nil {
			t.Log(formatResumeNetworkAckLine(i, start, *mailboxAck, spans))
		}
		if appliedAck != nil {
			t.Log(formatResumeNetworkAckLine(i, start, *appliedAck, spans))
		}
		_ = mgr.DeleteInstance(ctx, fork.Id)
	}
}

func requireGuestNetworkIdentity(t *testing.T, inst *Instance) {
	t.Helper()

	output, exitCode, err := execInInstance(context.Background(), inst, "sh", "-c", "printf 'mac='; cat /sys/class/net/eth0/address; printf 'cidrs='; ip -4 -o addr show dev eth0 scope global | awk '{print $4}'")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, "guest network identity command output: %s", output)
	require.Contains(t, strings.ToLower(output), "mac="+strings.ToLower(inst.MAC))
	require.Contains(t, output, inst.IP+"/")
}

func startResumeNetworkAckListener(t *testing.T) (int, <-chan resumeNetworkAck) {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ch := make(chan resumeNetworkAck, 128)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			ch <- resumeNetworkAck{
				received: time.Now(),
				text:     strings.TrimSpace(string(buf[:n])),
			}
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port, ch
}

func waitForResumeNetworkAck(t *testing.T, ch <-chan resumeNetworkAck, inst *Instance, stage string, timeout time.Duration) *resumeNetworkAck {
	t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	wantStage := "stage=" + stage
	wantMAC := "mac=" + strings.ToLower(inst.MAC)
	wantIP := "ip=" + inst.IP
	for {
		select {
		case ack := <-ch:
			text := strings.ToLower(ack.text)
			if strings.Contains(text, wantStage) && strings.Contains(text, wantMAC) && strings.Contains(text, wantIP) {
				return &ack
			}
		case <-timer.C:
			require.Failf(t, "timed out waiting for guest resume network ack", "stage=%s instance=%s mac=%s ip=%s", stage, inst.Id, inst.MAC, inst.IP)
			return nil
		}
	}
}

func formatResumeNetworkAckLine(iter int, forkStart time.Time, ack resumeNetworkAck, spans []sdktrace.ReadOnlySpan) string {
	resume := lastSpanNamed(spans, "resume_vm")
	afterResumeMS := int64(-1)
	if resume != nil {
		afterResumeMS = ack.received.Sub(resume.EndTime()).Milliseconds()
	}
	return fmt.Sprintf(
		"PERF_ACK iter=%d guest_network_ack_after_fork_start_ms=%d guest_network_ack_after_resume_ms=%d ack=%q",
		iter,
		ack.received.Sub(forkStart).Milliseconds(),
		afterResumeMS,
		ack.text,
	)
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

func perfVCPUsFromEnv(t *testing.T, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(reconfigureGuestNetworkPerfVCPUsEnv))
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
	resumeNetworkWait := lastSpanNamed(desc, "guest.resume_network.wait")
	getConn := lastSpanNamed(desc, "guest.exec.get_conn")
	openStream := lastSpanNamed(desc, "guest.exec.open_stream")
	sendStart := lastSpanNamed(desc, "guest.exec.send_start")
	recvUntilExit := lastSpanNamed(desc, "guest.exec.recv_until_exit")
	vsockDial := lastSpanNamed(desc, "hypervisor.vsock.dial")
	vsockUnixDial := lastSpanNamed(desc, "hypervisor.vsock.unix_dial")
	vsockWriteConnect := lastSpanNamed(desc, "hypervisor.vsock.write_connect")
	vsockReadOK := lastSpanNamed(desc, "hypervisor.vsock.read_ok")

	return fmt.Sprintf(
		"PERF iter=%d fork_total_ms=%d reconfigure_guest_network_ms=%d guest_exec_ms=%d guest_network_rpc_ms=%d guest_resume_network_wait_ms=%d get_conn_ms=%d open_stream_ms=%d send_start_ms=%d recv_until_exit_ms=%d vsock_dial_ms=%d vsock_unix_dial_ms=%d vsock_write_connect_ms=%d vsock_read_ok_ms=%d attempts=%d retryable_attempts=%d network_rpc_attempts=%d network_rpc_retryable_attempts=%d first_retryable_error=%s last_retryable_error=%s wait_elapsed_ms=%d open_stream_attempts=%s recv_attempts=%s unary_attempts=%s guest_resume_network_wait_attempts=%s vsock_dial_attempts=%s vsock_unix_dial_attempts=%s vsock_write_connect_attempts=%s vsock_read_ok_attempts=%s",
		iter,
		forkElapsed.Milliseconds(),
		spanDurationMS(reconfigure),
		spanDurationMS(exec),
		spanDurationMS(networkRPC),
		spanDurationMS(resumeNetworkWait),
		spanDurationMS(getConn),
		spanDurationMS(openStream),
		spanDurationMS(sendStart),
		spanDurationMS(recvUntilExit),
		spanDurationMS(vsockDial),
		spanDurationMS(vsockUnixDial),
		spanDurationMS(vsockWriteConnect),
		spanDurationMS(vsockReadOK),
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
		spanDurationsByName(desc, "guest.resume_network.wait"),
		spanDurationsByName(desc, "hypervisor.vsock.dial"),
		spanDurationsByName(desc, "hypervisor.vsock.unix_dial"),
		spanDurationsByName(desc, "hypervisor.vsock.write_connect"),
		spanDurationsByName(desc, "hypervisor.vsock.read_ok"),
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
