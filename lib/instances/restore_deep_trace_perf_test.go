//go:build linux

package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

const restoreDeepTracePerfEnv = "HYPEMAN_RUN_RESTORE_DEEP_TRACE_PERF"
const restoreDeepTracePerfItersEnv = "HYPEMAN_RESTORE_DEEP_TRACE_PERF_ITERS"
const guestResumeNetworkDebugStagesEnv = "HYPEMAN_RESUME_NETWORK_DEBUG_STAGES"

func TestRestoreDeepTracePerf(t *testing.T) {
	if os.Getenv(restoreDeepTracePerfEnv) != "1" {
		t.Skipf("set %s=1 to run restore deep trace perf test", restoreDeepTracePerfEnv)
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
	traceDir := strings.TrimSpace(os.Getenv(restoreDeepTraceDirEnv))
	if traceDir == "" {
		traceDir = filepath.Join(os.TempDir(), "hypeman-restore-debug-test")
		t.Setenv(restoreDeepTraceDirEnv, traceDir)
	}
	require.NoError(t, os.RemoveAll(traceDir))

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
		guestResumeNetworkMailboxTokenEnv: fmt.Sprintf("deep-%d", time.Now().UnixNano()),
		guestResumeNetworkDebugStagesEnv:  "1",
	}
	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-deep-trace-src",
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
		Name: "fc-deep-trace-snap",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteSnapshot(context.Background(), snapshot.Id) })

	t.Setenv(restoreDeepTraceEnv, "1")
	waitForNetwork := true
	iterations := restoreDeepTracePerfIterations(t, 3)
	for i := 1; i <= iterations; i++ {
		beforeSpanCount := len(recorder.Ended())
		beforeTraceDirs := currentRestoreDeepTraceDirs(t, traceDir)
		start := time.Now()
		fork, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
			Name:           fmt.Sprintf("fc-deep-trace-%02d", i),
			TargetState:    StateRunning,
			WaitForNetwork: &waitForNetwork,
		})
		forkElapsed := time.Since(start)
		require.NoError(t, err)
		require.Equal(t, StateRunning, fork.State)

		spans := append([]sdktrace.ReadOnlySpan(nil), recorder.Ended()[beforeSpanCount:]...)
		tracePath := newestRestoreDeepTraceDir(t, traceDir, beforeTraceDirs)
		t.Log(formatRestoreDeepTracePerfLine(i, forkElapsed, spans, tracePath))

		require.NoError(t, waitForExecAgent(ctx, mgr, fork.Id, 45*time.Second))
		_ = mgr.DeleteInstance(context.Background(), fork.Id)
	}
}

func restoreDeepTracePerfIterations(t *testing.T, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(restoreDeepTracePerfItersEnv))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.Positive(t, n)
	return n
}

func currentRestoreDeepTraceDirs(t *testing.T, traceDir string) map[string]struct{} {
	t.Helper()
	out := make(map[string]struct{})
	entries, err := os.ReadDir(traceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		require.NoError(t, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			out[entry.Name()] = struct{}{}
		}
	}
	return out
}

func newestRestoreDeepTraceDir(t *testing.T, traceDir string, before map[string]struct{}) string {
	t.Helper()
	entries, err := os.ReadDir(traceDir)
	require.NoError(t, err)
	var newest string
	var newestMod time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, exists := before[entry.Name()]; exists {
			continue
		}
		info, err := entry.Info()
		require.NoError(t, err)
		if newest == "" || info.ModTime().After(newestMod) {
			newest = filepath.Join(traceDir, entry.Name())
			newestMod = info.ModTime()
		}
	}
	return newest
}

func formatRestoreDeepTracePerfLine(iter int, forkElapsed time.Duration, spans []sdktrace.ReadOnlySpan, tracePath string) string {
	return fmt.Sprintf(
		"DEEP_TRACE iter=%d fork_total_ms=%d restore_from_snapshot_ms=%d resume_vm_ms=%d fault_guest_memory_from_disk_ms=%d guest_resume_network_fault_guest_memory_from_disk_ms=%d trace_dir=%s",
		iter,
		forkElapsed.Milliseconds(),
		restoreDeepTraceSpanDurationMS(restoreDeepTraceLastSpanNamed(spans, "restore_from_snapshot")),
		restoreDeepTraceSpanDurationMS(restoreDeepTraceLastSpanNamed(spans, "resume_vm")),
		restoreDeepTraceSpanDurationMS(restoreDeepTraceLastSpanNamed(spans, "fault_guest_memory_from_disk")),
		restoreDeepTraceSpanDurationMS(restoreDeepTraceLastSpanNamed(spans, "guest.resume_network.fault_guest_memory_from_disk")),
		tracePath,
	)
}

func restoreDeepTraceLastSpanNamed(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name() == name {
			return spans[i]
		}
	}
	return nil
}

func restoreDeepTraceSpanDurationMS(span sdktrace.ReadOnlySpan) int64 {
	if span == nil {
		return -1
	}
	return span.EndTime().Sub(span.StartTime()).Milliseconds()
}
