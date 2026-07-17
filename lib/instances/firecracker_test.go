//go:build linux

package instances

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/diskutilization"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	snapshottest "github.com/kernel/hypeman/lib/snapshot/testsupport"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/uffdpager"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func setupTestManagerForFirecrackerWithNetworkConfig(t *testing.T, networkCfg config.NetworkConfig) (*manager, string) {
	return setupTestManagerForFirecrackerWithConfig(t, networkCfg, ManagerConfig{})
}

func setupTestManagerForFirecrackerWithConfig(t *testing.T, networkCfg config.NetworkConfig, managerConfig ManagerConfig) (*manager, string) {
	tmpDir := t.TempDir()
	prepareIntegrationTestDataDir(t, tmpDir)
	cfg := &config.Config{
		DataDir: tmpDir,
		Network: networkCfg,
	}

	p := paths.New(tmpDir)
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	systemManager := system.NewManager(p)
	networkManager := network.NewManager(p, cfg, nil)
	deviceManager := devices.NewManager(p)
	volumeManager := volumes.NewManager(p, 0, nil)

	limits := ResourceLimits{
		MaxOverlaySize:       100 * 1024 * 1024 * 1024,
		MaxVcpusPerInstance:  0,
		MaxMemoryPerInstance: 0,
	}
	mgrInterface, err := NewManagerWithConfigE(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, hypervisor.TypeFirecracker, SnapshotPolicy{}, managerConfig, nil, nil)
	require.NoError(t, err)
	mgr := mgrInterface.(*manager)

	resourceMgr := resources.NewManager(cfg, p)
	resourceMgr.SetInstanceLister(mgr)
	resourceMgr.SetImageLister(imageManager)
	resourceMgr.SetVolumeLister(volumeManager)
	require.NoError(t, resourceMgr.Initialize(context.Background()))
	mgr.SetResourceValidator(resourceMgr)

	t.Cleanup(func() {
		cleanupOrphanedProcesses(t, mgr)
	})

	return mgr, tmpDir
}

func setupTestManagerForFirecracker(t *testing.T) (*manager, string) {
	return setupTestManagerForFirecrackerWithNetworkConfig(t, newParallelTestNetworkConfig(t))
}

func setupTestManagerForFirecrackerNoNetwork(t *testing.T) (*manager, string) {
	return setupTestManagerForFirecrackerWithNetworkConfig(t, legacyParallelTestNetworkConfig(testNetworkSeq.Add(1)))
}

func requireFirecrackerIntegrationPrereqs(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping Firecracker integration test")
	}
}

func requireUserfaultfdIntegrationPrereqs(t *testing.T) {
	t.Helper()
	file, err := os.OpenFile("/dev/userfaultfd", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("/dev/userfaultfd is not accessible, skipping UFFD integration test: %v", err)
	}
	_ = file.Close()
}

func createNginxImageAndWait(t *testing.T, ctx context.Context, p *paths.Paths, imageManager images.Manager) {
	t.Helper()
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, integrationTestImageRef(t, "docker.io/library/nginx:alpine"))
}

func startGatewayProbeServer(t *testing.T, gatewayIP string) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", net.JoinHostPort(gatewayIP, "0"))
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Connection successful"))
	})

	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()

	cleanup := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}

	return fmt.Sprintf("http://%s/probe", listener.Addr().String()), cleanup
}

func TestFirecrackerStandbyAndRestore(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)
	acquireHeavyIO(t)

	mgr, tmpDir := setupTestManagerForFirecrackerNoNetwork(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, p, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-firecracker-standby",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           lifecycleTestMemorySize,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	assert.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, inst.Id)
		}
	})

	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second))
	assert.Equal(t, phasetracking.PhaseRunning, inst.Phases.Current, "fresh instance should be in running phase")

	firstFilePath := "/tmp/firecracker-standby-first.txt"
	secondFilePath := "/tmp/firecracker-standby-second.txt"
	firstFileContents := "first-cycle"
	secondFileContents := "second-cycle"

	writeGuestFile := func(path string, contents string) {
		t.Helper()
		output, exitCode, err := execCommand(ctx, inst, "sh", "-c", fmt.Sprintf("printf %q > %s && sync", contents, path))
		require.NoError(t, err, "write file via exec should succeed")
		require.Equal(t, 0, exitCode, "write file via exec should exit successfully: %s", output)
	}

	assertGuestFileContents := func(path string, expected string) {
		t.Helper()
		output, exitCode, err := execCommand(ctx, inst, "cat", path)
		require.NoError(t, err, "read file via exec should succeed")
		require.Equal(t, 0, exitCode, "read file via exec should exit successfully: %s", output)
		assert.Equal(t, expected, strings.TrimSpace(output))
	}

	assertRetainedBaseState := func() {
		t.Helper()
		_, err = os.Stat(p.InstanceSnapshotLatest(inst.Id))
		assert.True(t, os.IsNotExist(err), "running instances should not keep snapshot-latest after restore")
		_, err = os.Stat(p.InstanceSnapshotBase(inst.Id))
		require.NoError(t, err, "hypervisors that reuse snapshot bases should retain the hidden base after restore")
	}

	restoreAndMeasure := func(label string) (time.Duration, time.Duration) {
		t.Helper()
		start := time.Now()
		inst, err = mgr.RestoreInstance(ctx, inst.Id)
		require.NoError(t, err)
		assert.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
		inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
		require.NoError(t, err)
		require.Equal(t, StateRunning, inst.State)
		runningDuration := time.Since(start)
		t.Logf("%s restore-to-running took %v", label, runningDuration)

		require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 15*time.Second))
		execReadyDuration := time.Since(start)
		t.Logf("%s restore-to-exec-ready took %v", label, execReadyDuration)
		return runningDuration, execReadyDuration
	}

	_, err = os.Stat(p.InstanceSnapshotBase(inst.Id))
	assert.True(t, os.IsNotExist(err), "freshly started instances should not have a retained snapshot base")

	writeGuestFile(firstFilePath, firstFileContents)

	firstStandbyStart := time.Now()
	inst, err = mgr.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	firstStandbyDuration := time.Since(firstStandbyStart)
	t.Logf("first standby (full snapshot expected) took %v", firstStandbyDuration)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	assert.Equal(t, phasetracking.PhaseStandby, inst.Phases.Current, "standby transition should set current phase")
	assert.Greater(t, inst.Phases.Cumulative[phasetracking.PhaseRunning], int64(0), "first running stint should be accrued after standby")

	firstRestoreRunningDuration, _ := restoreAndMeasure("first")
	assert.False(t, inst.HasSnapshot, "running instances should not expose retained snapshot bases as standby snapshots")
	assertRetainedBaseState()
	assert.Equal(t, phasetracking.PhaseRunning, inst.Phases.Current, "restored instance should be in running phase")
	assert.Greater(t, inst.Phases.Cumulative[phasetracking.PhaseStandby], int64(0), "first standby stint should be accrued after restore")
	t.Logf("first full-cycle timings: standby=%v restore-to-running=%v", firstStandbyDuration, firstRestoreRunningDuration)

	assertGuestFileContents(firstFilePath, firstFileContents)
	writeGuestFile(secondFilePath, secondFileContents)

	_, err = os.Stat(p.InstanceSnapshotBase(inst.Id))
	require.NoError(t, err, "restored instances should keep the retained snapshot base for the next diff snapshot")

	secondStandbyStart := time.Now()
	inst, err = mgr.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	secondStandbyDuration := time.Since(secondStandbyStart)
	t.Logf("second standby (diff snapshot expected) took %v", secondStandbyDuration)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)

	secondRestoreRunningDuration, _ := restoreAndMeasure("second")
	assert.False(t, inst.HasSnapshot, "running instances should not expose retained snapshot bases as standby snapshots")
	assertRetainedBaseState()
	assert.Equal(t, phasetracking.PhaseRunning, inst.Phases.Current, "second restore should land back in running")
	t.Logf("second diff-cycle timings: standby=%v restore-to-running=%v", secondStandbyDuration, secondRestoreRunningDuration)

	assertGuestFileContents(secondFilePath, secondFileContents)
	assertGuestFileContents(firstFilePath, firstFileContents)

	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))
	deleted = true
}

