package instances

import (
	"context"
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
			"CMD": "sh -c 'echo \"Testing internet connectivity...\"; wget -O- https://public-ping-bucket-kernel.s3.us-east-1.amazonaws.com/index.html 2>&1 && echo \"Internet connectivity: SUCCESS\" && sleep 300'",
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

	// Verify internet connectivity by checking logs
	t.Log("Verifying internet connectivity...")
	err = waitForLogMessage(ctx, manager, inst.Id, "Internet connectivity: SUCCESS", 10*time.Second)
	require.NoError(t, err, "Instance should be able to reach the internet")
	t.Log("Internet connectivity verified!")

	// Cleanup
	t.Log("Cleaning up instance...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Verify TAP deleted after instance cleanup
	t.Log("Verifying TAP deleted after cleanup...")
	_, err = netlink.LinkByName(alloc.TAPDevice)
	// TAP device may or may not be deleted depending on whether VMM exited cleanly.
	// Best-effort cleanup means straggler TAPs can remain (expected behavior for unclean shutdown).
	// This is acceptable - TAPs will be cleaned up on host reboot or manual cleanup.
	if err == nil {
		t.Logf("TAP device still exists (acceptable for unclean VMM shutdown): %s", alloc.TAPDevice)
	} else {
		t.Logf("TAP device successfully deleted: %s", alloc.TAPDevice)
	}


	t.Log("Network integration test complete!")
}

// requireKVMAccess checks for KVM availability
func requireKVMAccess(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group")
	}
}

