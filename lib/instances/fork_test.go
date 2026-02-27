package instances

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkInstanceNotSupportedHypervisor(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()
	if _, err := manager.getVMStarter(hypervisor.TypeVZ); err != nil {
		t.Skip("vz starter not available on this platform")
	}

	sourceID := "fork-vz-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              "fork-vz-source",
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeVZ,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "vz.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	_, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-vz-copy"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotSupported)
}

func TestForkCloudHypervisorFromRunningNetwork(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()

	imageManager, err := images.NewManager(paths.New(tmpDir), 1, nil)
	require.NoError(t, err)

	t.Log("Ensuring nginx image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: "docker.io/library/nginx:alpine"})
	require.NoError(t, err)

	imageName := nginxImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			nginxImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, nginxImage.Status, "Image should be ready after 60 seconds")

	systemManager := manager.systemManager
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	source, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fork-running-src",
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    256 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), source.Id) })
	require.NoError(t, waitForVMReady(ctx, source.SocketPath, 5*time.Second))
	require.NoError(t, waitForLogMessage(ctx, manager, source.Id, "start worker processes", 15*time.Second))

	assert.NotEmpty(t, source.IP)
	assert.NotEmpty(t, source.MAC)
	assertHostCanReachNginx(t, source.IP, 80, 60*time.Second)

	// Default behavior remains strict: running source requires explicit opt-in.
	_, err = manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{Name: "fork-running-no-flag"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)

	// Fork from running (internally: standby source -> copy fork -> restore source).
	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        "fork-running-copy",
		FromRunning: true,
	})
	require.NoError(t, err)
	require.Equal(t, StateStandby, forked.State)
	forkedID := forked.Id
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), forkedID) })

	// Source should be restored and still reachable by its private IP.
	sourceAfterFork, err := manager.GetInstance(ctx, source.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, sourceAfterFork.State)
	require.NotEmpty(t, sourceAfterFork.IP)
	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)

	// Restore fork and validate both VMs are independently reachable on private IPs.
	forked, err = manager.RestoreInstance(ctx, forkedID)
	require.NoError(t, err)
	require.Equal(t, StateRunning, forked.State)
	require.NoError(t, waitForVMReady(ctx, forked.SocketPath, 5*time.Second))

	assert.NotEmpty(t, forked.IP)
	assert.NotEmpty(t, forked.MAC)
	assert.NotEqual(t, sourceAfterFork.IP, forked.IP)
	assert.NotEqual(t, sourceAfterFork.MAC, forked.MAC)
	assertGuestHasOnlyExpectedIPv4(t, forked, forked.IP, 30*time.Second)
	assertHostCanReachNginx(t, forked.IP, 80, 60*time.Second)
	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)
}

func assertHostCanReachNginx(t *testing.T, ip string, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:%d/", ip, port)
	client := &http.Client{Timeout: 3 * time.Second}

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK && strings.Contains(string(body), "Welcome to nginx!") {
				return
			}
			if readErr != nil {
				lastErr = fmt.Errorf("read body: %w", readErr)
			} else {
				lastErr = fmt.Errorf("status=%d body=%q", resp.StatusCode, string(body))
			}
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}

	require.NoError(t, lastErr, "host should reach %s within %s", url, timeout)
}

func assertGuestHasOnlyExpectedIPv4(t *testing.T, inst *Instance, expectedIP string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		output, exitCode, err := execInInstance(context.Background(), inst, "sh", "-c", "ip -4 -o addr show dev eth0 scope global | awk '{print $4}'")
		if err == nil && exitCode == 0 {
			var cidrs []string
			for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
				if trimmed := strings.TrimSpace(line); trimmed != "" {
					cidrs = append(cidrs, trimmed)
				}
			}

			if len(cidrs) == 1 && strings.HasPrefix(cidrs[0], expectedIP+"/") {
				return
			}

			lastErr = fmt.Errorf("expected only %s on eth0, got %v", expectedIP, cidrs)
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("ip addr command exit code %d, output=%q", exitCode, output)
		}

		time.Sleep(500 * time.Millisecond)
	}

	require.NoError(t, lastErr, "guest should expose only the fork IP on eth0 within %s", timeout)
}

func execInInstance(ctx context.Context, inst *Instance, command ...string) (string, int, error) {
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return "", -1, err
	}

	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      command,
		Stdout:       &stdout,
		Stderr:       &stderr,
		WaitForAgent: 30 * time.Second,
	})
	if err != nil {
		return "", -1, err
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR: " + stderr.String()
	}
	return output, exit.Code, nil
}