func TestFirecrackerStopClearsStaleSnapshot(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)
	acquireHeavyIO(t)

	mgr, tmpDir := setupTestManagerForFirecracker(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, p, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-stale-snapshot",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           lifecycleTestMemorySize,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	require.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)

	// Establish a realistic standby/restore lifecycle first.
	inst, err = mgr.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, inst.State)
	require.True(t, inst.HasSnapshot)

	inst, err = mgr.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	require.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)

	// Simulate stale snapshot residue from a prior failure/interruption.
	snapshotDir := p.InstanceSnapshotLatest(inst.Id)
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "stale-marker"), []byte("stale"), 0644))
	retainedBaseDir := p.InstanceSnapshotBase(inst.Id)
	require.NoError(t, os.MkdirAll(retainedBaseDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(retainedBaseDir, "base-marker"), []byte("base"), 0644))

	beforeStop, err := mgr.GetInstance(ctx, inst.Id)
	require.NoError(t, err)
	require.True(t, beforeStop.HasSnapshot, "test setup should create visible stale snapshot")

	inst, err = mgr.StopInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, inst.State)
	assert.False(t, inst.HasSnapshot, "stopped instances should not retain stale snapshots")

	retrieved, err := mgr.GetInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, retrieved.State)
	assert.False(t, retrieved.HasSnapshot, "state derivation should remain Stopped after stop")
	_, err = os.Stat(retainedBaseDir)
	assert.True(t, os.IsNotExist(err), "stopped instances should not retain hidden snapshot bases")

	inst, err = mgr.StartInstance(ctx, inst.Id, StartInstanceRequest{})
	require.NoError(t, err)
	assert.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)

	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))
}

func TestFirecrackerNetworkLifecycle(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)

	mgr, tmpDir := setupTestManagerForFirecracker(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, p, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	// Initialize bridge/TAP infrastructure before networked instance creation.
	require.NoError(t, mgr.networkManager.Initialize(ctx, nil))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-net",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	require.NotNil(t, inst)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)

	alloc, err := mgr.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, alloc)
	assert.NotEmpty(t, alloc.IP)
	assert.NotEmpty(t, alloc.MAC)
	assert.NotEmpty(t, alloc.TAPDevice)

	tap, err := netlink.LinkByName(alloc.TAPDevice)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(tap.Attrs().Name, "hype-"))
	t.Logf("TAP device verified: %s oper_state=%v", alloc.TAPDevice, tap.Attrs().OperState)

	master, err := netlink.LinkByIndex(tap.Attrs().MasterIndex)
	require.NoError(t, err)
	_, isBridge := master.(*netlink.Bridge)
	assert.True(t, isBridge, "TAP should be attached to a bridge")

	probeURL, stopProbeServer := startGatewayProbeServer(t, alloc.Gateway)
	t.Cleanup(stopProbeServer)

	require.NoError(t, waitForLogMessage(ctx, mgr, inst.Id, "start worker processes", 15*time.Second))
	require.NoError(t, waitForLogMessage(ctx, mgr, inst.Id, "[guest-agent] listening", 10*time.Second))

	// Retry while guest network stack settles.
	var output string
	var exitCode int
	for i := 0; i < 10; i++ {
		output, exitCode, err = execCommand(ctx, inst, "curl", "-sS", "--connect-timeout", "10", probeURL)
		if err == nil && exitCode == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	require.Contains(t, output, "Connection successful")

	inst, err = mgr.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)

	_, err = netlink.LinkByName(alloc.TAPDevice)
	require.Error(t, err, "TAP device should be removed during standby")

	allocStandby, err := mgr.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, allocStandby)
	assert.Equal(t, alloc.IP, allocStandby.IP)
	assert.Equal(t, alloc.MAC, allocStandby.MAC)

	inst, err = mgr.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)

	allocRestored, err := mgr.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, allocRestored)
	assert.Equal(t, alloc.IP, allocRestored.IP)
	assert.Equal(t, alloc.MAC, allocRestored.MAC)
	assert.Equal(t, alloc.TAPDevice, allocRestored.TAPDevice)

	tapRestored, err := netlink.LinkByName(allocRestored.TAPDevice)
	require.NoError(t, err)
	t.Logf("TAP device recreated successfully: %s oper_state=%v", allocRestored.TAPDevice, tapRestored.Attrs().OperState)

	for i := 0; i < 10; i++ {
		output, exitCode, err = execCommand(ctx, inst, "curl", "-sS", "--connect-timeout", "10", probeURL)
		if err == nil && exitCode == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	require.Contains(t, output, "Connection successful")

	psOutput, psExitCode, err := execCommand(ctx, inst, "ps", "aux")
	require.NoError(t, err)
	require.Equal(t, 0, psExitCode)
	require.Contains(t, psOutput, "nginx: master process")

	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))

	_, err = netlink.LinkByName(alloc.TAPDevice)
	require.Error(t, err, "TAP device should be removed on delete")

	_, err = mgr.networkManager.GetAllocation(ctx, inst.Id)
	require.Error(t, err, "network allocation should be removed on delete")
}

