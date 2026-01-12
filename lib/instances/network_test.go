package instances

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// TestCreateInstanceWithNetwork tests instance creation with network allocation
// and verifies network connectivity persists after standby/restore
func TestCreateInstanceWithNetwork(t *testing.T) {
	// Require KVM access
	requireKVMAccess(t)

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	// Pull nginx:alpine image (long-running workload)
	t.Log("Pulling nginx:alpine image...")
	nginxImage, err := manager.imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build to complete...")
	imageName := nginxImage.Name
	for i := 0; i < 60; i++ {
		img, err := manager.imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			nginxImage = img
			break
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, nginxImage.Status)
	t.Log("Nginx image ready")

	// Ensure system files
	t.Log("Ensuring system files...")
	systemManager := manager.systemManager
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Initialize network (creates bridge if needed)
	t.Log("Initializing network...")
	err = manager.networkManager.Initialize(ctx, nil)
	require.NoError(t, err)
	t.Log("Network initialized")

	// Create instance with nginx:alpine and default network
	t.Log("Creating instance with default network...")
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-net-instance",
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024, // 2GB (needs extra room for initrd with NVIDIA libs)
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for VM to be fully ready
	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err)
	t.Log("VM is ready")

	// Verify network allocation
	t.Log("Verifying network allocation...")
	alloc, err := manager.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, alloc, "Allocation should exist")
	assert.NotEmpty(t, alloc.IP, "IP should be allocated")
	assert.NotEmpty(t, alloc.MAC, "MAC should be allocated")
	assert.NotEmpty(t, alloc.TAPDevice, "TAP device should be allocated")
	t.Logf("Network allocated: IP=%s, MAC=%s, TAP=%s", alloc.IP, alloc.MAC, alloc.TAPDevice)

	// Verify TAP device exists
	t.Log("Verifying TAP device exists...")
	tap, err := netlink.LinkByName(alloc.TAPDevice)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(tap.Attrs().Name, "hype-"))
	assert.Equal(t, uint8(netlink.OperUp), uint8(tap.Attrs().OperState))
	t.Logf("TAP device verified: %s", alloc.TAPDevice)

	// Verify TAP attached to bridge
	bridge, err := netlink.LinkByName("vmbr0")
	require.NoError(t, err)
	assert.Equal(t, bridge.Attrs().Index, tap.Attrs().MasterIndex, "TAP should be attached to bridge")

	// Wait for nginx to start
	t.Log("Waiting for nginx to start...")
	err = waitForLogMessage(ctx, manager, inst.Id, "start worker processes", 15*time.Second)
	require.NoError(t, err, "Nginx should start")
	t.Log("Nginx is running")

	// Wait for exec agent to be ready
	t.Log("Waiting for exec agent...")
	err = waitForLogMessage(ctx, manager, inst.Id, "[guest-agent] listening", 10*time.Second)
	require.NoError(t, err, "Exec agent should be listening")
	t.Log("Exec agent is ready")

	// Test initial internet connectivity via exec
	t.Log("Testing initial internet connectivity via exec...")
	output, exitCode, err := execCommand(ctx, inst, "curl", "-s", "--connect-timeout", "10", "https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html")
	if err != nil || exitCode != 0 {
		t.Logf("curl failed: exitCode=%d err=%v output=%s", exitCode, err, output)
	}
	require.NoError(t, err, "Exec should succeed")
	require.Equal(t, 0, exitCode, "curl should succeed")
	require.Contains(t, output, "Connection successful", "Should get successful response")
	t.Log("Initial internet connectivity verified!")

	// Standby instance
	t.Log("Standing by instance...")
	inst, err = manager.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	t.Log("Instance in standby")

	// Verify TAP device is cleaned up during standby
	t.Log("Verifying TAP device cleaned up during standby...")
	_, err = netlink.LinkByName(alloc.TAPDevice)
	require.Error(t, err, "TAP device should be deleted during standby")
	t.Log("TAP device cleaned up as expected")

	// Verify network allocation still returns correct IP/MAC during standby (from snapshot)
	t.Log("Verifying network allocation during standby...")
	allocStandby, err := manager.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, allocStandby, "Allocation should exist during standby")
	assert.Equal(t, alloc.IP, allocStandby.IP, "IP should be preserved during standby")
	assert.Equal(t, alloc.MAC, allocStandby.MAC, "MAC should be preserved during standby")
	t.Logf("Network allocation during standby: IP=%s, MAC=%s", allocStandby.IP, allocStandby.MAC)

	// Restore instance
	t.Log("Restoring instance from standby...")
	inst, err = manager.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Log("Instance restored and running")

	// Wait for VM to be ready again
	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err)
	t.Log("VM is ready after restore")

	// Verify network allocation is restored
	t.Log("Verifying network allocation restored...")
	allocRestored, err := manager.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, allocRestored, "Allocation should exist after restore")
	assert.Equal(t, alloc.IP, allocRestored.IP, "IP should be preserved")
	assert.Equal(t, alloc.MAC, allocRestored.MAC, "MAC should be preserved")
	assert.Equal(t, alloc.TAPDevice, allocRestored.TAPDevice, "TAP name should be preserved")
	t.Logf("Network allocation restored: IP=%s, MAC=%s, TAP=%s", allocRestored.IP, allocRestored.MAC, allocRestored.TAPDevice)

	// Verify TAP device exists again
	t.Log("Verifying TAP device recreated...")
	tapRestored, err := netlink.LinkByName(allocRestored.TAPDevice)
	require.NoError(t, err)
	assert.Equal(t, uint8(netlink.OperUp), uint8(tapRestored.Attrs().OperState))
	t.Log("TAP device recreated successfully")

	// Test internet connectivity after restore via exec
	// Retry a few times as exec agent may need a moment after restore
	t.Log("Testing internet connectivity after restore via exec...")
	var restoreOutput string
	var restoreExitCode int
	for i := 0; i < 10; i++ {
		restoreOutput, restoreExitCode, err = execCommand(ctx, inst, "curl", "-s", "https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html")
		if err == nil && restoreExitCode == 0 {
			break
		}
		t.Logf("Exec attempt %d/10: err=%v exitCode=%d output=%s", i+1, err, restoreExitCode, restoreOutput)
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err, "Exec should succeed after restore")
	require.Equal(t, 0, restoreExitCode, "curl should succeed after restore")
	require.Contains(t, restoreOutput, "Connection successful", "Should get successful response after restore")
	t.Log("Internet connectivity verified after restore!")

	// Verify the original nginx process is still running (proves restore worked, not reboot)
	t.Log("Verifying nginx master process is still running...")
	psOutput, psExitCode, err := execCommand(ctx, inst, "ps", "aux")
	require.NoError(t, err)
	require.Equal(t, 0, psExitCode)
	require.Contains(t, psOutput, "nginx: master process", "nginx master should still be running")
	t.Log("Nginx process confirmed running - restore was successful!")

	// Cleanup
	t.Log("Cleaning up instance...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Verify TAP deleted after instance cleanup
	t.Log("Verifying TAP deleted after cleanup...")
	_, err = netlink.LinkByName(alloc.TAPDevice)
	require.Error(t, err, "TAP device should be deleted")
	t.Log("TAP device cleaned up after delete")

	// Verify network allocation released after delete
	t.Log("Verifying network allocation released after delete...")
	_, err = manager.networkManager.GetAllocation(ctx, inst.Id)
	require.Error(t, err, "Network allocation should not exist after delete")
	t.Log("Network allocation released after delete")

	t.Log("Network integration test complete!")
}

// execCommand runs a command in the instance via vsock and returns stdout+stderr, exit code, and error
func execCommand(ctx context.Context, inst *Instance, command ...string) (string, int, error) {
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

	// Return combined output
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR: " + stderr.String()
	}
	return output, exit.Code, nil
}

// requireKVMAccess checks for KVM availability
func requireKVMAccess(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group")
	}
}
