package instances

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/healthcheck"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	restartpolicy "github.com/kernel/hypeman/lib/restart-policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

// TestCreateInstanceWithNetwork tests instance creation with network allocation
// and verifies network connectivity persists after standby/restore
func TestCreateInstanceWithNetwork(t *testing.T) {
	t.Parallel()
	// Require KVM access
	requireKVMAccess(t)

	manager, _ := setupTestManager(t)
	ctx := context.Background()
	startHealthCheckControllerForTest(t, ctx, manager)
	startRestartPolicyControllerForTest(t, ctx, manager)

	// Pull nginx:alpine image (long-running workload)
	t.Log("Pulling nginx:alpine image...")
	nginxImage, err := manager.imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
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
	require.NoError(t, manager.networkManager.SetupHTB(ctx, 100*1024*1024))
	t.Log("Network initialized")

	// Create instance with nginx:alpine and default network
	t.Log("Creating instance with default network...")
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-net-instance",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           2 * 1024 * 1024 * 1024, // 2GB (needs extra room for initrd with NVIDIA libs)
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		HealthCheck: &healthcheck.Policy{
			Type:             healthcheck.TypeHTTP,
			Interval:         "1s",
			Timeout:          "1s",
			StartPeriod:      "30s",
			FailureThreshold: 3,
			SuccessThreshold: 1,
			HTTP: &healthcheck.HTTPCheck{
				Port:           80,
				Path:           "/",
				ExpectedStatus: 200,
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	require.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	require.NotNil(t, inst.HealthCheck)
	initialHealth := healthcheck.Snapshot(inst.HealthCheck, string(inst.State), inst.HealthCheckRuntime)
	assert.Equal(t, healthcheck.StatusStarting, initialHealth.Status)
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
	t.Logf("TAP device verified: %s oper_state=%v", alloc.TAPDevice, tap.Attrs().OperState)

	// Verify TAP attached to a bridge
	master, err := netlink.LinkByIndex(tap.Attrs().MasterIndex)
	require.NoError(t, err)
	_, isBridge := master.(*netlink.Bridge)
	assert.True(t, isBridge, "TAP should be attached to a bridge")
	bridgeName := master.Attrs().Name

	t.Log("Verifying orphaned bridge tc cleanup preserves live TAP state...")
	liveFlowID := createBridgeTCForTest(t, bridgeName, tap.Attrs().Index)
	require.True(t, bridgeClassExists(t, bridgeName, liveFlowID), "live bridge tc class should exist before cleanup")
	staleFlowID := createBridgeTCForTest(t, bridgeName, 1)
	deletedTC := manager.networkManager.CleanupOrphanedClasses(ctx)
	require.GreaterOrEqual(t, deletedTC, 2, "expected stale filter and class to be deleted")
	assert.True(t, bridgeFilterExistsForFlowID(t, bridgeName, liveFlowID), "live bridge tc filter should remain")
	assert.True(t, bridgeClassExists(t, bridgeName, liveFlowID), "live bridge tc class should remain")
	assert.False(t, bridgeFilterExistsForFlowID(t, bridgeName, staleFlowID), "stale bridge tc filter should be deleted")
	assert.False(t, bridgeClassExists(t, bridgeName, staleFlowID), "stale bridge tc class should be deleted")

	// Wait for nginx to start
	t.Log("Waiting for nginx to start...")
	err = waitForLogMessage(ctx, manager, inst.Id, "start worker processes", 45*time.Second)
	require.NoError(t, err, "Nginx should start")
	t.Log("Nginx is running")

	// Wait for exec agent to be ready
	t.Log("Waiting for exec agent...")
	err = waitForLogMessage(ctx, manager, inst.Id, "[guest-agent] listening", 30*time.Second)
	require.NoError(t, err, "Exec agent should be listening")
	t.Log("Exec agent is ready")

	// Standby requires running state; create may still return Initializing.
	inst, err = waitForInstanceState(ctx, manager, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)

	t.Log("Waiting for health check to report healthy...")
	inst, healthStatus, err := waitForInstanceHealthStatus(ctx, manager, inst.Id, healthcheck.StatusHealthy, 20*time.Second)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	assert.GreaterOrEqual(t, healthStatus.ConsecutiveSuccesses, 1)
	assert.NotNil(t, healthStatus.LastCheckedAt)
	assert.NotNil(t, healthStatus.LastSuccessAt)
	t.Log("Health check reported healthy")

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
	inst, err = manager.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{})
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
	assert.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, manager, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
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
	t.Logf("TAP device recreated successfully: %s oper_state=%v", allocRestored.TAPDevice, tapRestored.Attrs().OperState)

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

	// Flip health checks to a guaranteed failure and verify restart policy stop-starts the same instance.
	t.Log("Triggering restart policy from failing health check...")
	require.NotNil(t, inst.StartedAt)
	startedAtBeforeRestart := *inst.StartedAt
	inst, err = manager.UpdateInstance(ctx, inst.Id, UpdateInstanceRequest{
		HealthCheck: &healthcheck.Policy{
			Type:             healthcheck.TypeHTTP,
			Interval:         "1s",
			Timeout:          "1s",
			StartPeriod:      "1s",
			FailureThreshold: 1,
			SuccessThreshold: 1,
			HTTP: &healthcheck.HTTPCheck{
				Port:           80,
				Path:           "/definitely-missing",
				ExpectedStatus: 200,
			},
		},
		RestartPolicy: &restartpolicy.Policy{
			Policy:      restartpolicy.PolicyOnFailure,
			Backoff:     "1s",
			MaxAttempts: 1,
			StableAfter: "30s",
		},
		RestartPolicySet: true,
	})
	require.NoError(t, err)
	require.NotNil(t, inst.RestartPolicy)
	assert.Equal(t, restartpolicy.PolicyOnFailure, inst.RestartPolicy.Policy)

	inst, err = waitForRestartPolicyBlocked(ctx, manager, inst.Id, restartpolicy.BlockedReasonMaxAttemptsExceeded, 60*time.Second)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)
	require.Equal(t, 1, inst.RestartStatus.Attempts)
	require.NotNil(t, inst.StartedAt)
	assert.True(t, inst.StartedAt.After(startedAtBeforeRestart), "instance should have been started again")
	t.Log("Restart policy performed stop-start and blocked after max attempts")

	// Cleanup
	t.Log("Cleaning up instance...")
	deleteInstanceEventually(t, ctx, manager, inst.Id)

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

func tcForTest(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("tc"); err == nil {
		return path
	}
	return "/usr/sbin/tc"
}

func runTCForTest(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command(tcForTest(t), args...).CombinedOutput()
	require.NoError(t, err, "tc %s output: %s", strings.Join(args, " "), string(output))
	return string(output)
}

func bridgeClassesForTest(t *testing.T, bridgeName string) []string {
	t.Helper()
	output := runTCForTest(t, "class", "show", "dev", bridgeName)
	var classes []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.Contains(line, "class htb 1:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			classes = append(classes, fields[2])
		}
	}
	return classes
}