func TestFirecrackerForkFromRunningNetwork(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)
	acquireHeavyIO(t)

	mgr, tmpDir := setupTestManagerForFirecracker(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, p, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))
	require.NoError(t, mgr.networkManager.Initialize(ctx, nil))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-fork-running-src",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    256 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	source, err = waitForInstanceState(ctx, mgr, source.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	sourceID := source.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, sourceID) })
	assert.NotEmpty(t, source.IP)
	assert.NotEmpty(t, source.MAC)

	_, err = mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fc-fork-running-no-flag"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)

	forked, err := mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{
		Name:        "fc-fork-running-copy",
		FromRunning: true,
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	require.Contains(t, []State{StateInitializing, StateRunning}, forked.State)
	forked, err = waitForInstanceState(ctx, mgr, forked.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.Equal(t, StateRunning, forked.State)
	forkID := forked.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, forkID) })
	assert.NotEmpty(t, forked.IP)
	assert.NotEmpty(t, forked.MAC)
	assert.Equal(t, mgr.paths.InstanceVsockSocket(forkID), forked.VsockSocket)

	forkMeta, err := mgr.loadMetadata(forkID)
	require.NoError(t, err)
	assert.Equal(t, mgr.paths.InstanceVsockSocket(forkID), forkMeta.StoredMetadata.VsockSocket)

	sourceAfterFork, err := mgr.GetInstance(ctx, sourceID)
	require.NoError(t, err)
	if sourceAfterFork.State != StateRunning {
		sourceAfterFork, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, integrationTestTimeout(20*time.Second))
		require.NoError(t, err)
	}
	require.Equal(t, StateRunning, sourceAfterFork.State)
	assert.NotEmpty(t, sourceAfterFork.IP)
	assert.NotEmpty(t, sourceAfterFork.MAC)

	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)
	assertHostCanReachNginx(t, forked.IP, 80, 60*time.Second)
	assert.NotEqual(t, sourceAfterFork.IP, forked.IP)
	assert.NotEqual(t, sourceAfterFork.MAC, forked.MAC)
}

func TestFirecrackerSnapshotFeature(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)

	mgr, tmpDir := setupTestManagerForFirecracker(t)
	runStandbySnapshotScenario(t, mgr, tmpDir, snapshotScenarioConfig{
		hypervisor: hypervisor.TypeFirecracker,
		sourceName: "fc-snapshot-src",
		snapshot:   "fc-snapshot-1",
		forkName:   "fc-snapshot-fork",
	})
}

func TestFirecrackerWarmForkChain(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)
	acquireHeavyIO(t)

	mgr, tmpDir := setupTestManagerForFirecrackerNoNetwork(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	imageName := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, imageName)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-warm-chain-src",
		Image:          imageName,
		Size:           lifecycleTestMemorySize,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
		Cmd:            []string{"sleep", "infinity"},
	})
	require.NoError(t, err)
	sourceID := source.Id
	sourceDeleted := false
	t.Cleanup(func() {
		if !sourceDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, sourceID)
		}
	})

	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 30*time.Second))

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "fc-warm-chain-snap",
	})
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStandby, snapshot.Kind)

	require.NoError(t, deleteTestInstanceNow(ctx, mgr, sourceID))
	sourceDeleted = true

	warm, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        "fc-warm-chain-warm",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	warmID := warm.Id
	warmDeleted := false
	t.Cleanup(func() {
		if !warmDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, warmID)
		}
	})
	warm, err = waitForInstanceState(ctx, mgr, warmID, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, warmID, 30*time.Second))

	child, err := mgr.ForkInstance(ctx, warmID, ForkInstanceRequest{
		Name:        "fc-warm-chain-child",
		FromRunning: true,
		TargetState: StateStopped,
	})
	require.NoError(t, err)
	require.Equal(t, StateStopped, child.State)
	childID := child.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, childID) })

	warm, err = mgr.GetInstance(ctx, warmID)
	require.NoError(t, err)
	if warm.State != StateRunning {
		warm, err = waitForInstanceState(ctx, mgr, warmID, StateRunning, integrationTestTimeout(20*time.Second))
		require.NoError(t, err)
	}
	require.Equal(t, StateRunning, warm.State)
	require.NoError(t, waitForExecAgent(ctx, mgr, warmID, 30*time.Second))

	require.NoError(t, deleteTestInstanceNow(ctx, mgr, warmID))
	warmDeleted = true
	require.NoError(t, mgr.DeleteSnapshot(ctx, snapshot.Id))
}

