//go:build linux

package instances

import (
	"context"
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

func setupTestManagerForFirecracker(t *testing.T) (*manager, string) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		DataDir: tmpDir,
		Network: config.NetworkConfig{
			BridgeName: "vmbr0",
			SubnetCIDR: "10.100.0.0/16",
			DNSServer:  "1.1.1.1",
		},
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

func requireFirecrackerIntegrationPrereqs(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping Firecracker integration test")
	}
}

func createNginxImageAndWait(t *testing.T, ctx context.Context, imageManager images.Manager) {
	t.Helper()

	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	for i := 0; i < 180; i++ {
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

func TestFirecrackerStandbyAndRestore(t *testing.T) {
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
		Name:           "test-firecracker-standby",
		Image:          "docker.io/library/nginx:alpine",
		Size:           1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)

	inst, err = mgr.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)

	inst, err = mgr.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)

	inst, err = mgr.StopInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStopped, inst.State)
	assert.False(t, inst.HasSnapshot, "stopped instances should not retain standby snapshots")

	// Verify stopped -> start works after standby/restore lifecycle.
	inst, err = mgr.StartInstance(ctx, inst.Id, StartInstanceRequest{})
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)

	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))
}

func TestFirecrackerStopClearsStaleSnapshot(t *testing.T) {
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
		Image:          "docker.io/library/nginx:alpine",
		Size:           1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)

	// Establish a realistic standby/restore lifecycle first.
	inst, err = mgr.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	require.Equal(t, StateStandby, inst.State)
	require.True(t, inst.HasSnapshot)

	inst, err = mgr.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)

	// Simulate stale snapshot residue from a prior failure/interruption.
	snapshotDir := p.InstanceSnapshotLatest(inst.Id)
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "stale-marker"), []byte("stale"), 0644))

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

	inst, err = mgr.StartInstance(ctx, inst.Id, StartInstanceRequest{})
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)

	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))
}

func TestFirecrackerNetworkLifecycle(t *testing.T) {
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
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    5 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
	require.NoError(t, err)
	require.NotNil(t, inst)

	alloc, err := mgr.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, alloc)
	assert.NotEmpty(t, alloc.IP)
	assert.NotEmpty(t, alloc.MAC)
	assert.NotEmpty(t, alloc.TAPDevice)

	tap, err := netlink.LinkByName(alloc.TAPDevice)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(tap.Attrs().Name, "hype-"))
	assert.Equal(t, uint8(netlink.OperUp), uint8(tap.Attrs().OperState))

	bridge, err := netlink.LinkByName("vmbr0")
	require.NoError(t, err)
	assert.Equal(t, bridge.Attrs().Index, tap.Attrs().MasterIndex)

	require.NoError(t, waitForLogMessage(ctx, mgr, inst.Id, "start worker processes", 15*time.Second))
	require.NoError(t, waitForLogMessage(ctx, mgr, inst.Id, "[guest-agent] listening", 10*time.Second))

	// Retry to reduce flakiness while guest network stack settles.
	var output string
	var exitCode int
	for i := 0; i < 10; i++ {
		output, exitCode, err = execCommand(ctx, inst, "curl", "-s", "--connect-timeout", "10", "https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html")
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
	assert.Equal(t, StateRunning, inst.State)

	allocRestored, err := mgr.networkManager.GetAllocation(ctx, inst.Id)
	require.NoError(t, err)
	require.NotNil(t, allocRestored)
	assert.Equal(t, alloc.IP, allocRestored.IP)
	assert.Equal(t, alloc.MAC, allocRestored.MAC)
	assert.Equal(t, alloc.TAPDevice, allocRestored.TAPDevice)

	tapRestored, err := netlink.LinkByName(allocRestored.TAPDevice)
	require.NoError(t, err)
	assert.Equal(t, uint8(netlink.OperUp), uint8(tapRestored.Attrs().OperState))

	for i := 0; i < 10; i++ {
		output, exitCode, err = execCommand(ctx, inst, "curl", "-s", "https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html")
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
		Image:          "docker.io/library/nginx:alpine",
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    256 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisor.TypeFirecracker,
	})
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
	require.Equal(t, StateRunning, sourceAfterFork.State)
	assert.NotEmpty(t, sourceAfterFork.IP)
	assert.NotEmpty(t, sourceAfterFork.MAC)

	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)
	assertHostCanReachNginx(t, forked.IP, 80, 60*time.Second)
	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)
	assert.NotEqual(t, sourceAfterFork.IP, forked.IP)
	assert.NotEqual(t, sourceAfterFork.MAC, forked.MAC)
}