func bridgeClassExists(t *testing.T, bridgeName, classID string) bool {
	t.Helper()
	for _, class := range bridgeClassesForTest(t, bridgeName) {
		if class == classID {
			return true
		}
	}
	return false
}

func bridgeFilterExistsForFlowID(t *testing.T, bridgeName, flowID string) bool {
	t.Helper()
	return len(bridgeFilterHandlesForFlowID(t, bridgeName, flowID)) > 0
}

func bridgeFilterHandlesForFlowID(t *testing.T, bridgeName, flowID string) []string {
	t.Helper()
	output := runTCForTest(t, "filter", "show", "dev", bridgeName, "parent", "1:")
	var handles []string
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "filter ") {
			continue
		}
		fields := strings.Fields(line)
		handle, gotFlowID := "", ""
		for i, field := range fields {
			if i+1 >= len(fields) {
				break
			}
			switch field {
			case "handle":
				handle = fields[i+1]
			case "flowid":
				gotFlowID = fields[i+1]
			}
		}
		if handle != "" && gotFlowID == flowID {
			handles = append(handles, handle)
		}
	}
	return handles
}

func createBridgeTCForTest(t *testing.T, bridgeName string, rtIif int) string {
	t.Helper()
	used := make(map[string]bool)
	for _, classID := range bridgeClassesForTest(t, bridgeName) {
		used[classID] = true
	}

	flowID := ""
	for id := 0xff00; id <= 0xffff; id++ {
		candidate := fmt.Sprintf("1:%04x", id)
		if !used[candidate] {
			flowID = candidate
			break
		}
	}
	require.NotEmpty(t, flowID, "expected an unused test class id")

	t.Cleanup(func() {
		bestEffortDeleteBridgeFiltersForFlowID(t, bridgeName, flowID)
		_ = exec.Command(tcForTest(t), "qdisc", "del", "dev", bridgeName, "parent", flowID).Run()
		_ = exec.Command(tcForTest(t), "class", "del", "dev", bridgeName, "classid", flowID).Run()
	})

	runTCForTest(t, "class", "add", "dev", bridgeName, "parent", "1:1",
		"classid", flowID, "htb", "rate", "1mbit", "ceil", "1mbit")
	runTCForTest(t, "qdisc", "add", "dev", bridgeName, "parent", flowID, "fq_codel")
	runTCForTest(t, "filter", "add", "dev", bridgeName, "parent", "1:",
		"protocol", "all", "prio", "1", "basic",
		"match", fmt.Sprintf("meta(rt_iif eq %d)", rtIif), "flowid", flowID)

	require.True(t, bridgeClassExists(t, bridgeName, flowID), "staged bridge tc class should exist")
	require.True(t, bridgeFilterExistsForFlowID(t, bridgeName, flowID), "staged bridge tc filter should exist")
	return flowID
}