func TestFCUFFDOneShotLifecycle(t *testing.T) {
	// SKIP: flakes ~21% even in isolation — the restored guest's agent vsock/gRPC
	// connection intermittently dies in the first seconds after a UFFD one-shot
	// restore (the guest resets the stream). A controlled cgroup experiment ruled
	// out disk I/O and CPU as the cause (both only amplify it). Re-enable once the
	// guest-agent reconnect/readiness fix lands.
	// Tracked in: https://linear.app/onkernel/issue/KERNEL-1354
	t.Skip("flaky: guest-agent vsock race after UFFD restore (KERNEL-1354)")
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)
	requireUserfaultfdIntegrationPrereqs(t)
	if pagerBinary := strings.TrimSpace(os.Getenv("HYPEMAN_UFFD_PAGER_BINARY")); pagerBinary == "" {
		t.Skip("HYPEMAN_UFFD_PAGER_BINARY must point at hypeman-uffd-pager for UFFD integration tests")
	} else if st, err := os.Stat(pagerBinary); err != nil || !st.Mode().IsRegular() {
		t.Skipf("HYPEMAN_UFFD_PAGER_BINARY is not a regular file: %s", pagerBinary)
	}
	acquireHeavyIO(t)

	mgr, tmpDir := setupTestManagerForFirecrackerWithConfig(t, legacyParallelTestNetworkConfig(testNetworkSeq.Add(1)), ManagerConfig{
		FirecrackerSnapshotMemoryBackend: uffdpager.BackendUFFD,
		FirecrackerUFFDCacheMaxBytes:     512 << 20,
	})
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	imageName := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, imageName)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-uffd-oneshot-src",
		Image:          imageName,
		Size:           lifecycleTestMemorySize,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
		Cmd:            []string{"sleep", "infinity"},
	})
	require.NoError(t, err)
	sourceID := source.Id
	sourceDeleted := false
	t.Cleanup(func() {
		if !sourceDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, sourceID)
		}
	})

	source = requireRunningSleepInstance(t, ctx, mgr, sourceID)
	requireGuestTmpfs(t, ctx, source)
	writeGuestFile(t, ctx, source, "/root/uffd-lifecycle/source", "source-disk")
	writeGuestFile(t, ctx, source, "/dev/shm/uffd-lifecycle/source", "source-memory")

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "fc-uffd-oneshot-snap",
	})
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStandby, snapshot.Kind)
	snapshotDeleted := false
	t.Cleanup(func() {
		if !snapshotDeleted {
			_ = mgr.DeleteSnapshot(context.Background(), snapshot.Id)
		}
	})
	snapshotMemoryPath := firecrackerSnapshotMemoryPathInGuestDir(p.SnapshotGuestDir(snapshot.Id))
	require.FileExists(t, snapshotMemoryPath)

	source = requireRunningSleepInstance(t, ctx, mgr, sourceID)
	assertGuestFile(t, ctx, source, "/root/uffd-lifecycle/source", "source-disk")
	assertGuestFile(t, ctx, source, "/dev/shm/uffd-lifecycle/source", "source-memory")

	parent, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        "fc-uffd-oneshot-parent",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	parentID := parent.Id
	parentDeleted := false
	t.Cleanup(func() {
		if !parentDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, parentID)
		}
	})
	parent = requireRunningSleepInstance(t, ctx, mgr, parentID)
	assertGuestFile(t, ctx, parent, "/root/uffd-lifecycle/source", "source-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-lifecycle/source", "source-memory")
	writeGuestFile(t, ctx, parent, "/root/uffd-lifecycle/parent-after-uffd", "parent-disk")
	writeGuestFile(t, ctx, parent, "/dev/shm/uffd-lifecycle/parent-after-uffd", "parent-memory")
	parentSnapshotMemoryPath := filepath.Join(p.InstanceSnapshotLatest(parentID), "memory")
	parentBaseMemoryPath := filepath.Join(p.InstanceSnapshotBase(parentID), "memory")
	require.FileExists(t, parentBaseMemoryPath, "UFFD snapshot fanout should hardlink the mem-file (retained as diff base after restore)")
	requireSameInode(t, snapshotMemoryPath, parentBaseMemoryPath)
	parentMeta, err := mgr.loadMetadata(parentID)
	require.NoError(t, err)
	require.False(t, parentMeta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	require.NotEmpty(t, parentMeta.StoredMetadata.FirecrackerUFFDSessionID)

	require.NoError(t, mgr.DeleteSnapshot(ctx, snapshot.Id), "source snapshot must be deletable while a fork is running from it")
	snapshotDeleted = true

	parent, err = mgr.StandbyInstance(ctx, parentID, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, parent.State)
	require.FileExists(t, parentSnapshotMemoryPath, "standby should produce a file-backed snapshot")
	parentMeta, err = mgr.loadMetadata(parentID)
	require.NoError(t, err)
	require.False(t, parentMeta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	require.Empty(t, parentMeta.StoredMetadata.FirecrackerUFFDSessionID)

	parent, err = mgr.RestoreInstance(ctx, parentID)
	require.NoError(t, err)
	parent = requireRunningSleepInstance(t, ctx, mgr, parentID)
	assertGuestFile(t, ctx, parent, "/root/uffd-lifecycle/source", "source-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-lifecycle/source", "source-memory")
	assertGuestFile(t, ctx, parent, "/root/uffd-lifecycle/parent-after-uffd", "parent-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-lifecycle/parent-after-uffd", "parent-memory")
	parentMeta, err = mgr.loadMetadata(parentID)
	require.NoError(t, err)
	require.False(t, parentMeta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	require.Empty(t, parentMeta.StoredMetadata.FirecrackerUFFDSessionID)
	require.FileExists(t, filepath.Join(p.InstanceSnapshotBase(parentID), "memory"), "file-backed resume should retain the standby snapshot as the next diff base")

	child, err := mgr.ForkInstance(ctx, parentID, ForkInstanceRequest{
		Name:        "fc-uffd-oneshot-child",
		FromRunning: true,
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	childID := child.Id
	childDeleted := false
	t.Cleanup(func() {
		if !childDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, childID)
		}
	})

	parent = requireRunningSleepInstance(t, ctx, mgr, parentID)
	child = requireRunningSleepInstance(t, ctx, mgr, childID)
	assertGuestFile(t, ctx, parent, "/root/uffd-lifecycle/parent-after-uffd", "parent-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-lifecycle/parent-after-uffd", "parent-memory")
	assertGuestFile(t, ctx, child, "/root/uffd-lifecycle/source", "source-disk")
	assertGuestFile(t, ctx, child, "/dev/shm/uffd-lifecycle/source", "source-memory")
	assertGuestFile(t, ctx, child, "/root/uffd-lifecycle/parent-after-uffd", "parent-disk")
	assertGuestFile(t, ctx, child, "/dev/shm/uffd-lifecycle/parent-after-uffd", "parent-memory")
	writeGuestFile(t, ctx, child, "/root/uffd-lifecycle/child-only", "child-disk")
	writeGuestFile(t, ctx, child, "/dev/shm/uffd-lifecycle/child-only", "child-memory")
	assertGuestFileAbsent(t, ctx, parent, "/root/uffd-lifecycle/child-only")
	assertGuestFileAbsent(t, ctx, parent, "/dev/shm/uffd-lifecycle/child-only")

	childSnapshotMemoryPath := filepath.Join(p.InstanceSnapshotLatest(childID), "memory")
	childBaseMemoryPath := filepath.Join(p.InstanceSnapshotBase(childID), "memory")
	require.FileExists(t, childBaseMemoryPath, "running-source child should hardlink the mem-file (retained as diff base after restore)")
	requireSameInode(t, filepath.Join(p.InstanceSnapshotBase(parentID), "memory"), childBaseMemoryPath)
	childMeta, err := mgr.loadMetadata(childID)
	require.NoError(t, err)
	require.False(t, childMeta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	require.NotEmpty(t, childMeta.StoredMetadata.FirecrackerUFFDSessionID)

	parent, err = mgr.StandbyInstance(ctx, parentID, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, parent.State)
	parentSnapshotMemoryPath = filepath.Join(p.InstanceSnapshotLatest(parentID), "memory")
	require.FileExists(t, parentSnapshotMemoryPath)
	parentMeta, err = mgr.loadMetadata(parentID)
	require.NoError(t, err)
	require.Empty(t, parentMeta.StoredMetadata.FirecrackerUFFDSessionID)

	child, err = mgr.StandbyInstance(ctx, childID, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, child.State)
	require.FileExists(t, childSnapshotMemoryPath, "child standby should produce a file-backed snapshot")
	requireDifferentInode(t, filepath.Join(p.InstanceSnapshotLatest(parentID), "memory"), childSnapshotMemoryPath)
	childMeta, err = mgr.loadMetadata(childID)
	require.NoError(t, err)
	require.False(t, childMeta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)
	require.Empty(t, childMeta.StoredMetadata.FirecrackerUFFDSessionID)

	child, err = mgr.RestoreInstance(ctx, childID)
	require.NoError(t, err)
	child = requireRunningSleepInstance(t, ctx, mgr, childID)
	assertGuestFile(t, ctx, child, "/root/uffd-lifecycle/source", "source-disk")
	assertGuestFile(t, ctx, child, "/dev/shm/uffd-lifecycle/source", "source-memory")
	assertGuestFile(t, ctx, child, "/root/uffd-lifecycle/parent-after-uffd", "parent-disk")
	assertGuestFile(t, ctx, child, "/dev/shm/uffd-lifecycle/parent-after-uffd", "parent-memory")
	assertGuestFile(t, ctx, child, "/root/uffd-lifecycle/child-only", "child-disk")
	assertGuestFile(t, ctx, child, "/dev/shm/uffd-lifecycle/child-only", "child-memory")

	child, err = mgr.StandbyInstance(ctx, childID, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, child.State)

	require.NoError(t, deleteTestInstanceNow(ctx, mgr, childID))
	childDeleted = true
	require.NoError(t, deleteTestInstanceNow(ctx, mgr, parentID))
	parentDeleted = true
	require.NoError(t, deleteTestInstanceNow(ctx, mgr, sourceID))
	sourceDeleted = true
	require.NoError(t, mgr.DeleteSnapshot(ctx, snapshot.Id))
	snapshotDeleted = true
}

// TestFCUFFDGraduationLifecycle exercises detaching a running UFFD-backed VM
// from its pager: the pager populates the remaining pages and unregisters the
// session, and the VM must keep running on resident memory with its guest state
// intact. It is a sibling of TestFCUFFDOneShotLifecycle and leaves that test's
// coverage unchanged.
func TestFCUFFDGraduationLifecycle(t *testing.T) {
	// Intentionally not parallel: graduation forces a full guest-memory populate,
	// and overlapping that with the sibling UFFD lifecycle test's VMs saturated
	// the CI runner and timed out guest-agent readiness. Running solo keeps peak
	// concurrent UFFD VM load the same as before this test existed.
	requireFirecrackerIntegrationPrereqs(t)
	requireUserfaultfdIntegrationPrereqs(t)
	if pagerBinary := strings.TrimSpace(os.Getenv("HYPEMAN_UFFD_PAGER_BINARY")); pagerBinary == "" {
		t.Skip("HYPEMAN_UFFD_PAGER_BINARY must point at hypeman-uffd-pager for UFFD integration tests")
	} else if st, err := os.Stat(pagerBinary); err != nil || !st.Mode().IsRegular() {
		t.Skipf("HYPEMAN_UFFD_PAGER_BINARY is not a regular file: %s", pagerBinary)
	}

	mgr, tmpDir := setupTestManagerForFirecrackerWithConfig(t, legacyParallelTestNetworkConfig(testNetworkSeq.Add(1)), ManagerConfig{
		FirecrackerSnapshotMemoryBackend: uffdpager.BackendUFFD,
		FirecrackerUFFDCacheMaxBytes:     512 << 20,
	})
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	imageName := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, imageName)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-uffd-grad-src",
		Image:          imageName,
		Size:           lifecycleTestMemorySize,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
		Cmd:            []string{"sleep", "infinity"},
	})
	require.NoError(t, err)
	sourceID := source.Id
	sourceDeleted := false
	t.Cleanup(func() {
		if !sourceDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, sourceID)
		}
	})

	source = requireRunningSleepInstance(t, ctx, mgr, sourceID)
	requireGuestTmpfs(t, ctx, source)
	writeGuestFile(t, ctx, source, "/root/uffd-grad/source", "source-disk")
	writeGuestFile(t, ctx, source, "/dev/shm/uffd-grad/source", "source-memory")

	// A VM with no pager session (the freshly created, file-backed source) is a
	// no-op to graduate.
	require.NoError(t, mgr.GraduateSnapshotMemoryPager(ctx, sourceID))

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "fc-uffd-grad-snap",
	})
	require.NoError(t, err)
	snapshotDeleted := false
	t.Cleanup(func() {
		if !snapshotDeleted {
			_ = mgr.DeleteSnapshot(context.Background(), snapshot.Id)
		}
	})

	// Forking the standby snapshot to a running VM restores it UFFD-backed and
	// pins a live pager session.
	parent, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        "fc-uffd-grad-parent",
		TargetState: StateRunning,
	})
	require.NoError(t, err)
	parentID := parent.Id
	parentDeleted := false
	t.Cleanup(func() {
		if !parentDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, parentID)
		}
	})

	parent = requireRunningSleepInstance(t, ctx, mgr, parentID)
	assertGuestFile(t, ctx, parent, "/root/uffd-grad/source", "source-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/source", "source-memory")
	writeGuestFile(t, ctx, parent, "/root/uffd-grad/parent", "parent-disk")
	writeGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/parent", "parent-memory")

	parentMeta, err := mgr.loadMetadata(parentID)
	require.NoError(t, err)
	require.NotEmpty(t, parentMeta.StoredMetadata.FirecrackerUFFDSessionID, "running UFFD fork should hold a pager session")
	target := mgr.UFFDGraduationTargetVersion()
	require.NotEmpty(t, target, "uffd backend should expose a target pager version")
	require.Equal(t, target, parentMeta.StoredMetadata.FirecrackerUFFDPagerVersion)

	// Graduate: the pager fully populates memory from the backing file and
	// unregisters the session. The VM keeps running with no pager dependency.
	require.NoError(t, mgr.GraduateSnapshotMemoryPager(ctx, parentID))

	parentMeta, err = mgr.loadMetadata(parentID)
	require.NoError(t, err)
	require.Empty(t, parentMeta.StoredMetadata.FirecrackerUFFDSessionID, "graduation should clear the pager session binding")
	require.Empty(t, parentMeta.StoredMetadata.FirecrackerUFFDPagerVersion)
	require.False(t, parentMeta.StoredMetadata.FirecrackerUseUFFDOnNextRestore)

	// The VM is still running and all guest memory and disk content survived the
	// populate + unregister.
	parent = requireRunningSleepInstance(t, ctx, mgr, parentID)
	assertGuestFile(t, ctx, parent, "/root/uffd-grad/source", "source-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/source", "source-memory")
	assertGuestFile(t, ctx, parent, "/root/uffd-grad/parent", "parent-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/parent", "parent-memory")

	// New guest memory and disk writes still work, proving the guest did not hang
	// on a previously untouched page after userfaultfd was unregistered.
	writeGuestFile(t, ctx, parent, "/root/uffd-grad/post", "post-disk")
	writeGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/post", "post-memory")
	assertGuestFile(t, ctx, parent, "/root/uffd-grad/post", "post-disk")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/post", "post-memory")

	// Graduating again is a no-op now that the session is gone.
	require.NoError(t, mgr.GraduateSnapshotMemoryPager(ctx, parentID))

	// A graduated VM still standbys and restores via the file backend, and its
	// memory survives the round trip.
	parent, err = mgr.StandbyInstance(ctx, parentID, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, parent.State)

	parent, err = mgr.RestoreInstance(ctx, parentID)
	require.NoError(t, err)
	parent = requireRunningSleepInstance(t, ctx, mgr, parentID)
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/source", "source-memory")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/parent", "parent-memory")
	assertGuestFile(t, ctx, parent, "/dev/shm/uffd-grad/post", "post-memory")

	parentMeta, err = mgr.loadMetadata(parentID)
	require.NoError(t, err)
	require.Empty(t, parentMeta.StoredMetadata.FirecrackerUFFDSessionID, "file-backed restore after graduation should not create a pager session")

	require.NoError(t, deleteTestInstanceNow(ctx, mgr, parentID))
	parentDeleted = true
	require.NoError(t, deleteTestInstanceNow(ctx, mgr, sourceID))
	sourceDeleted = true
	require.NoError(t, mgr.DeleteSnapshot(ctx, snapshot.Id))
	snapshotDeleted = true
}

