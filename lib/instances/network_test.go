package instances

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// TestCreateInstanceWithNetwork tests instance creation with network allocation
func TestCreateInstanceWithNetwork(t *testing.T) {
	// Require KVM access
	requireKVMAccess(t)

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()

	// Setup image manager for pulling alpine
	imageManager, err := images.NewManager(paths.New(tmpDir), 1)
	require.NoError(t, err)

	// Pull alpine image (lightweight, fast)
	t.Log("Pulling alpine:latest image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build to complete...")
	imageName := alpineImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			alpineImage = img
			break
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, alpineImage.Status)
	t.Log("Alpine image ready")

	// Ensure system files
	t.Log("Ensuring system files...")
	systemManager := manager.systemManager
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Initialize network (creates bridge if needed)
	t.Log("Initializing network...")
	err = manager.networkManager.Initialize(ctx)
	require.NoError(t, err)
	t.Log("Network initialized")

	// Create instance with default network
	// Use a scheduled check every 0.2 seconds to verify network persists after standby/resume
	t.Log("Creating instance with default network...")
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-net-instance",
		Image:          "docker.io/library/alpine:latest",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true, // Enable default network
		Env: map[string]string{
			"CMD": "sh -c 'counter=1; while true; do echo \"Check $counter: Testing internet connectivity...\"; if wget -q -O- https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html >/dev/null 2>&1; then echo \"Check $counter: Internet connectivity SUCCESS\"; fi; counter=$((counter+1)); sleep 0.2; done'",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for VM to be fully ready
	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err)
	t.Log("VM is ready")

	// Verify network allocation by querying network manager
	t.Log("Verifying network allocation...")
	alloc, err := manager.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, alloc, "Allocation should exist")
	assert.NotEmpty(t, alloc.IP, "IP should be allocated")
	assert.NotEmpty(t, alloc.MAC, "MAC should be allocated")
	assert.NotEmpty(t, alloc.TAPDevice, "TAP device should be allocated")
	assert.Equal(t, "default", alloc.Network)
	assert.Equal(t, "test-net-instance", alloc.InstanceName)
	t.Logf("Network allocated: IP=%s, MAC=%s, TAP=%s", alloc.IP, alloc.MAC, alloc.TAPDevice)

	// Verify TAP device exists in kernel
	t.Log("Verifying TAP device exists...")
	tap, err := netlink.LinkByName(alloc.TAPDevice)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(tap.Attrs().Name, "tap-"))
	// TAP should be UP - check the state matches
	assert.Equal(t, uint8(netlink.OperUp), uint8(tap.Attrs().OperState))
	t.Logf("TAP device verified: %s", alloc.TAPDevice)

	// Verify TAP attached to bridge
	// Test setup uses "vmbr0" as the bridge name
	bridge, err := netlink.LinkByName("vmbr0")
	require.NoError(t, err)
	assert.Equal(t, bridge.Attrs().Index, tap.Attrs().MasterIndex, "TAP should be attached to bridge")

	// Verify initial internet connectivity by checking logs
	t.Log("Verifying initial internet connectivity...")
	err = waitForLogMessage(ctx, manager, inst.Id, "Internet connectivity SUCCESS", 10*time.Second)
	require.NoError(t, err, "Instance should be able to reach the internet")
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

	// Verify network allocation is no longer active
	t.Log("Verifying network allocation released during standby...")
	_, err = manager.networkManager.GetAllocation(ctx, inst.Id)
	require.Error(t, err, "Network allocation should not exist during standby")
	t.Log("Network allocation released as expected")

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

	// Verify TAP still attached to bridge
	bridgeRestored, err := netlink.LinkByName("vmbr0")
	require.NoError(t, err)
	assert.Equal(t, bridgeRestored.Attrs().Index, tapRestored.Attrs().MasterIndex, "TAP should still be attached to bridge")

	// Verify internet connectivity continues after restore
	// First, get the current highest check number from logs
	t.Log("Finding current check number in logs...")
	logs, err := manager.GetInstanceLogs(ctx, inst.Id, false, 100)
	require.NoError(t, err)
	
	// Parse logs to find highest check number
	highestCheck := 0
	lines := strings.Split(logs, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Internet connectivity SUCCESS") {
			// Extract check number from "Check N: Internet connectivity SUCCESS"
			var checkNum int
			if n, err := fmt.Sscanf(line, "Check %d:", &checkNum); n == 1 && err == nil {
				if checkNum > highestCheck {
					highestCheck = checkNum
				}
			}
		}
	}
	t.Logf("Current highest check number: %d", highestCheck)
	require.Greater(t, highestCheck, 0, "Should have at least one successful check before waiting for next")

	// Wait for next check to ensure network is still working
	nextCheck := highestCheck + 1
	nextMessage := fmt.Sprintf("Check %d: Internet connectivity SUCCESS", nextCheck)
	t.Logf("Waiting for '%s'...", nextMessage)
	err = waitForLogMessage(ctx, manager, inst.Id, nextMessage, 5*time.Second)
	require.NoError(t, err, "Instance should continue checking internet connectivity after restore")
	t.Log("Network connectivity continues after restore!")

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

// requireKVMAccess checks for KVM availability
func requireKVMAccess(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group")
	}
}