func bestEffortDeleteBridgeFiltersForFlowID(t *testing.T, bridgeName, flowID string) {
	t.Helper()
	for _, handle := range bridgeFilterHandlesForFlowID(t, bridgeName, flowID) {
		_ = exec.Command(tcForTest(t), "filter", "del", "dev", bridgeName, "parent", "1:",
			"protocol", "all", "prio", "1", "handle", handle, "basic").Run()
	}
}

func startRestartPolicyControllerForTest(t *testing.T, ctx context.Context, manager *manager) {
	t.Helper()

	controllerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- manager.StartRestartPolicyController(controllerCtx)
	}()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func startHealthCheckControllerForTest(t *testing.T, ctx context.Context, manager Manager) {
	t.Helper()

	controller := NewHealthCheckController(manager, slog.Default())
	require.NotNil(t, controller)

	controllerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- controller.Run(controllerCtx)
	}()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func waitForInstanceHealthStatus(ctx context.Context, manager Manager, instanceID string, expected healthcheck.Status, timeout time.Duration) (*Instance, healthcheck.StatusSnapshot, error) {
	timeout = integrationTestTimeout(timeout)
	deadline := time.Now().Add(timeout)
	var last healthcheck.StatusSnapshot
	lastState := StateUnknown
	lastErr := error(nil)

	for time.Now().Before(deadline) {
		inst, err := manager.GetInstance(ctx, instanceID)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}

		lastState = inst.State
		last = healthcheck.Snapshot(inst.HealthCheck, string(inst.State), inst.HealthCheckRuntime)
		if last.Status == expected {
			return inst, last, nil
		}

		time.Sleep(200 * time.Millisecond)
	}

	if lastErr != nil {
		return nil, last, fmt.Errorf("instance %s health did not reach %s within %v (last error: %w)", instanceID, expected, timeout, lastErr)
	}
	return nil, last, fmt.Errorf("instance %s health did not reach %s within %v (last state: %s, last health: %s)", instanceID, expected, timeout, lastState, last.Status)
}

