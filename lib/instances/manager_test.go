package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateAndDeleteInstance(t *testing.T) {
	// Require KVM access (don't skip, fail informatively)
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	tmpDir := t.TempDir()
	ctx := context.Background()

	// Create REAL image manager and pull nginx (stays running)
	imageManager, err := images.NewManager(tmpDir, 1)
	require.NoError(t, err)

	// Pull nginx image (runs a daemon, won't exit)
	t.Log("Pulling nginx:alpine image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	// Wait for image to be ready (poll by name)
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
	systemManager := system.NewManager(tmpDir)
	t.Log("Ensuring system files (downloads kernel ~70MB and builds initrd ~1MB)...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Create instances manager
	maxOverlaySize := int64(100 * 1024 * 1024 * 1024) // 100GB
	manager := NewManager(tmpDir, imageManager, systemManager, maxOverlaySize).(*manager)

	// Create instance with real nginx image (stays running)
	req := CreateInstanceRequest{
		Name:        "test-nginx",
		Image:       "docker.io/library/nginx:alpine",
		Size:        512 * 1024 * 1024,      // 512MB
		HotplugSize: 512 * 1024 * 1024,      // 512MB
		OverlaySize: 10 * 1024 * 1024 * 1024, // 10GB
		Vcpus:       1,
		Env: map[string]string{
			"TEST_VAR": "test_value",
		},
	}

	t.Log("Creating instance...")
	inst, err := manager.CreateInstance(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, inst)
	t.Logf("Instance created: %s", inst.Id)

	// Verify instance fields
	assert.NotEmpty(t, inst.Id)
	assert.Equal(t, "test-nginx", inst.Name)
	assert.Equal(t, "docker.io/library/nginx:alpine", inst.Image)
	assert.Equal(t, StateRunning, inst.State)
	assert.False(t, inst.HasSnapshot)
	assert.NotEmpty(t, inst.KernelVersion)
	assert.NotEmpty(t, inst.InitrdVersion)

	// Verify directories exist
	instDir := filepath.Join(tmpDir, "guests", inst.Id)
	assert.DirExists(t, instDir)
	assert.FileExists(t, filepath.Join(instDir, "metadata.json"))
	assert.FileExists(t, filepath.Join(instDir, "overlay.raw"))
	assert.FileExists(t, filepath.Join(instDir, "config.ext4"))

	// Give VM a moment to fully start
	time.Sleep(2 * time.Second)

	// Get instance
	retrieved, err := manager.GetInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, inst.Id, retrieved.Id)
	assert.Equal(t, StateRunning, retrieved.State)

	// List instances
	instances, err := manager.ListInstances(ctx)
	require.NoError(t, err)
	assert.Len(t, instances, 1)
	assert.Equal(t, inst.Id, instances[0].Id)

	// Get logs (should have boot output and nginx start)
	// Get more lines to see full nginx startup
	logs, err := manager.GetInstanceLogs(ctx, inst.Id, false, 100)
	require.NoError(t, err)
	t.Logf("Instance logs (last 100 lines):\n%s", logs)
	
	// Verify nginx started successfully
	assert.Contains(t, logs, "start worker processes", "Nginx should have started worker processes")

	// Delete instance
	t.Log("Deleting instance...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Verify cleanup
	assert.NoDirExists(t, instDir)

	// Verify instance no longer exists
	_, err = manager.GetInstance(ctx, inst.Id)
	assert.ErrorIs(t, err, ErrNotFound)
	
	t.Log("Instance lifecycle test complete!")
}

func TestStorageOperations(t *testing.T) {
	// Test storage layer without starting VMs
	tmpDir := t.TempDir()

	imageManager, _ := images.NewManager(tmpDir, 1)
	systemManager := system.NewManager(tmpDir)
	maxOverlaySize := int64(100 * 1024 * 1024 * 1024) // 100GB
	manager := NewManager(tmpDir, imageManager, systemManager, maxOverlaySize).(*manager)

	// Test metadata doesn't exist initially
	_, err := manager.loadMetadata("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)

	// Create instance metadata
	inst := &Instance{
		Id:          "test-123",
		Name:        "test",
		Image:       "test:latest",
		State:       StateStopped,
		Size:        1024 * 1024 * 1024,
		HotplugSize: 2048 * 1024 * 1024,
		OverlaySize: 10 * 1024 * 1024 * 1024,
		Vcpus:       2,
		Env:         map[string]string{"TEST": "value"},
		CreatedAt:   time.Now(),
		CHVersion:   vmm.V49_0,
		SocketPath:  "/tmp/test.sock",
		DataDir:     filepath.Join(tmpDir, "guests", "test-123"),
	}

	// Ensure directories
	err = manager.ensureDirectories(inst.Id)
	require.NoError(t, err)

	// Save metadata
	meta := &metadata{Instance: *inst}
	err = manager.saveMetadata(meta)
	require.NoError(t, err)

	// Load metadata
	loaded, err := manager.loadMetadata(inst.Id)
	require.NoError(t, err)
	assert.Equal(t, inst.Id, loaded.Id)
	assert.Equal(t, inst.Name, loaded.Name)
	assert.Equal(t, inst.State, loaded.State)

	// List metadata files
	files, err := manager.listMetadataFiles()
	require.NoError(t, err)
	assert.Len(t, files, 1)

	// Delete instance data
	err = manager.deleteInstanceData(inst.Id)
	require.NoError(t, err)

	// Verify deletion
	_, err = manager.loadMetadata(inst.Id)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestStandbyAndRestore(t *testing.T) {
	// Require KVM access (don't skip, fail informatively)
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	tmpDir := t.TempDir()
	ctx := context.Background()

	// Create REAL image manager and pull nginx
	imageManager, err := images.NewManager(tmpDir, 1)
	require.NoError(t, err)

	// Pull nginx image (reuse if already pulled in previous test)
	t.Log("Ensuring nginx:alpine image...")
	nginxImage, err := imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/nginx:alpine",
	})
	require.NoError(t, err)

	// Wait for image to be ready
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

	// Ensure system files
	systemManager := system.NewManager(tmpDir)
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	maxOverlaySize := int64(100 * 1024 * 1024 * 1024) // 100GB
	manager := NewManager(tmpDir, imageManager, systemManager, maxOverlaySize).(*manager)

	// Create instance
	t.Log("Creating instance...")
	req := CreateInstanceRequest{
		Name:        "test-standby",
		Image:       "docker.io/library/nginx:alpine",
		Size:        512 * 1024 * 1024,
		HotplugSize: 512 * 1024 * 1024,
		OverlaySize: 10 * 1024 * 1024 * 1024,
		Vcpus:       1,
		Env:         map[string]string{},
	}

	inst, err := manager.CreateInstance(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Logf("Instance created: %s", inst.Id)

	// Give VM a moment to boot
	time.Sleep(1 * time.Second)

	// Standby instance
	t.Log("Standing by instance...")
	inst, err = manager.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	t.Log("Instance in standby")

	// Verify snapshot exists
	snapshotDir := filepath.Join(tmpDir, "guests", inst.Id, "snapshots", "snapshot-latest")
	assert.DirExists(t, snapshotDir)
	assert.FileExists(t, filepath.Join(snapshotDir, "memory-ranges"))
	// Cloud Hypervisor creates various snapshot files, just verify directory exists

	// Restore instance
	t.Log("Restoring instance...")
	inst, err = manager.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Log("Instance restored and running")

	// Cleanup
	time.Sleep(500 * time.Millisecond)
	t.Log("Cleaning up...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)
	
	t.Log("Standby/restore test complete!")
}

func TestStateTransitions(t *testing.T) {
	tests := []struct {
		name       string
		from       State
		to         State
		shouldFail bool
	}{
		{"Stopped to Created", StateStopped, StateCreated, false},
		{"Created to Running", StateCreated, StateRunning, false},
		{"Running to Paused", StateRunning, StatePaused, false},
		{"Paused to Running", StatePaused, StateRunning, false},
		{"Paused to Standby", StatePaused, StateStandby, false},
		{"Standby to Paused", StateStandby, StatePaused, false},
		{"Shutdown to Stopped", StateShutdown, StateStopped, false},
		{"Standby to Stopped", StateStandby, StateStopped, false},
		// Invalid transitions
		{"Running to Standby", StateRunning, StateStandby, true},
		{"Stopped to Running", StateStopped, StateRunning, true},
		{"Standby to Running", StateStandby, StateRunning, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.from.CanTransitionTo(tt.to)
			if tt.shouldFail {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}


// No mock image manager needed - tests use real images!

