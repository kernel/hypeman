//go:build linux

package instances

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuestMemoryPolicyCloudHypervisor(t *testing.T) {
	requireGuestMemoryManualRun(t)
	requireKVMAccess(t)

	mgr, _ := setupTestManager(t)
	forceEnableGuestMemoryPolicyForTest(mgr)
	ctx := context.Background()

	createImageAndWait(t, ctx, mgr.imageManager, "docker.io/library/alpine:latest")
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "guestmem-ch",
		Image:          "docker.io/library/alpine:latest",
		Size:           4 * 1024 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeCloudHypervisor,
		Entrypoint:     []string{"/bin/sh", "-c"},
		Cmd:            []string{guestMemoryIdleScript()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteInstance(ctx, inst.Id) })

	require.NoError(t, waitForVMReady(ctx, inst.SocketPath, 10*time.Second))

	client, err := vmm.NewVMM(inst.SocketPath)
	require.NoError(t, err)
	infoResp, err := client.GetVmInfoWithResponse(ctx)
	require.NoError(t, err)
	require.Equal(t, 200, infoResp.StatusCode())
	require.NotNil(t, infoResp.JSON200)
	require.NotNil(t, infoResp.JSON200.Config.Payload)
	require.NotNil(t, infoResp.JSON200.Config.Payload.Cmdline)
	assert.Contains(t, *infoResp.JSON200.Config.Payload.Cmdline, "init_on_alloc=0")
	assert.Contains(t, *infoResp.JSON200.Config.Payload.Cmdline, "init_on_free=0")

	require.NotNil(t, infoResp.JSON200.Config.Balloon, "cloud-hypervisor vm.info config should include balloon")
	assert.True(t, infoResp.JSON200.Config.Balloon.DeflateOnOom != nil && *infoResp.JSON200.Config.Balloon.DeflateOnOom)
	assert.True(t, infoResp.JSON200.Config.Balloon.FreePageReporting != nil && *infoResp.JSON200.Config.Balloon.FreePageReporting)

	pid := requireHypervisorPID(t, ctx, mgr, inst.Id)
	assertLowIdleHostMemoryFootprint(t, "cloud-hypervisor", pid, 512*1024)
}

func TestGuestMemoryPolicyQEMU(t *testing.T) {
	requireGuestMemoryManualRun(t)
	requireKVMAccess(t)

	mgr, _ := setupTestManagerForQEMU(t)
	forceEnableGuestMemoryPolicyForTest(mgr)
	ctx := context.Background()

	createImageAndWait(t, ctx, mgr.imageManager, "docker.io/library/alpine:latest")
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "guestmem-qemu",
		Image:          "docker.io/library/alpine:latest",
		Size:           4 * 1024 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeQEMU,
		Entrypoint:     []string{"/bin/sh", "-c"},
		Cmd:            []string{guestMemoryIdleScript()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteInstance(ctx, inst.Id) })

	require.NoError(t, waitForQEMUReady(ctx, inst.SocketPath, 10*time.Second))

	pid := requireHypervisorPID(t, ctx, mgr, inst.Id)
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	require.NoError(t, err)
	joined := strings.ReplaceAll(string(cmdline), "\x00", " ")
	assert.Contains(t, joined, "init_on_alloc=0")
	assert.Contains(t, joined, "init_on_free=0")
	assert.Contains(t, joined, "virtio-balloon-pci", "qemu cmdline should include virtio balloon device")

	assertLowIdleHostMemoryFootprint(t, "qemu", pid, 640*1024)
}

func TestGuestMemoryPolicyFirecracker(t *testing.T) {
	requireGuestMemoryManualRun(t)
	requireFirecrackerIntegrationPrereqs(t)

	mgr, _ := setupTestManagerForFirecracker(t)
	forceEnableGuestMemoryPolicyForTest(mgr)
	ctx := context.Background()

	createImageAndWait(t, ctx, mgr.imageManager, "docker.io/library/alpine:latest")
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "guestmem-fc",
		Image:          "docker.io/library/alpine:latest",
		Size:           4 * 1024 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
		Entrypoint:     []string{"/bin/sh", "-c"},
		Cmd:            []string{guestMemoryIdleScript()},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteInstance(ctx, inst.Id) })

	vmCfg, err := getFirecrackerVMConfig(inst.SocketPath)
	require.NoError(t, err)
	assert.Contains(t, vmCfg.BootSource.BootArgs, "init_on_alloc=0")
	assert.Contains(t, vmCfg.BootSource.BootArgs, "init_on_free=0")
	assert.True(t, vmCfg.Balloon.DeflateOnOOM)
	assert.True(t, vmCfg.Balloon.FreePageHinting)
	assert.True(t, vmCfg.Balloon.FreePageReporting)

	pid := requireHypervisorPID(t, ctx, mgr, inst.Id)
	assertLowIdleHostMemoryFootprint(t, "firecracker", pid, 512*1024)
}

