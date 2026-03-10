//go:build darwin

package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuestMemoryPolicyVZ(t *testing.T) {
	requireGuestMemoryManualRun(t)
	if runtime.GOOS != "darwin" {
		t.Skip("vz tests require macOS")
	}
	if runtime.GOARCH != "arm64" {
		t.Skip("vz tests require Apple Silicon")
	}

	mgr, tmpDir := setupVZTestManager(t)
	forceEnableGuestMemoryPolicyForVZTest(mgr)
	ctx := context.Background()

	createNginxImageAndWaitDarwin(t, ctx, mgr.imageManager)
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "guestmem-vz",
		Image:          "docker.io/library/nginx:alpine",
		Size:           4 * 1024 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
	})
	if err != nil {
		dumpVZShimLogs(t, tmpDir)
		require.NoError(t, err)
	}
	defer func() { _ = mgr.DeleteInstance(ctx, inst.Id) }()

	require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second))

	out, exitCode, err := vzExecCommand(ctx, inst, "cat", "/proc/cmdline")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	assert.Contains(t, out, "init_on_alloc=0")
	assert.Contains(t, out, "init_on_free=0")

	info, err := getVZVMInfo(inst.SocketPath)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, info.MemoryBalloonDevices, 1, "vz shim should report attached memory balloon device")

	instMeta, err := mgr.GetInstance(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, instMeta.HypervisorPID)
	assertLowIdleVZHostMemoryFootprint(t, *instMeta.HypervisorPID, 192*1024)
}

func forceEnableGuestMemoryPolicyForVZTest(mgr *manager) {
	mgr.guestMemoryPolicy = guestmemory.Policy{
		Enabled:            true,
		KernelPageInitMode: guestmemory.KernelPageInitPerformance,
		ReclaimEnabled:     true,
		VZBalloonRequired:  true,
	}.Normalize()
}

func createNginxImageAndWaitDarwin(t *testing.T, ctx context.Context, imageManager images.Manager) {
	t.Helper()

	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	imageName := nginxImage.Name
	for i := 0; i < 120; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			return
		}
		if err == nil && img.Status == images.StatusFailed {
			if img.Error != nil {
				t.Fatalf("image build failed: %s", *img.Error)
			}
			t.Fatalf("image build failed: unknown error")
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("timed out waiting for image %q to become ready", imageName)
}

type vzVMInfo struct {
	State                string `json:"state"`
	MemoryBalloonDevices int    `json:"memory_balloon_devices"`
}

func getVZVMInfo(socketPath string) (*vzVMInfo, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "http://vz-shim/api/v1/vm.info", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected vz vm.info status: %d", resp.StatusCode)
	}
	var info vzVMInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

func assertLowIdleVZHostMemoryFootprint(t *testing.T, pid int, maxRSSKB int64) {
	t.Helper()

	time.Sleep(12 * time.Second)
	var rssSamplesKB []int64
	for i := 0; i < 6; i++ {
		rssSamplesKB = append(rssSamplesKB, mustReadDarwinRSSBytes(t, pid)/1024)
		time.Sleep(1 * time.Second)
	}
	var rssSumKB int64
	for _, v := range rssSamplesKB {
		rssSumKB += v
	}
	avgRSSKB := rssSumKB / int64(len(rssSamplesKB))
	assert.LessOrEqualf(
		t,
		avgRSSKB,
		maxRSSKB,
		"expected low idle host memory footprint for vz (avg_rss_kb=%d max_rss_kb=%d rss_samples_kb=%v)",
		avgRSSKB,
		maxRSSKB,
		rssSamplesKB,
	)
}

func mustReadDarwinRSSBytes(t *testing.T, pid int) int64 {
	t.Helper()
	cmd := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	require.NoError(t, err)
	trimmed := strings.TrimSpace(string(out))
	require.NotEmpty(t, trimmed)
	kb, err := strconv.ParseInt(trimmed, 10, 64)
	require.NoError(t, err)
	return kb * 1024
}
