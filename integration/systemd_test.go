package integration

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSystemdMode verifies that hypeman correctly detects and runs
// systemd-based images with systemd as PID 1.
//
// This test uses the jrei/systemd-ubuntu image from Docker Hub which runs
// systemd as its CMD. The test verifies that hypeman auto-detects this and:
// - Uses systemd mode (chroot to container rootfs)
// - Starts systemd as PID 1
// - Injects and starts the hypeman-agent.service
func TestSystemdMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Skip if KVM is not available
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Set up test environment
	tmpDir := t.TempDir()
	p := paths.New(tmpDir)

	cfg := &config.Config{
		DataDir:    tmpDir,
		BridgeName: "vmbr0",
		SubnetCIDR: "10.100.0.0/16",
		DNSServer:  "1.1.1.1",
	}

	// Create managers
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)

	limits := instances.ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  0,
		MaxMemoryPerInstance: 0,
		MaxTotalVcpus:        0,
		MaxTotalMemory:       0,
	}

	instanceManager := instances.NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, "", nil, nil)

	// Cleanup any orphaned instances
	t.Cleanup(func() {
		instanceManager.DeleteInstance(ctx, "systemd-test")
	})

	imageName := "docker.io/jrei/systemd-ubuntu:22.04"

	// Pull the systemd image
	t.Log("Pulling systemd image:", imageName)
	_, err = imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: imageName,
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build...")
	var img *images.Image
	for i := 0; i < 120; i++ {
		img, err = imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, img.Status, "image should be ready")

	// Verify systemd detection
	t.Run("IsSystemdImage", func(t *testing.T) {
		isSystemd := images.IsSystemdImage(img.Entrypoint, img.Cmd)
		assert.True(t, isSystemd, "image should be detected as systemd, entrypoint=%v cmd=%v", img.Entrypoint, img.Cmd)
	})

	// Ensure system files (kernel, initrd)
	t.Log("Ensuring system files...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Create the systemd instance
	t.Log("Creating systemd instance...")
	inst, err := instanceManager.CreateInstance(ctx, instances.CreateInstanceRequest{
		Name:           "systemd-test",
		Image:          imageName,
		Size:           2 * 1024 * 1024 * 1024, // 2GB
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          2,
		NetworkEnabled: false, // No network needed for this test
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for guest agent to be ready
	t.Log("Waiting for guest agent...")
	err = waitForGuestAgent(ctx, instanceManager, inst.Id, 60*time.Second)
	require.NoError(t, err, "guest agent should be ready")

	// Test: Verify systemd is PID 1
	t.Run("SystemdIsPID1", func(t *testing.T) {
		output, exitCode, err := execInInstance(ctx, inst, "cat", "/proc/1/comm")
		require.NoError(t, err, "exec should work")
		require.Equal(t, 0, exitCode, "command should succeed")

		pid1Name := strings.TrimSpace(output)
		assert.Equal(t, "systemd", pid1Name, "PID 1 should be systemd")
		t.Logf("PID 1 is: %s", pid1Name)
	})

	// Test: Verify guest-agent binary exists
	t.Run("GuestAgentExists", func(t *testing.T) {
		output, exitCode, err := execInInstance(ctx, inst, "test", "-x", "/opt/hypeman/guest-agent")
		require.NoError(t, err, "exec should work")
		assert.Equal(t, 0, exitCode, "guest-agent binary should exist at /opt/hypeman/guest-agent, output: %s", output)
	})

	// Test: Verify hypeman-agent.service is active
	t.Run("AgentServiceActive", func(t *testing.T) {
		output, exitCode, err := execInInstance(ctx, inst, "systemctl", "is-active", "hypeman-agent")
		require.NoError(t, err, "exec should work")
		status := strings.TrimSpace(output)
		assert.Equal(t, 0, exitCode, "hypeman-agent service should be active, status: %s", status)
		assert.Equal(t, "active", status, "service status should be 'active'")
		t.Logf("hypeman-agent service status: %s", status)
	})

	// Test: Verify we can view agent logs via journalctl
	t.Run("AgentLogsAccessible", func(t *testing.T) {
		output, exitCode, err := execInInstance(ctx, inst, "journalctl", "-u", "hypeman-agent", "--no-pager", "-n", "5")
		require.NoError(t, err, "exec should work")
		assert.Equal(t, 0, exitCode, "journalctl should succeed")
		t.Logf("Agent logs (last 5 lines):\n%s", output)
	})

	t.Log("All systemd mode tests passed!")
}

// waitForGuestAgent polls until the guest agent is ready
func waitForGuestAgent(ctx context.Context, mgr instances.Manager, instanceID string, timeout time.Duration) error {
	inst, err := mgr.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}

	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return err
	}

	// Use WaitForAgent to wait for the agent to be ready
	var stdout bytes.Buffer
	_, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command:      []string{"echo", "ready"},
		Stdout:       &stdout,
		TTY:          false,
		WaitForAgent: timeout,
	})
	return err
}

// execInInstance executes a command in the instance
func execInInstance(ctx context.Context, inst *instances.Instance, command ...string) (string, int, error) {
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return "", -1, err
	}

	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: command,
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     false,
	})
	if err != nil {
		return stderr.String(), -1, err
	}

	return stdout.String(), exit.Code, nil
}
