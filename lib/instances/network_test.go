package instances

import (
	"context"
	"os"
	"path/filepath"
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
	
	// DEBUG: Capture logs and console.log file size before standby
	t.Log("DEBUG: Capturing logs before standby...")
	logsBeforeStandby, err := manager.GetInstanceLogs(ctx, inst.Id, false, 200)
	require.NoError(t, err)
	successCountBeforeStandby := strings.Count(logsBeforeStandby, "Internet connectivity SUCCESS")
	linesBeforeStandby := len(strings.Split(logsBeforeStandby, "\n"))
	t.Logf("DEBUG: Before standby - %d total lines, %d successful checks", linesBeforeStandby, successCountBeforeStandby)
	
	// Check console.log file size
	consoleLogPath := filepath.Join(tmpDir, "guests", inst.Id, "logs", "console.log")
	var consoleLogSizeBeforeStandby int64
	if info, err := os.Stat(consoleLogPath); err == nil {
		consoleLogSizeBeforeStandby = info.Size()
		t.Logf("DEBUG: console.log file size before standby: %d bytes", consoleLogSizeBeforeStandby)
	}

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

	// DEBUG: Check snapshot files
	t.Log("DEBUG: Checking snapshot files...")
	snapshotDir := filepath.Join(tmpDir, "guests", inst.Id, "snapshots", "snapshot-latest")
	entries, err := os.ReadDir(snapshotDir)
	if err != nil {
		t.Logf("DEBUG: Failed to read snapshot dir: %v", err)
	} else {
		t.Logf("DEBUG: Snapshot files:")
		for _, entry := range entries {
			info, _ := entry.Info()
			t.Logf("  - %s (size: %d bytes)", entry.Name(), info.Size())
		}
	}
	
	// Check if state.json exists (critical for proper CPU state restore)
	stateJsonPath := filepath.Join(snapshotDir, "state.json")
	if info, err := os.Stat(stateJsonPath); err == nil {
		t.Logf("DEBUG: state.json EXISTS (size: %d bytes) - CPU state should be preserved", info.Size())
	} else {
		t.Logf("DEBUG: state.json MISSING! This would cause fresh boot. Error: %v", err)
	}

	// Verify network allocation metadata still exists (derived from snapshot)
	// Note: Standby VMs derive allocation from snapshot's config.json, even though TAP is deleted
	t.Log("Verifying network allocation metadata preserved in snapshot...")
	allocStandby, err := manager.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err, "Network allocation should still be derivable from snapshot")
	require.NotNil(t, allocStandby, "Allocation should not be nil")
	assert.Equal(t, alloc.IP, allocStandby.IP, "IP should be preserved in snapshot")
	assert.Equal(t, alloc.MAC, allocStandby.MAC, "MAC should be preserved in snapshot")
	assert.Equal(t, alloc.TAPDevice, allocStandby.TAPDevice, "TAP name should be preserved in snapshot")
	t.Log("Network allocation metadata preserved in snapshot")

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

	// DEBUG: Check Cloud Hypervisor logs
	t.Log("DEBUG: Checking Cloud Hypervisor stdout/stderr logs...")
	instanceDir := filepath.Join(tmpDir, "guests", inst.Id)
	
	chStdoutPath := filepath.Join(instanceDir, "ch-stdout.log")
	if chStdout, err := os.ReadFile(chStdoutPath); err != nil {
		t.Logf("DEBUG: Failed to read ch-stdout.log: %v", err)
	} else {
		t.Logf("DEBUG: ch-stdout.log contents (%d bytes):", len(chStdout))
		stdoutLines := strings.Split(string(chStdout), "\n")
		// Show last 30 lines
		startIdx := len(stdoutLines) - 30
		if startIdx < 0 {
			startIdx = 0
		}
		for i := startIdx; i < len(stdoutLines); i++ {
			if stdoutLines[i] != "" {
				t.Logf("  %s", stdoutLines[i])
			}
		}
	}
	
	chStderrPath := filepath.Join(instanceDir, "ch-stderr.log")
	if chStderr, err := os.ReadFile(chStderrPath); err != nil {
		t.Logf("DEBUG: Failed to read ch-stderr.log: %v", err)
	} else {
		t.Logf("DEBUG: ch-stderr.log contents (%d bytes):", len(chStderr))
		stderrLines := strings.Split(string(chStderr), "\n")
		// Show all lines (usually shorter)
		for i, line := range stderrLines {
			if line != "" {
				t.Logf("  Line %d: %s", i+1, line)
			}
		}
	}
	
	// DEBUG: Check console.log file size after restore
	t.Log("DEBUG: Checking console.log after restore...")
	if info, err := os.Stat(consoleLogPath); err == nil {
		consoleLogSizeAfterRestore := info.Size()
		t.Logf("DEBUG: console.log file size after restore: %d bytes", consoleLogSizeAfterRestore)
		t.Logf("DEBUG: File size change: before=%d bytes, after=%d bytes, diff=%d bytes", 
			consoleLogSizeBeforeStandby, consoleLogSizeAfterRestore, consoleLogSizeAfterRestore-consoleLogSizeBeforeStandby)
		
		if consoleLogSizeAfterRestore < consoleLogSizeBeforeStandby {
			t.Logf("DEBUG: WARNING! console.log was TRUNCATED during restore (lost %d bytes)", 
				consoleLogSizeBeforeStandby-consoleLogSizeAfterRestore)
		} else if consoleLogSizeAfterRestore == consoleLogSizeBeforeStandby {
			t.Log("DEBUG: console.log size unchanged - file not truncated, but no new output yet")
		} else {
			t.Logf("DEBUG: console.log grew by %d bytes - new output being generated", 
				consoleLogSizeAfterRestore-consoleLogSizeBeforeStandby)
		}
	}
	
	// DEBUG: Check console logs immediately after restore
	t.Log("DEBUG: Checking console logs content immediately after restore...")
	logsAfterRestore, err := manager.GetInstanceLogs(ctx, inst.Id, false, 200)
	require.NoError(t, err)
	successCountAfterRestore := strings.Count(logsAfterRestore, "Internet connectivity SUCCESS")
	linesAfterRestore := len(strings.Split(logsAfterRestore, "\n"))
	t.Logf("DEBUG: After restore - %d total lines, %d successful checks", linesAfterRestore, successCountAfterRestore)
	t.Logf("DEBUG: Lines before standby: %d, lines after restore: %d, difference: %d", 
		linesBeforeStandby, linesAfterRestore, linesAfterRestore-linesBeforeStandby)
	t.Logf("DEBUG: First 20 lines after restore:")
	logLinesAfterRestore := strings.Split(logsAfterRestore, "\n")
	for i := 0; i < 20 && i < len(logLinesAfterRestore); i++ {
		if logLinesAfterRestore[i] != "" {
			t.Logf("  Line %d: %s", i+1, logLinesAfterRestore[i])
		}
	}
	
	// Wait longer to see if process eventually starts producing output
	t.Log("DEBUG: Waiting 5 seconds to see if VM boot completes and process starts...")
	time.Sleep(5 * time.Second)
	logsAfterWait, err := manager.GetInstanceLogs(ctx, inst.Id, false, 200)
	require.NoError(t, err)
	successCountAfterWait := strings.Count(logsAfterWait, "Internet connectivity SUCCESS")
	linesAfterWait := len(strings.Split(logsAfterWait, "\n"))
	t.Logf("DEBUG: After waiting 5s - %d total lines, %d successful checks", linesAfterWait, successCountAfterWait)
	t.Logf("DEBUG: Log growth: %d new lines, %d new successful checks", linesAfterWait-linesAfterRestore, successCountAfterWait-successCountAfterRestore)
	
	// Check console.log file size growth
	if info, err := os.Stat(consoleLogPath); err == nil {
		t.Logf("DEBUG: console.log size after 5s wait: %d bytes (grew by %d bytes)", info.Size(), info.Size()-89)
	}
	
	if successCountAfterWait == successCountAfterRestore {
		t.Log("DEBUG: No new checks produced after 5s! Dumping current log output:")
		afterWaitLines := strings.Split(logsAfterWait, "\n")
		for i, line := range afterWaitLines {
			if line != "" {
				t.Logf("  Line %d: %s", i+1, line)
			}
		}
	} else {
		t.Logf("DEBUG: SUCCESS! Found %d checks after restore (VM is working)", successCountAfterWait)
	}

	// Verify internet connectivity continues after restore
	t.Log("Verifying internet connectivity after restore...")
	err = waitForLogMessage(ctx, manager, inst.Id, "Internet connectivity SUCCESS", 10*time.Second)
	require.NoError(t, err, "Instance should have working internet connectivity after restore")
	t.Log("Internet connectivity verified after restore!")

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