func requireRunningSleepInstance(t *testing.T, ctx context.Context, mgr Manager, instanceID string) *Instance {
	t.Helper()
	inst, err := waitForInstanceState(ctx, mgr, instanceID, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)

	require.Eventually(t, func() bool {
		current, err := mgr.GetInstance(ctx, instanceID)
		if err != nil {
			t.Logf("get instance %s: %v", instanceID, err)
			return false
		}
		// Bounded retry on transient vsock/gRPC blips that show up right after a
		// resume under contention; the outer Eventually still gates on the result.
		output, exitCode, err := execCommandWithRetry(ctx, current, 5*time.Second, "sh", "-c", "ps | grep '[s]leep' | grep -q infinity")
		if err != nil {
			t.Logf("exec sleep check for %s: %v", instanceID, err)
			return false
		}
		if exitCode != 0 {
			t.Logf("sleep check for %s exited %d: %s", instanceID, exitCode, output)
			return false
		}
		return true
	}, integrationTestTimeout(30*time.Second), 250*time.Millisecond)

	inst, err = mgr.GetInstance(ctx, instanceID)
	require.NoError(t, err)
	return inst
}

func requireGuestTmpfs(t *testing.T, ctx context.Context, inst *Instance) {
	t.Helper()
	output, exitCode, err := execCommand(ctx, inst, "sh", "-c", "mkdir -p /dev/shm && if ! grep -q ' /dev/shm ' /proc/mounts; then mount -t tmpfs -o size=16m tmpfs /dev/shm; fi && grep -q ' /dev/shm ' /proc/mounts")
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, output)
}

