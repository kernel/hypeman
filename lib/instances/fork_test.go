package instances

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkInstanceNotSupportedHypervisor(t *testing.T) {
	manager, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-qemu-source"
	require.NoError(t, manager.ensureDirectories(sourceID))

	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                sourceID,
		Name:              "fork-qemu-source",
		Image:             "docker.io/library/alpine:latest",
		CreatedAt:         time.Now(),
		HypervisorType:    hypervisor.TypeQEMU,
		HypervisorVersion: "test",
		SocketPath:        paths.New(manager.paths.DataDir()).InstanceSocket(sourceID, "qemu.sock"),
		DataDir:           paths.New(manager.paths.DataDir()).InstanceDir(sourceID),
		VsockCID:          42,
		VsockSocket:       paths.New(manager.paths.DataDir()).InstanceVsockSocket(sourceID),
	}}
	require.NoError(t, manager.saveMetadata(meta))

	_, err := manager.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-qemu-copy"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotSupported)
}

func TestForkCloudHypervisorStoppedAndStandby(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()

	imageManager, err := images.NewManager(paths.New(tmpDir), 1, nil)
	require.NoError(t, err)

	t.Log("Ensuring alpine image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{Name: "docker.io/library/alpine:latest"})
	require.NoError(t, err)

	imageName := alpineImage.Name
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, imageName)
		if err == nil && img.Status == images.StatusReady {
			alpineImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, alpineImage.Status, "Image should be ready after 60 seconds")

	systemManager := manager.systemManager
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	require.NoError(t, manager.networkManager.Initialize(ctx, nil))

	createReq := CreateInstanceRequest{
		Image:          "docker.io/library/alpine:latest",
		Size:           2 * 1024 * 1024 * 1024,
		HotplugSize:    256 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: true,
		Entrypoint:     []string{"sh", "-c"},
		Cmd:            []string{"while true; do sleep 3600; done"},
	}

	// Stopped source fork flow.
	sourceStopped, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fork-stop-src",
		Image:          createReq.Image,
		Size:           createReq.Size,
		HotplugSize:    createReq.HotplugSize,
		OverlaySize:    createReq.OverlaySize,
		Vcpus:          createReq.Vcpus,
		NetworkEnabled: createReq.NetworkEnabled,
		Entrypoint:     createReq.Entrypoint,
		Cmd:            createReq.Cmd,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), sourceStopped.Id) })
	require.NoError(t, waitForVMReady(ctx, sourceStopped.SocketPath, 5*time.Second))

	sourceStopped, err = manager.StopInstance(ctx, sourceStopped.Id)
	require.NoError(t, err)
	require.Equal(t, StateStopped, sourceStopped.State)

	forkStopped, err := manager.ForkInstance(ctx, sourceStopped.Id, ForkInstanceRequest{Name: "fork-stop-copy"})
	require.NoError(t, err)
	require.Equal(t, StateStopped, forkStopped.State)
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), forkStopped.Id) })

	sourceStopped, err = manager.StartInstance(ctx, sourceStopped.Id, StartInstanceRequest{})
	require.NoError(t, err)
	forkStopped, err = manager.StartInstance(ctx, forkStopped.Id, StartInstanceRequest{})
	require.NoError(t, err)
	require.NoError(t, waitForVMReady(ctx, sourceStopped.SocketPath, 5*time.Second))
	require.NoError(t, waitForVMReady(ctx, forkStopped.SocketPath, 5*time.Second))

	assert.NotEmpty(t, sourceStopped.IP)
	assert.NotEmpty(t, forkStopped.IP)
	assert.NotEqual(t, sourceStopped.IP, forkStopped.IP)
	assert.NotEqual(t, sourceStopped.MAC, forkStopped.MAC)

	// Standby source fork flow.
	sourceStandby, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "fork-standby-src",
		Image:          createReq.Image,
		Size:           createReq.Size,
		HotplugSize:    createReq.HotplugSize,
		OverlaySize:    createReq.OverlaySize,
		Vcpus:          createReq.Vcpus,
		NetworkEnabled: createReq.NetworkEnabled,
		Entrypoint:     createReq.Entrypoint,
		Cmd:            createReq.Cmd,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), sourceStandby.Id) })
	require.NoError(t, waitForVMReady(ctx, sourceStandby.SocketPath, 5*time.Second))

	sourceStandby, err = manager.StandbyInstance(ctx, sourceStandby.Id)
	require.NoError(t, err)
	require.Equal(t, StateStandby, sourceStandby.State)

	forkStandby, err := manager.ForkInstance(ctx, sourceStandby.Id, ForkInstanceRequest{Name: "fork-standby-copy"})
	require.NoError(t, err)
	require.Equal(t, StateStandby, forkStandby.State)
	t.Cleanup(func() { _ = manager.DeleteInstance(context.Background(), forkStandby.Id) })

	sourceStandby, err = manager.RestoreInstance(ctx, sourceStandby.Id)
	require.NoError(t, err)
	forkStandby, err = manager.RestoreInstance(ctx, forkStandby.Id)
	require.NoError(t, err)
	require.NoError(t, waitForVMReady(ctx, sourceStandby.SocketPath, 5*time.Second))
	require.NoError(t, waitForVMReady(ctx, forkStandby.SocketPath, 5*time.Second))

	assert.NotEmpty(t, sourceStandby.IP)
	assert.NotEmpty(t, forkStandby.IP)
	assert.NotEqual(t, sourceStandby.IP, forkStandby.IP)
	assert.NotEqual(t, sourceStandby.MAC, forkStandby.MAC)
}
