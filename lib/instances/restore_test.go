package instances

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// TestRestoreInstance tests that restore properly resumes VM state with networking
func TestRestoreInstance(t *testing.T) {
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
	// Use a simple counter that prints every 0.2 seconds (no network dependency)
	t.Log("Creating instance with network enabled...")
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-restore",
		Image:          "docker.io/library/alpine:latest",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true, // Enable default network
		Env: map[string]string{
			"CMD": "sh -c 'counter=1; while true; do echo \"Count $counter: Process is running\"; counter=$((counter+1)); sleep 0.2; done'",
		},
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
	assert.Equal(t, uint8(netlink.OperUp), uint8(tap.Attrs().OperState))
	t.Log("TAP device verified")

	// Wait for process to start and print some counts
	t.Log("Waiting for process to start printing...")
	err = waitForLogMessage(ctx, manager, inst.Id, "Process is running", 10*time.Second)
	require.NoError(t, err, "Process should start running")
	t.Log("Process is running!")

	// Wait a bit to accumulate some counts
	time.Sleep(1 * time.Second)
	
	// Get current count
	t.Log("Getting current count before standby...")
	logs, err := manager.GetInstanceLogs(ctx, inst.Id, false, 100)
	require.NoError(t, err)
	
	highestCount := 0
	lines := strings.Split(logs, "\n")
	for _, line := range lines {
		var countNum int
		if n, err := fmt.Sscanf(line, "Count %d:", &countNum); n == 1 && err == nil {
			if countNum > highestCount {
				highestCount = countNum
			}
		}
	}
	t.Logf("Current highest count before standby: %d", highestCount)
	require.Greater(t, highestCount, 0, "Should have some counts before standby")

	// Standby instance
	t.Log("Standing by instance...")
	inst, err = manager.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	t.Log("Instance in standby")

	// Verify TAP device is cleaned up
	t.Log("Verifying TAP device cleaned up during standby...")
	_, err = netlink.LinkByName(alloc.TAPDevice)
	require.Error(t, err, "TAP device should be deleted during standby")
	t.Log("TAP device cleaned up")

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

	// Verify TAP device recreated
	t.Log("Verifying TAP device recreated...")
	tapRestored, err := netlink.LinkByName(alloc.TAPDevice)
	require.NoError(t, err)
	assert.Equal(t, uint8(netlink.OperUp), uint8(tapRestored.Attrs().OperState))
	t.Log("TAP device recreated")

	// Check what count we see immediately after restore
	t.Log("Checking count immediately after restore...")
	logsAfterRestore, err := manager.GetInstanceLogs(ctx, inst.Id, false, 100)
	require.NoError(t, err)
	
	highestCountAfterRestore := 0
	linesAfterRestore := strings.Split(logsAfterRestore, "\n")
	for _, line := range linesAfterRestore {
		var countNum int
		if n, err := fmt.Sscanf(line, "Count %d:", &countNum); n == 1 && err == nil {
			if countNum > highestCountAfterRestore {
				highestCountAfterRestore = countNum
			}
		}
	}
	t.Logf("Highest count after restore: %d (was %d before standby)", highestCountAfterRestore, highestCount)
	
	// The count should continue from where it left off (or close to it)
	// Due to console.log truncation, we might see a count >= highestCount
	if highestCountAfterRestore > 0 {
		t.Logf("Process resumed! Current count: %d", highestCountAfterRestore)
	} else {
		t.Log("No counts seen yet, waiting for process to output...")
		// Dump what we see
		for i, line := range linesAfterRestore {
			if line != "" {
				t.Logf("  Line %d: %s", i+1, line)
			}
		}
	}

	// Wait for count to increment to prove it's continuing
	t.Log("Waiting for count to increment...")
	nextCount := highestCountAfterRestore + 1
	nextMessage := fmt.Sprintf("Count %d: Process is running", nextCount)
	t.Logf("Looking for: '%s'", nextMessage)
	err = waitForLogMessage(ctx, manager, inst.Id, nextMessage, 5*time.Second)
	require.NoError(t, err, "Count should increment, proving process is continuing")
	t.Log("SUCCESS! Count incremented - process is continuing after restore")

	// Cleanup
	t.Log("Cleaning up instance...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Verify TAP deleted
	t.Log("Verifying TAP deleted after cleanup...")
	_, err = netlink.LinkByName(alloc.TAPDevice)
	require.Error(t, err, "TAP device should be deleted")

	t.Log("Restore test complete!")
}