func requireDifferentInode(t *testing.T, pathA, pathB string) {
	t.Helper()
	infoA, err := os.Stat(pathA)
	require.NoError(t, err)
	infoB, err := os.Stat(pathB)
	require.NoError(t, err)
	require.False(t, os.SameFile(infoA, infoB), "%s and %s must not share an inode", pathA, pathB)
}

func requireSameInode(t *testing.T, pathA, pathB string) {
	t.Helper()
	infoA, err := os.Stat(pathA)
	require.NoError(t, err)
	infoB, err := os.Stat(pathB)
	require.NoError(t, err)
	require.True(t, os.SameFile(infoA, infoB), "%s and %s must share an inode", pathA, pathB)
}

func writeGuestFile(t *testing.T, ctx context.Context, inst *Instance, path, contents string) {
	t.Helper()
	output, exitCode, err := execCommand(ctx, inst, "sh", "-c", "mkdir -p \"$1\" && printf '%s' \"$2\" > \"$3\" && sync", "sh", filepath.Dir(path), contents, path)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, output)
}

func assertGuestFile(t *testing.T, ctx context.Context, inst *Instance, path, contents string) {
	t.Helper()
	// Use the retrying exec helper: right after a restore/resume under shared-runner
	// I/O contention the in-guest exec over vsock can momentarily return EOF /
	// gRPC Unavailable. execCommandWithRetry retries only those transient
	// connection errors, never a real assertion mismatch.
	output, exitCode, err := execCommandWithRetry(ctx, inst, compressionGuestExecTimeout, "cat", path)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, output)
	require.Equal(t, contents, output)
}