func waitForRestartPolicyBlocked(ctx context.Context, manager Manager, instanceID string, reason restartpolicy.BlockedReason, timeout time.Duration) (*Instance, error) {
	timeout = integrationTestTimeout(timeout)
	deadline := time.Now().Add(timeout)
	lastState := StateUnknown
	lastStatus := restartpolicy.Status{}
	lastErr := error(nil)

	for time.Now().Before(deadline) {
		inst, err := manager.GetInstance(ctx, instanceID)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}

		lastState = inst.State
		lastStatus = inst.RestartStatus
		if inst.State == StateRunning && inst.RestartStatus.BlockedReason == reason {
			return inst, nil
		}

		time.Sleep(200 * time.Millisecond)
	}

	if lastErr != nil {
		return nil, fmt.Errorf("instance %s restart policy did not block with %s within %v (last error: %w)", instanceID, reason, timeout, lastErr)
	}
	return nil, fmt.Errorf("instance %s restart policy did not block with %s within %v (last state: %s, attempts: %d, blocked_reason: %s)", instanceID, reason, timeout, lastState, lastStatus.Attempts, lastStatus.BlockedReason)
}

// TestDockerForwardChainRestored validates recovery from an external FORWARD-chain flush.
// This test intentionally mutates host-global iptables state, so it must run non-parallel.
func TestDockerForwardChainRestored(t *testing.T) {
	// Require KVM access
	requireKVMAccess(t)

	manager, _ := setupTestManager(t)
	ctx := context.Background()

	// Initialize network so hypeman rules are present before mutation.
	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	// Check if DOCKER-FORWARD chain exists (Docker must be running on host).
	checkChain := exec.Command("iptables", "-w", "5", "-L", "DOCKER-FORWARD", "-n")
	if checkChain.Run() != nil {
		t.Skip("DOCKER-FORWARD chain not present (Docker not running), skipping")
	}

	// Verify jump currently exists.
	checkJump := exec.Command("iptables", "-w", "5", "-C", "FORWARD", "-j", "DOCKER-FORWARD")
	require.NoError(t, checkJump.Run(), "DOCKER-FORWARD jump should exist before test")

	// Safety net: restore the jump if the test fails or aborts after we delete it,
	// so we don't leave the host's Docker networking broken.
	t.Cleanup(func() {
		check := exec.Command("iptables", "-w", "5", "-C", "FORWARD", "-j", "DOCKER-FORWARD")
		if check.Run() != nil {
			restore := exec.Command("iptables", "-w", "5", "-A", "FORWARD", "-j", "DOCKER-FORWARD")
			_ = restore.Run()
		}
	})

	// Simulate the hypervisor flush: remove every jump.
	for {
		delJump := exec.Command("iptables", "-w", "5", "-D", "FORWARD", "-j", "DOCKER-FORWARD")
		if err := delJump.Run(); err != nil {
			break
		}
	}

	// Confirm it's gone.
	checkGone := exec.Command("iptables", "-w", "5", "-C", "FORWARD", "-j", "DOCKER-FORWARD")
	require.Error(t, checkGone.Run(), "DOCKER-FORWARD jump should be gone after delete")

	// Re-initialize network — this should restore the jump.
	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	// Verify jump is restored.
	checkRestored := exec.Command("iptables", "-w", "5", "-C", "FORWARD", "-j", "DOCKER-FORWARD")
	require.NoError(t, checkRestored.Run(), "ensureDockerForwardJump should have restored the DOCKER-FORWARD jump")
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
		t.Skip("/dev/kvm not available, skipping on this platform")
	}
}
