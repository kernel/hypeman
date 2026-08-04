//go:build linux

package instances

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/cloudhypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCloudHypervisorVersionUpgradeRestore verifies that instances created on
// one CH version can be put in standby and restored after the default version
// changes. This tests the backwards-compatible upgrade path: deploy new binary
// with both versions embedded, flip the flag, and existing standbys still
// restore using their original version.
func TestCloudHypervisorVersionUpgradeRestore(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available, skipping on this platform")
	}
	acquireHeavyIO(t)

	mgr, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Prepare image and system files
	imageManager, err := images.NewManager(p, 1, nil, nil)
	require.NoError(t, err)

	t.Log("Ensuring alpine image...")
	alpineImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: integrationTestImageRef(t, "docker.io/library/alpine:latest"),
	})
	require.NoError(t, err)
	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, alpineImage.Name)
		if err == nil && img.Status == images.StatusReady {
			alpineImage = img
			break
		}
		if err == nil && img.Status == images.StatusFailed {
			t.Fatalf("Image build failed: %s", *img.Error)
		}
		time.Sleep(1 * time.Second)
	}
	require.Equal(t, images.StatusReady, alpineImage.Status, "Image should be ready after 60 seconds")

	systemManager := system.NewManager(p)
	require.NoError(t, systemManager.EnsureSystemFiles(ctx))

	// Reset default version override after this test
	t.Cleanup(func() {
		_ = cloudhypervisor.SetDefaultVersion("")
	})

	// Phase 1: Create instance with explicit v49.0 version
	t.Log("Creating instance with CH v49.0...")
	inst, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:              "version-upgrade-test",
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		Size:              1024 * 1024 * 1024,
		OverlaySize:       10 * 1024 * 1024 * 1024,
		Vcpus:             1,
		NetworkEnabled:    false,
		Hypervisor:        hypervisor.TypeCloudHypervisor,
		HypervisorVersion: string(vmm.V49_0),
		Cmd:               []string{"sleep", "infinity"},
	})
	require.NoError(t, err)
	assert.Equal(t, string(vmm.V49_0), inst.HypervisorVersion)
	t.Logf("Instance created: %s (version: %s)", inst.Id, inst.HypervisorVersion)

	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst.State)

	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err)

	// Phase 2: Put instance in standby
	t.Log("Putting instance in standby...")
	inst, err = mgr.StandbyInstance(ctx, inst.Id, StandbyInstanceRequest{})
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	assert.Equal(t, string(vmm.V49_0), inst.HypervisorVersion)

	// Phase 3: Change default version to v51.1 (simulates a deploy with new config)
	t.Log("Switching default CH version to v51.1...")
	require.NoError(t, cloudhypervisor.SetDefaultVersion(string(vmm.V51_1)))

	// Phase 4: Restore -- must use the stored v49.0, not the new default
	t.Log("Restoring instance (should use stored v49.0)...")
	inst, err = mgr.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	inst, err = waitForInstanceState(ctx, mgr, inst.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	assert.Equal(t, string(vmm.V49_0), inst.HypervisorVersion,
		"Restored instance must use original version, not the new default")
	t.Logf("Instance restored with version: %s", inst.HypervisorVersion)

	// Phase 5: Verify a NEW instance picks up the v51.1 default
	t.Log("Creating second instance (should use v51.1 via default)...")
	inst2, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "version-upgrade-new",
		Image:          integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		Size:           1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     hypervisor.TypeCloudHypervisor,
		Cmd:            []string{"sleep", "infinity"},
	})
	require.NoError(t, err)
	assert.Equal(t, string(vmm.V51_1), inst2.HypervisorVersion,
		"New instance should use the updated default version")
	t.Logf("New instance created: %s (version: %s)", inst2.Id, inst2.HypervisorVersion)

	inst2, err = waitForInstanceState(ctx, mgr, inst2.Id, StateRunning, integrationTestTimeout(20*time.Second))
	require.NoError(t, err)
	require.Equal(t, StateRunning, inst2.State)

	// Cleanup
	t.Log("Cleaning up...")
	require.NoError(t, mgr.DeleteInstance(ctx, inst.Id))
	require.NoError(t, mgr.DeleteInstance(ctx, inst2.Id))

	t.Log("Version upgrade restore test complete!")
}
