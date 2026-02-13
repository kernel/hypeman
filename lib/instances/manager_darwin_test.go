//go:build darwin

package instances

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vzExecCommand runs a command in the guest via vsock exec.
func vzExecCommand(ctx context.Context, inst *Instance, command ...string) (string, int, error) {
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	if err != nil {
		return "", -1, err
	}

	var stdout, stderr bytes.Buffer
	exit, err := guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: command,
		Stdin:   nil,
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     false,
	})
	if err != nil {
		return stderr.String(), -1, err
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR: " + stderr.String()
	}
	return output, exit.Code, nil
}

// TestVZBasicLifecycle tests the full vz instance lifecycle: create, exec, stop, start, delete.
func TestVZBasicLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("vz tests require macOS")
	}

	mgr, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Prepare image
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	t.Log("Pulling alpine:latest image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	imageName := alpineImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			alpineImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, alpineImage.Status, "Image should be ready")
	t.Log("Alpine image ready")

	// Ensure system files (kernel + initrd)
	systemManager := system.NewManager(p)
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Create instance using vz hypervisor
	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-vz-lifecycle",
		Image:          "docker.io/library/alpine:latest",
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
		Env:            map[string]string{"TEST_VAR": "hello"},
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, StateRunning, inst.State)
	assert.Equal(t, hypervisor.TypeVZ, inst.HypervisorType)
	t.Logf("Instance created: %s (hypervisor: %s)", inst.Id, inst.HypervisorType)

	t.Cleanup(func() {
		t.Log("Cleaning up instance...")
		mgr.DeleteInstance(ctx, inst.Id)
	})

	// Wait for guest agent to be ready
	err = waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second)
	require.NoError(t, err, "guest agent should be ready")
	t.Log("Guest agent ready")

	// Exec test: echo hello
	output, exitCode, err := vzExecCommand(ctx, inst, "echo", "hello")
	require.NoError(t, err, "exec should succeed")
	require.Equal(t, 0, exitCode)
	assert.Equal(t, "hello", strings.TrimSpace(output))
	t.Log("Exec test passed")

	// Graceful shutdown test
	t.Log("Stopping instance (graceful shutdown)...")
	inst, err = mgr.StopInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, inst.State)
	t.Log("Instance stopped")

	// Verify hypervisor process is gone
	if inst.HypervisorPID != nil {
		time.Sleep(500 * time.Millisecond)
		err := checkProcessGone(*inst.HypervisorPID)
		assert.NoError(t, err, "hypervisor process should be gone after stop")
	}

	// Restart test
	t.Log("Starting instance...")
	inst, err = mgr.StartInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Log("Instance restarted")

	// Wait for guest agent after restart
	err = waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second)
	require.NoError(t, err, "guest agent should be ready after restart")

	// Verify exec still works after restart
	output, exitCode, err = vzExecCommand(ctx, inst, "echo", "restarted")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	assert.Equal(t, "restarted", strings.TrimSpace(output))
	t.Log("Exec after restart passed")

	// Delete test
	t.Log("Deleting instance...")
	err = mgr.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	assert.NoDirExists(t, p.InstanceDir(inst.Id))
	_, err = mgr.GetInstance(ctx, inst.Id)
	assert.ErrorIs(t, err, ErrNotFound)
	t.Log("Instance deleted and cleaned up")
}

// TestVZExecAndShutdown focuses on exec behavior and graceful shutdown.
func TestVZExecAndShutdown(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("vz tests require macOS")
	}

	mgr, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Prepare image
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	t.Log("Pulling alpine:latest image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	imageName := alpineImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			alpineImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, alpineImage.Status, "Image should be ready")

	systemManager := system.NewManager(p)
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-vz-exec",
		Image:          "docker.io/library/alpine:latest",
		Size:           2 * 1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeVZ,
	})
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Logf("Instance created: %s", inst.Id)

	t.Cleanup(func() {
		mgr.DeleteInstance(ctx, inst.Id)
	})

	err = waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second)
	require.NoError(t, err, "guest agent should be ready")

	// Test: echo hello
	output, exitCode, err := vzExecCommand(ctx, inst, "echo", "hello")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	assert.Equal(t, "hello", strings.TrimSpace(output))
	t.Log("echo test passed")

	// Test: nonexistent command should error, not hang
	dialer, err := hypervisor.NewVsockDialer(inst.HypervisorType, inst.VsockSocket, inst.VsockCID)
	require.NoError(t, err)

	start := time.Now()
	var stdout, stderr strings.Builder
	_, err = guest.ExecIntoInstance(ctx, dialer, guest.ExecOptions{
		Command: []string{"nonexistent_command_xyz"},
		Stdout:  &stdout,
		Stderr:  &stderr,
		TTY:     false,
	})
	elapsed := time.Since(start)
	require.Error(t, err, "exec should fail for nonexistent command")
	require.Less(t, elapsed, 5*time.Second, "exec should not hang")
	t.Logf("Nonexistent command failed correctly in %v", elapsed)

	// Graceful shutdown
	t.Log("Stopping instance...")
	inst, err = mgr.StopInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, inst.State)
	t.Log("Instance stopped gracefully")

	// Delete
	err = mgr.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)
	_, err = mgr.GetInstance(ctx, inst.Id)
	assert.ErrorIs(t, err, ErrNotFound)
	t.Log("Instance deleted")
}

// checkProcessGone verifies a process no longer exists.
func checkProcessGone(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	err = proc.Signal(syscall.Signal(0))
	if err != nil {
		return nil // Process doesn't exist
	}
	return fmt.Errorf("process %d still running", pid)
}
