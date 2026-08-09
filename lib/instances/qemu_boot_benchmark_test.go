package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
)

// TestQEMUBootComparisonReport is an opt-in, non-gating developer benchmark.
// Run scripts/benchmark-qemu-machine-types.sh to compare q35 and microvm.
func TestQEMUBootComparisonReport(t *testing.T) {
	if os.Getenv("HYPEMAN_QEMU_BOOT_BENCH") != "1" {
		t.Skip("set HYPEMAN_QEMU_BOOT_BENCH=1 to run the QEMU boot comparison")
	}
	requireQEMUUsable(t)
	requireMicroVMAvailable(t)
	acquireHeavyIO(t)

	samples := 5
	if raw := os.Getenv("HYPEMAN_QEMU_BOOT_BENCH_SAMPLES"); raw != "" {
		var err error
		samples, err = strconv.Atoi(raw)
		require.NoError(t, err)
		require.Greater(t, samples, 0)
	}

	for _, hypervisorType := range []hypervisor.Type{hypervisor.TypeQEMU, hypervisor.TypeQEMUMicroVM} {
		durations, rss := runQEMUBootSamples(t, hypervisorType, samples)
		t.Logf("qemu boot report hypervisor=%s samples=%d started_to_program_p50=%s started_to_program_p95=%s rss_p50=%dB rss_p95=%dB", hypervisorType, samples, percentileDuration(durations, 50), percentileDuration(durations, 95), percentileInt64(rss, 50), percentileInt64(rss, 95))
	}
}

func runQEMUBootSamples(t *testing.T, hypervisorType hypervisor.Type, samples int) ([]time.Duration, []int64) {
	t.Helper()
	manager, tmpDir := setupTestManagerForQEMU(t)
	ctx := context.Background()
	p := paths.New(tmpDir)
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	image := integrationTestImageRef(t, "docker.io/library/nginx:alpine")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, image)
	require.NoError(t, system.NewManager(p).EnsureSystemFiles(ctx))

	durations := make([]time.Duration, 0, samples)
	rss := make([]int64, 0, samples)
	for i := 0; i < samples; i++ {
		inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
			Name:           fmt.Sprintf("%s-bench-%d", hypervisorType, i),
			Image:          image,
			Size:           lifecycleTestMemorySize,
			OverlaySize:    1024 * 1024 * 1024,
			Vcpus:          1,
			NetworkEnabled: false,
			Hypervisor:     hypervisorType,
		})
		require.NoError(t, err)
		instanceID := inst.Id
		deleted := false
		t.Cleanup(func() {
			if !deleted {
				_ = deleteTestInstanceNow(context.Background(), manager, instanceID)
			}
		})

		inst, err = waitForInstanceState(ctx, manager, inst.Id, StateRunning, integrationTestTimeout(45*time.Second))
		require.NoError(t, err)
		require.NotNil(t, inst.StartedAt)
		require.NotNil(t, inst.ProgramStartedAt)
		durations = append(durations, inst.ProgramStartedAt.Sub(*inst.StartedAt))
		if inst.HypervisorPID != nil {
			if sampleRSS, err := processRSSBytes(*inst.HypervisorPID); err == nil {
				rss = append(rss, sampleRSS)
			}
		}
		require.NoError(t, manager.DeleteInstance(ctx, inst.Id))
		deleted = true
	}
	return durations, rss
}

func processRSSBytes(pid int) (int64, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmRSS:" && fields[2] == "kB" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("VmRSS not found for pid %d", pid)
}

func percentileDuration(values []time.Duration, percentile int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	copy := append([]time.Duration(nil), values...)
	sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
	return copy[percentileIndex(len(copy), percentile)]
}

func percentileInt64(values []int64, percentile int) int64 {
	if len(values) == 0 {
		return 0
	}
	copy := append([]int64(nil), values...)
	sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
	return copy[percentileIndex(len(copy), percentile)]
}

func percentileIndex(length, percentile int) int {
	index := (length*percentile+99)/100 - 1 // nearest-rank percentile
	if index < 0 {
		return 0
	}
	if index >= length {
		return length - 1
	}
	return index
}
