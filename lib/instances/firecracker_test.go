//go:build linux

package instances

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
)

func setupTestManagerForFirecrackerWithNetworkConfig(t *testing.T, networkCfg config.NetworkConfig) (*manager, string) {
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
	mgr := NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, hypervisor.TypeFirecracker, nil, nil).(*manager)

	resourceMgr := resources.NewManager(cfg, p)
	resourceMgr.SetInstanceLister(mgr)
	resourceMgr.SetImageLister(imageManager)
	resourceMgr.SetVolumeLister(volumeManager)
	require.NoError(t, resourceMgr.Initialize(context.Background()))
	mgr.SetResourceValidator(resourceMgr)

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

func createNginxImageAndWait(t *testing.T, ctx context.Context, imageManager images.Manager) {
	t.Helper()

	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
	})
	require.NoError(t, err)

	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, nginxImage.Name)
		if err == nil && img.Status == images.StatusReady {
			return
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}

	t.Fatalf("timed out waiting for image %q to become ready", nginxImage.Name)
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

	mgr, tmpDir := setupTestManagerForFirecrackerNoNetwork(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "test-firecracker-standby",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           1024 * 1024 * 1024,
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
			_ = mgr.DeleteInstance(context.Background(), inst.Id)
		}
	})

	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, 20*time.Second)
	require.NoError(t, err)
	require.NoError(t, waitForExecAgent(ctx, mgr, inst.Id, 30*time.Second))

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
		inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, 20*time.Second)
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
	inst, err = mgr.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	firstStandbyDuration := time.Since(firstStandbyStart)
	t.Logf("first standby (full snapshot expected) took %v", firstStandbyDuration)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)

	firstRestoreRunningDuration, _ := restoreAndMeasure("first")
	assert.False(t, inst.HasSnapshot, "running instances should not expose retained snapshot bases as standby snapshots")
	assertRetainedBaseState()
	t.Logf("first full-cycle timings: standby=%v restore-to-running=%v", firstStandbyDuration, firstRestoreRunningDuration)

	assertGuestFileContents(firstFilePath, firstFileContents)
	writeGuestFile(secondFilePath, secondFileContents)

	_, err = os.Stat(p.InstanceSnapshotBase(inst.Id))
	require.NoError(t, err, "restored instances should keep the retained snapshot base for the next diff snapshot")

	secondStandbyStart := time.Now()
	inst, err = mgr.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	secondStandbyDuration := time.Since(secondStandbyStart)
	t.Logf("second standby (diff snapshot expected) took %v", secondStandbyDuration)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)

	secondRestoreRunningDuration, _ := restoreAndMeasure("second")
	assert.False(t, inst.HasSnapshot, "running instances should not expose retained snapshot bases as standby snapshots")
	assertRetainedBaseState()
	t.Logf("second diff-cycle timings: standby=%v restore-to-running=%v", secondStandbyDuration, secondRestoreRunningDuration)

	assertGuestFileContents(secondFilePath, secondFileContents)
	assertGuestFileContents(firstFilePath, firstFileContents)

	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))
	deleted = true
}

func TestFirecrackerStopClearsStaleSnapshot(t *testing.T) {
	t.Parallel()
	requireFirecrackerIntegrationPrereqs(t)

	mgr, tmpDir := setupTestManagerForFirecracker(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, imageManager)

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fc-stale-snapshot",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	require.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, 20*time.Second)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)

	// Establish a realistic standby/restore lifecycle first.
	inst, err = mgr.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	require.Equal(t, StateStandby, inst.State)
	require.True(t, inst.HasSnapshot)

	inst, err = mgr.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	require.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, 20*time.Second)
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
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, 20*time.Second)
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
	createNginxImageAndWait(t, ctx, imageManager)

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
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, 20*time.Second)
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

	inst, err = mgr.StandbyInstance(ctx, inst.Id)
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
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, 20*time.Second)
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

	mgr, tmpDir := setupTestManagerForFirecracker(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)
	createNginxImageAndWait(t, ctx, imageManager)

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
	source, err = waitForInstanceState(ctx, mgr, source.Id, StateRunning, 20*time.Second)
	require.NoError(t, err)
	sourceID := source.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), sourceID) })
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
	forked, err = waitForInstanceState(ctx, mgr, forked.Id, StateRunning, 20*time.Second)
	require.NoError(t, err)
	require.Equal(t, StateRunning, forked.State)
	forkID := forked.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), forkID) })
	assert.NotEmpty(t, forked.IP)
	assert.NotEmpty(t, forked.MAC)
	assert.Equal(t, mgr.paths.InstanceVsockSocket(forkID), forked.VsockSocket)

	forkMeta, err := mgr.loadMetadata(forkID)
	require.NoError(t, err)
	assert.Equal(t, mgr.paths.InstanceVsockSocket(forkID), forkMeta.StoredMetadata.VsockSocket)

	sourceAfterFork, err := mgr.GetInstance(ctx, sourceID)
	require.NoError(t, err)
	if sourceAfterFork.State != StateRunning {
		sourceAfterFork, err = waitForInstanceState(ctx, mgr, sourceID, StateRunning, 20*time.Second)
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
