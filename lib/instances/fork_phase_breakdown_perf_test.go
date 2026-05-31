//go:build linux

package instances

import (
	"context"
	"fmt"
	"os"
	"sort"
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
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const forkPhaseBreakdownPerfEnv = "HYPEMAN_RUN_FORK_PHASE_BREAKDOWN_PERF"
const forkPhaseBreakdownPerfItersEnv = "HYPEMAN_FORK_PHASE_BREAKDOWN_PERF_ITERS"

func TestForkSnapshotPhaseBreakdownPerf(t *testing.T) {
	if os.Getenv(forkPhaseBreakdownPerfEnv) != "1" {
		t.Skipf("set %s=1 to run fork snapshot phase breakdown perf test", forkPhaseBreakdownPerfEnv)
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

	env := map[string]string{
		guestResumeNetworkMailboxEnv:      "1",
		guestResumeNetworkMailboxTokenEnv: fmt.Sprintf("phase-%d", time.Now().UnixNano()),
		guestResumeNetworkDebugStagesEnv:  "1",
	}
	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-phase-src",
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
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), source.Id) })

	source, err = waitForInstanceState(ctx, mgr, source.Id, StateRunning, integrationTestTimeout(45*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, source.Id, 45*time.Second))

	snapshot, err := mgr.CreateSnapshot(ctx, source.Id, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "fc-phase-snap",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteSnapshot(context.Background(), snapshot.Id) })

	waitForNetwork := true
	iterations := forkPhaseBreakdownPerfIterations(t, 3)
	for i := 1; i <= iterations; i++ {
		beforeSpanCount := len(recorder.Ended())
		start := time.Now()
		fork, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
			Name:           fmt.Sprintf("fc-phase-%02d", i),
			TargetState:    StateRunning,
			WaitForNetwork: &waitForNetwork,
		})
		forkElapsed := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, StateRunning, fork.State)

		spans := append([]sdktrace.ReadOnlySpan(nil), recorder.Ended()[beforeSpanCount:]...)
		sort.Slice(spans, func(i, j int) bool {
			return spans[i].StartTime().Before(spans[j].StartTime())
		})
		parts := make([]string, 0, len(spans))
		for _, span := range spans {
			parts = append(parts, fmt.Sprintf("%s=%d", span.Name(), span.EndTime().Sub(span.StartTime()).Milliseconds()))
		}
		t.Logf("FORK_PHASE_BREAKDOWN iter=%d fork_total_ms=%d spans=%s", i, forkElapsed.Milliseconds(), strings.Join(parts, ","))

		require.NoError(t, waitForExecAgent(ctx, mgr, fork.Id, 45*time.Second))
		_ = mgr.DeleteInstance(context.Background(), fork.Id)
	}
}

func forkPhaseBreakdownPerfIterations(t *testing.T, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(forkPhaseBreakdownPerfItersEnv))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.Positive(t, n)
	return n
}