func guestMemoryIdleScript() string {
	return "set -e; sleep 180"
}

func forceEnableGuestMemoryPolicyForTest(mgr *manager) {
	mgr.guestMemoryPolicy = guestmemory.Policy{
		Enabled:            true,
		KernelPageInitMode: guestmemory.KernelPageInitPerformance,
		ReclaimEnabled:     true,
		VZBalloonRequired:  true,
	}.Normalize()
}

func createImageAndWait(t *testing.T, ctx context.Context, imageManager images.Manager, imageName string) {
	t.Helper()

	img, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: imageName})
	require.NoError(t, err)

	for i := 0; i < 180; i++ {
		current, err := imageManager.GetImage(ctx, img.Name)
		if err == nil && current.Status == images.StatusReady {
			return
		}
		if err == nil && current.Status == images.StatusFailed {
			if current.Error != nil {
				t.Fatalf("image build failed: %s", *current.Error)
			}
			t.Fatalf("image build failed: unknown error")
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("timed out waiting for image %q to become ready", img.Name)
}

func requireHypervisorPID(t *testing.T, ctx context.Context, mgr *manager, instanceID string) int {
	t.Helper()
	inst, err := mgr.GetInstance(ctx, instanceID)
	require.NoError(t, err)
	require.NotNil(t, inst.HypervisorPID)
	return *inst.HypervisorPID
}

func assertLowIdleHostMemoryFootprint(t *testing.T, hypervisorName string, pid int, maxPSSKB int64) {
	t.Helper()

	// Give the guest a short settle window, then sample host memory.
	time.Sleep(12 * time.Second)
	var pssSamplesKB []int64
	var rssSamplesKB []int64
	for i := 0; i < 6; i++ {
		pssSamplesKB = append(pssSamplesKB, mustReadPSSKB(t, pid))
		rssSamplesKB = append(rssSamplesKB, mustReadRSSBytes(t, pid)/1024)
		time.Sleep(1 * time.Second)
	}

	var pssSumKB int64
	for _, v := range pssSamplesKB {
		pssSumKB += v
	}
	avgPSSKB := pssSumKB / int64(len(pssSamplesKB))

	assert.LessOrEqualf(
		t,
		avgPSSKB,
		maxPSSKB,
		"expected low idle host memory footprint for %s (avg_pss_kb=%d max_pss_kb=%d rss_samples_kb=%v pss_samples_kb=%v)",
		hypervisorName,
		avgPSSKB,
		maxPSSKB,
		rssSamplesKB,
		pssSamplesKB,
	)
}

func mustReadRSSBytes(t *testing.T, pid int) int64 {
	t.Helper()
	statusPath := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(statusPath)
	require.NoError(t, err)

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			require.GreaterOrEqual(t, len(fields), 2)
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			require.NoError(t, err)
			return kb * 1024
		}
	}
	t.Fatalf("VmRSS not found in %s", statusPath)
	return 0
}

func mustReadPSSKB(t *testing.T, pid int) int64 {
	t.Helper()
	smapsRollupPath := fmt.Sprintf("/proc/%d/smaps_rollup", pid)
	data, err := os.ReadFile(smapsRollupPath)
	require.NoError(t, err)

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Pss:") {
			fields := strings.Fields(line)
			require.GreaterOrEqual(t, len(fields), 2)
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			require.NoError(t, err)
			return kb
		}
	}
	t.Fatalf("Pss not found in %s", smapsRollupPath)
	return 0
}

type firecrackerVMConfig struct {
	BootSource struct {
		BootArgs string `json:"boot_args"`
	} `json:"boot-source"`
	Balloon struct {
		DeflateOnOOM      bool `json:"deflate_on_oom"`
		FreePageHinting   bool `json:"free_page_hinting"`
		FreePageReporting bool `json:"free_page_reporting"`
	} `json:"balloon"`
}

func getFirecrackerVMConfig(socketPath string) (*firecrackerVMConfig, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "http://localhost/vm/config", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected firecracker /vm/config status: %d", resp.StatusCode)
	}

	var cfg firecrackerVMConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
