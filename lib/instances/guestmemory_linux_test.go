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
	ctx := context.Background()

	createImageAndWait(t, ctx, mgr.imageManager, "docker.io/library/alpine:latest")
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "guestmem-ch",
		Image:          "docker.io/library/alpine:latest",
		Size:           1024 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeCloudHypervisor,
		Entrypoint:     []string{"/bin/sh", "-c"},
		Cmd:            []string{guestMemoryWorkloadScript()},
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
	runGuestMemoryReclaimProbe(t, pid)
}

func TestGuestMemoryPolicyQEMU(t *testing.T) {
	requireGuestMemoryManualRun(t)
	requireKVMAccess(t)

	mgr, _ := setupTestManagerForQEMU(t)
	ctx := context.Background()

	createImageAndWait(t, ctx, mgr.imageManager, "docker.io/library/alpine:latest")
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "guestmem-qemu",
		Image:          "docker.io/library/alpine:latest",
		Size:           1024 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeQEMU,
		Entrypoint:     []string{"/bin/sh", "-c"},
		Cmd:            []string{guestMemoryWorkloadScript()},
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

	runGuestMemoryReclaimProbe(t, pid)
}

func TestGuestMemoryPolicyFirecracker(t *testing.T) {
	requireGuestMemoryManualRun(t)
	requireFirecrackerIntegrationPrereqs(t)

	mgr, _ := setupTestManagerForFirecracker(t)
	ctx := context.Background()

	createImageAndWait(t, ctx, mgr.imageManager, "docker.io/library/alpine:latest")
	require.NoError(t, mgr.systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "guestmem-fc",
		Image:          "docker.io/library/alpine:latest",
		Size:           1024 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
		Entrypoint:     []string{"/bin/sh", "-c"},
		Cmd:            []string{guestMemoryWorkloadScript()},
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
	runGuestMemoryReclaimProbe(t, pid)
}

func guestMemoryWorkloadScript() string {
	return "set -e; sleep 8; test -d /dev/shm || mkdir -p /dev/shm; dd if=/dev/zero of=/dev/shm/hype-mem bs=1M count=256 >/dev/null 2>&1; sleep 3; rm -f /dev/shm/hype-mem; sync; sleep 120"
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

func runGuestMemoryReclaimProbe(t *testing.T, pid int) {
	t.Helper()

	baselineRSS := mustReadRSSBytes(t, pid)
	peakRSS := baselineRSS
	postPeakMinRSS := int64(0)
	growthThreshold := int64(16 * 1024 * 1024)
	dropSignalThreshold := int64(1 * 1024 * 1024)

	// Wait for the in-guest workload to allocate memory and require a visible RSS increase.
	deadline := time.Now().Add(50 * time.Second)
	for time.Now().Before(deadline) {
		rss := mustReadRSSBytes(t, pid)
		if rss > peakRSS {
			peakRSS = rss
		}
		if peakRSS > baselineRSS+growthThreshold {
			postPeakMinRSS = rss
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	assert.Greaterf(
		t,
		peakRSS,
		baselineRSS+growthThreshold,
		"expected RSS to rise during workload (baseline=%d peak=%d growth_threshold=%d)",
		baselineRSS,
		peakRSS,
		growthThreshold,
	)

	// Reclaim/drop signal is best-effort: backend flags are validated elsewhere in each test.
	// Host RSS accounting and kernel reclaim timing can vary across systems.
	recoveryDeadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(recoveryDeadline) {
		rss := mustReadRSSBytes(t, pid)
		if postPeakMinRSS == 0 || rss < postPeakMinRSS {
			postPeakMinRSS = rss
		}
		time.Sleep(500 * time.Millisecond)
	}

	drop := peakRSS - postPeakMinRSS
	if drop >= dropSignalThreshold {
		t.Logf("observed post-peak RSS drop: %d bytes (baseline=%d peak=%d min=%d)", drop, baselineRSS, peakRSS, postPeakMinRSS)
		return
	}
	t.Logf("no clear post-peak RSS drop observed (baseline=%d peak=%d min=%d)", baselineRSS, peakRSS, postPeakMinRSS)
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
