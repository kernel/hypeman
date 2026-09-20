package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQEMUStandbyAndRestore tests the standby/restore cycle with QEMU.
// This tests QEMU's migrate-to-file snapshot mechanism.
func TestQEMUStandbyAndRestore(t *testing.T) {
	t.Parallel()
	runQEMUStandbyAndRestore(t, hypervisor.TypeQEMU, "test-qemu-standby")
}

func TestQEMUMicroVMStandbyAndRestore(t *testing.T) {
	runQEMUStandbyAndRestore(t, hypervisor.TypeQEMUMicroVM, "test-qemu-microvm-standby")
}

func runQEMUStandbyAndRestore(t *testing.T, hypervisorType hypervisor.Type, instanceName string) {
	t.Helper()
	requireQEMUUsable(t)
	if hypervisorType == hypervisor.TypeQEMUMicroVM {
		requireMicroVMAvailable(t)
	}
	acquireHeavyIO(t)

	manager, tmpDir := setupTestManagerForQEMU(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Get the image manager for image operations
	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	// Pull nginx image
	t.Log("Pulling nginx:alpine image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
	})
	require.NoError(t, err)

	// Wait for image to be ready
	t.Log("Waiting for image build to complete...")
	imageName := nginxImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			nginxImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, nginxImage.Status, "Image should be ready after 60 seconds")
	t.Log("Nginx image ready")

	// Ensure system files
	systemManager := system.NewManager(p)
	t.Log("Ensuring system files...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Create instance with QEMU hypervisor (no network for simpler test).
	hotplugSize := int64(512 * 1024 * 1024) // Unused by standard QEMU.
	if hypervisorType == hypervisor.TypeQEMUMicroVM {
		hotplugSize = 0
	}
	req := CreateInstanceRequest{
		Name:           instanceName,
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           2 * 1024 * 1024 * 1024, // 2GB
		HotplugSize:    hotplugSize,
		OverlaySize:    10 * 1024 * 1024 * 1024, // 10GB
		Vcpus:          1,
		NetworkEnabled: false, // No network for simpler test
		Hypervisor:     hypervisorType,
		Env:            map[string]string{},
	}

	t.Log("Creating QEMU instance...")
	inst, err := manager.CreateInstance(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, manager, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	assert.Equal(t, hypervisorType, inst.HypervisorType)
	t.Logf("Instance created: %s (hypervisor: %s)", inst.Id, inst.HypervisorType)

	// Wait for VM to be fully running before standby
	err = waitForQEMUReady(ctx, inst.SocketPath, hypervisorType, integrationTestTimeout(30*time.Second))
	require.NoError(t, err, "QEMU VM should reach running state")
	assert.Equal(t, phasetracking.PhaseRunning, inst.Phases.Current, "fresh instance should be in running phase")

	// Standby instance
	t.Log("Standing by instance...")
	inst, err = manager.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	assert.Equal(t, phasetracking.PhaseStandby, inst.Phases.Current, "standby transition should set current phase")
	assert.Greater(t, inst.Phases.Cumulative[phasetracking.PhaseRunning], int64(0), "running stint should be accrued after standby")
	t.Log("Instance in standby")

	// Verify snapshot exists
	snapshotDir := p.InstanceSnapshotLatest(inst.Id)
	assert.DirExists(t, snapshotDir)
	assert.FileExists(t, filepath.Join(snapshotDir, "memory"), "QEMU snapshot memory file should exist")
	assert.FileExists(t, filepath.Join(snapshotDir, "qemu-config.json"), "QEMU config should be saved in snapshot")
	configMachineType, err := qemuConfigMachineType(snapshotDir)
	require.NoError(t, err)
	assert.Equal(t, string(expectedQEMUMachineType(t, hypervisorType)), configMachineType)

	// Log snapshot files
	t.Log("Snapshot files:")
	entries, _ := os.ReadDir(snapshotDir)
	for _, entry := range entries {
		info, _ := entry.Info()
		t.Logf("  - %s (size: %d bytes)", entry.Name(), info.Size())
	}

	// Restore instance
	t.Log("Restoring instance...")
	inst, err = manager.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Contains(t, []State{StateInitializing, StateRunning}, inst.State)
	inst, err = waitForInstanceState(ctx, manager, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	t.Log("Instance restored and running")

	// Wait for VM to be running again
	err = waitForQEMUReady(ctx, inst.SocketPath, hypervisorType, integrationTestTimeout(30*time.Second))
	require.NoError(t, err, "QEMU VM should reach running state after restore")
	assert.Equal(t, phasetracking.PhaseRunning, inst.Phases.Current, "restored instance should be in running phase")
	assert.Greater(t, inst.Phases.Cumulative[phasetracking.PhaseStandby], int64(0), "standby stint should be accrued after restore")
	assert.Equal(t, hypervisorType, inst.HypervisorType)
	configMachineType, err = qemuConfigMachineType(p.InstanceDir(inst.Id))
	require.NoError(t, err)
	assert.Equal(t, string(expectedQEMUMachineType(t, hypervisorType)), configMachineType)

	// Cleanup
	t.Log("Cleaning up...")
	deleteInstanceEventually(t, ctx, manager, inst.Id)

	// Verify cleanup
	assert.NoDirExists(t, p.InstanceDir(inst.Id))

	t.Log("QEMU standby/restore test complete!")
}

func TestQEMUForkFromRunningNetwork(t *testing.T) {
	t.Parallel()
	runQEMUForkFromRunningNetwork(t, hypervisor.TypeQEMU, "qemu")
}

func TestQEMUMicroVMForkFromRunningNetwork(t *testing.T) {
	runQEMUForkFromRunningNetwork(t, hypervisor.TypeQEMUMicroVM, "qemu-microvm")
}

func runQEMUForkFromRunningNetwork(t *testing.T, hypervisorType hypervisor.Type, namePrefix string) {
	t.Helper()
	requireQEMUUsable(t)
	if hypervisorType == hypervisor.TypeQEMUMicroVM {
		requireMicroVMAvailable(t)
	}
	acquireHeavyIO(t)

	manager, tmpDir := setupTestManagerForQEMU(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	imageManager, err := images.NewManager(p, 1, nil)
	require.NoError(t, err)

	t.Log("Ensuring nginx image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: integrationTestImageRef(t, "docker.io/library/nginx:alpine")})
	require.NoError(t, err)

	imageName := nginxImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			nginxImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, nginxImage.Status, "Image should be ready after 60 seconds")

	require.NoError(t, manager.systemManager.EnsureSystemFiles(ctx))
	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	hotplugSize := int64(256 * 1024 * 1024)
	if hypervisorType == hypervisor.TypeQEMUMicroVM {
		hotplugSize = 0
	}
	source, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           namePrefix + "-fork-running-src",
		Image:          integrationTestImageRef(t, "docker.io/library/nginx:alpine"),
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    hotplugSize,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Hypervisor:     hypervisorType,
	})
	require.NoError(t, err)
	sourceID := source.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, sourceID) })
	// QEMU is the slowest Linux hypervisor under full-suite load on Deft. Give
	// the host-side Running transition more headroom so we don't fail while the
	// VM is still legitimately completing boot marker hydration.
	source, err = waitForInstanceState(ctx, manager, sourceID, StateRunning, 45*time.Second)
	require.NoError(t, err)
	require.NoError(t, waitForQEMUReady(ctx, source.SocketPath, hypervisorType, integrationTestTimeout(30*time.Second)))

	assert.NotEmpty(t, source.IP)
	assert.NotEmpty(t, source.MAC)
	assertHostCanReachNginx(t, source.IP, 80, 60*time.Second)
	assert.Equal(t, hypervisorType, source.HypervisorType)

	_, err = manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{Name: namePrefix + "-fork-running-no-flag"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)

	forked, err := manager.ForkInstance(ctx, source.Id, ForkInstanceRequest{
		Name:        namePrefix + "-fork-running-copy",
		FromRunning: true,
		TargetState: StateStandby,
	})
	require.NoError(t, err)
	require.Equal(t, StateStandby, forked.State)
	assert.Equal(t, hypervisorType, forked.HypervisorType)
	forkedID := forked.Id
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), manager, forkedID) })

	sourceAfterFork, err := manager.GetInstance(ctx, source.Id)
	require.NoError(t, err)
	if sourceAfterFork.State != StateRunning {
		sourceAfterFork, err = waitForInstanceState(ctx, manager, source.Id, StateRunning, 45*time.Second)
		require.NoError(t, err)
	}
	require.Equal(t, StateRunning, sourceAfterFork.State)
	assert.Equal(t, hypervisorType, sourceAfterFork.HypervisorType)
	require.NotEmpty(t, sourceAfterFork.IP)
	assertHostCanReachNginx(t, sourceAfterFork.IP, 80, 60*time.Second)

	forked, err = manager.RestoreInstance(ctx, forkedID)
	require.NoError(t, err)
	require.Contains(t, []State{StateInitializing, StateRunning}, forked.State)
	forked, err = waitForInstanceState(ctx, manager, forkedID, StateRunning, 45*time.Second)
	require.NoError(t, err)
	require.Equal(t, StateRunning, forked.State)
	assert.Equal(t, hypervisorType, forked.HypervisorType)
	require.NoError(t, waitForQEMUReady(ctx, forked.SocketPath, hypervisorType, integrationTestTimeout(30*time.Second)))

	assert.NotEmpty(t, forked.IP)
	assert.NotEmpty(t, forked.MAC)
	assert.NotEqual(t, sourceAfterFork.IP, forked.IP)
	assert.NotEqual(t, sourceAfterFork.MAC, forked.MAC)
	assertHostCanReachNginx(t, forked.IP, 80, 60*time.Second)
}