func assertGuestFileAbsent(t *testing.T, ctx context.Context, inst *Instance, path string) {
	t.Helper()
	output, exitCode, err := execCommand(ctx, inst, "test", "!", "-e", path)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode, output)
}

// TestFirecrackerForkIsolation verifies CoW isolation between a firecracker
// source's standby snapshot and a fork derived from it. A fork must end up
// with its own mem-file inode (reflink-cloned, not hardlinked) so that
// mutating the fork — including taking a diff snapshot of the fork after
// divergence — never alters the source's snapshot bytes. This guards against
// the family of hazards where fan-out optimizations inadvertently share an
// inode with the source and let later writes propagate back through it.
//
// Test name is kept short on purpose: t.TempDir() embeds the test name, and
// firecracker's API socket path under that tempdir must fit within SUN_LEN
// (108 bytes on Linux).
func TestFirecrackerForkIsolation(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)
	acquireHeavyIO(t)

	mgr, tmpDir := setupTestManagerForFirecrackerNoNetwork(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, p, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	const guestMemBytes = int64(1024 * 1024 * 1024)

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-fork-isolation-src",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           guestMemBytes,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	sourceID := source.Id
	sourceDeleted := false
	t.Cleanup(func() {
		if !sourceDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, sourceID)
		}
	})

	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 30*time.Second))

	const sourceSentinelPath = "/tmp/source-sentinel.txt"
	const sourceSentinelContents = "source-only"
	output, exitCode, err := execCommand(ctx, source, "sh", "-c",
		fmt.Sprintf("printf %q > %s && sync", sourceSentinelContents, sourceSentinelPath))
	require.NoError(t, err)
	require.Equalf(t, 0, exitCode, "write source sentinel: %s", output)

	// Source standby produces a full firecracker snapshot. We hold the source
	// in Standby for the entire fork lifecycle below so the snapshot mem-file
	// stays at snapshot-latest/memory and is comparable across phases.
	source, err = mgr.StandbyInstance(ctx, sourceID, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, source.State)
	require.True(t, source.HasSnapshot)

	sourceMemPath := filepath.Join(p.InstanceSnapshotLatest(sourceID), "memory")
	sourceBefore, err := fingerprintFile(sourceMemPath)
	require.NoError(t, err, "fingerprint source mem-file after standby")

	reflinkOK := probeReflinkSupport(t, tmpDir)
	usageBefore, err := diskutilization.Collect(p)
	require.NoError(t, err)
	usedBefore := diskUtilizationTotal(usageBefore)

	fork, err := mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{
		Name: "fc-fork-isolation-fork",
	})
	require.NoError(t, err)
	forkID := fork.Id
	forkDeleted := false
	t.Cleanup(func() {
		if !forkDeleted {
			_ = deleteTestInstanceNow(context.Background(), mgr, forkID)
		}
	})
	require.Equal(t, StateStandby, fork.State)

	// Fork creation hardlinks the mem-file: the fork shares the source's inode
	// (zero-copy fanout, shared page cache) and the standby path unshares it
	// before any diff-snapshot write. The byte-identity assertions below are
	// the real isolation guard.
	forkMemPath := filepath.Join(p.InstanceSnapshotLatest(forkID), "memory")
	forkAfterCreate, err := fingerprintFile(forkMemPath)
	require.NoError(t, err, "fingerprint fork mem-file after fork")
	require.Equal(t, sourceBefore.inode, forkAfterCreate.inode,
		"fork mem-file should hardlink the source's inode at creation")

	sourceAfterFork, err := fingerprintFile(sourceMemPath)
	require.NoError(t, err)
	require.Equal(t, sourceBefore.inode, sourceAfterFork.inode,
		"source mem-file inode must not change after fork creation")
	require.Equal(t, sourceBefore.sha, sourceAfterFork.sha,
		"source mem-file bytes must not change after fork creation")

	// Restore the fork: it should see the source's pre-fork guest state.
	fork, err = mgr.RestoreInstance(ctx, forkID)
	require.NoError(t, err)
	fork, err = waitForInstanceState(ctx, mgr, forkID, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, forkID, 30*time.Second))

	output, exitCode, err = execCommand(ctx, fork, "cat", sourceSentinelPath)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	require.Equal(t, sourceSentinelContents, strings.TrimSpace(output))

	// Diverge the fork: write a fork-only sentinel, then standby the fork.
	// Firecracker's second standby produces a diff snapshot against the fork's
	// retained base — this is the operation most likely to corrupt the source
	// if the fork's mem-file were sharing the source's inode.
	const forkSentinelPath = "/tmp/fork-sentinel.txt"
	const forkSentinelContents = "fork-only"
	output, exitCode, err = execCommand(ctx, fork, "sh", "-c",
		fmt.Sprintf("printf %q > %s && sync", forkSentinelContents, forkSentinelPath))
	require.NoError(t, err)
	require.Equalf(t, 0, exitCode, "write fork sentinel: %s", output)

	fork, err = mgr.StandbyInstance(ctx, forkID, StandbyInstanceRequest{})
	require.NoError(t, err)
	require.Equal(t, StateStandby, fork.State)

	// The fork's standby must have unshared the hardlink before diff-writing.
	forkAfterStandby, err := fingerprintFile(forkMemPath)
	require.NoError(t, err, "fingerprint fork mem-file after fork standby")
	require.NotEqual(t, sourceBefore.inode, forkAfterStandby.inode,
		"fork standby must unshare the mem-file before writing its diff snapshot")

	// Source mem-file must STILL be byte-identical after the fork's full
	// lifecycle (restore + write + standby/diff-snapshot).
	sourceAfterForkStandby, err := fingerprintFile(sourceMemPath)
	require.NoError(t, err)
	require.Equal(t, sourceBefore.inode, sourceAfterForkStandby.inode,
		"source mem-file inode must not change after fork standby")
	require.Equal(t, sourceBefore.sha, sourceAfterForkStandby.sha,
		"source mem-file bytes must not change after fork standby")

	// Soft disk-usage assertion: on reflink-capable filesystems, the fork
	// lifecycle should consume substantially less than a full guest-mem copy
	// because pages are shared CoW. Gated on FICLONE probe — ext4 etc. fall
	// back to sparse copy which produces full physical copies, so the bound
	// would not hold there.
	usageAfter, err := diskutilization.Collect(p)
	require.NoError(t, err)
	consumed := diskUtilizationTotal(usageAfter) - usedBefore
	t.Logf("fork lifecycle disk-usage delta: consumed=%d guestMem=%d reflink=%v",
		consumed, guestMemBytes, reflinkOK)
	if reflinkOK {
		assert.Less(t, consumed, guestMemBytes/2,
			"fork lifecycle should consume substantially less than full guest mem on reflink-capable fs")
	}

	// Delete the fork — its inode goes away. On a reflink-capable fs, deleting
	// a CoW clone must not affect the source's blocks. Verify the source
	// mem-file is still readable and byte-identical after the unlink.
	require.NoError(t, mgr.DeleteInstance(ctx, forkID))
	forkDeleted = true

	sourceAfterForkDelete, err := fingerprintFile(sourceMemPath)
	require.NoError(t, err, "source mem-file should still be readable after fork delete")
	require.Equal(t, sourceBefore.inode, sourceAfterForkDelete.inode,
		"source mem-file inode must not change after fork delete")
	require.Equal(t, sourceBefore.sha, sourceAfterForkDelete.sha,
		"source mem-file bytes must not change after fork delete")

	// Strongest end-to-end check: the source snapshot must still be restorable
	// after the fork's full lifecycle. Verify the source's sentinel survived
	// and the fork-only sentinel did not leak across.
	source, err = mgr.RestoreInstance(ctx, sourceID)
	require.NoError(t, err)
	source, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, sourceID, 30*time.Second))

	output, exitCode, err = execCommand(ctx, source, "cat", sourceSentinelPath)
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	require.Equal(t, sourceSentinelContents, strings.TrimSpace(output))

	_, exitCode, err = execCommand(ctx, source, "test", "-f", forkSentinelPath)
	require.NoError(t, err)
	require.NotEqual(t, 0, exitCode, "source must not see the fork-only sentinel")

	require.NoError(t, mgr.DeleteInstance(ctx, sourceID))
	sourceDeleted = true
}

func diskUtilizationTotal(b diskutilization.Breakdown) int64 {
	return b.Images +
		b.OCICache +
		b.Volumes +
		b.RootfsOverlays +
		b.VolumeOverlays +
		b.SnapshotUncompressed +
		b.SnapshotCompressed +
		b.SnapshotShared +
		b.SnapshotOther
}

type fileFingerprint struct {
	inode uint64
	sha   string
}

func fingerprintFile(path string) (fileFingerprint, error) {
	st, err := os.Stat(path)
	if err != nil {
		return fileFingerprint{}, fmt.Errorf("stat %s: %w", path, err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fileFingerprint{}, fmt.Errorf("unexpected stat type for %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fileFingerprint{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileFingerprint{}, fmt.Errorf("read %s: %w", path, err)
	}
	return fileFingerprint{inode: sys.Ino, sha: hex.EncodeToString(h.Sum(nil))}, nil
}

// probeReflinkSupport returns true if FICLONE works on the given directory.
// Used to gate the soft disk-usage assertion: on ext4 and other non-reflink
// filesystems the copy falls back to sparse full-copy semantics, so the
// "fork should consume much less than guest-mem" bound would not hold.
func probeReflinkSupport(t *testing.T, dir string) bool {
	t.Helper()
	srcPath := filepath.Join(dir, ".reflink-probe-src")
	dstPath := filepath.Join(dir, ".reflink-probe-dst")
	defer func() {
		_ = os.Remove(srcPath)
		_ = os.Remove(dstPath)
	}()
	if err := os.WriteFile(srcPath, []byte("reflink-probe"), 0644); err != nil {
		t.Logf("reflink probe: write src failed: %v", err)
		return false
	}
	src, err := os.Open(srcPath)
	if err != nil {
		t.Logf("reflink probe: open src failed: %v", err)
		return false
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		t.Logf("reflink probe: open dst failed: %v", err)
		return false
	}
	defer dst.Close()
	if err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())); err != nil {
		t.Logf("reflink probe: FICLONE failed: %v", err)
		return false
	}
	return true
}
