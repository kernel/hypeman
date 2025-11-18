package instances

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/vmm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestManager creates a manager and registers cleanup for any orphaned processes
func setupTestManager(t *testing.T) (*manager, string) {
	tmpDir := t.TempDir()
	
	imageManager, err := images.NewManager(paths.New(tmpDir), 1)
	require.NoError(t, err)
	
	systemManager := system.NewManager(paths.New(tmpDir))
	maxOverlaySize := int64(100 * 1024 * 1024 * 1024)
	mgr := NewManager(paths.New(tmpDir), imageManager, systemManager, maxOverlaySize).(*manager)
	
	// Register cleanup to kill any orphaned Cloud Hypervisor processes
	t.Cleanup(func() {
		cleanupOrphanedProcesses(t, mgr)
	})
	
	return mgr, tmpDir
}

// waitForVMReady polls VM state via VMM API until it's running or times out
func waitForVMReady(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	
	for time.Now().Before(deadline) {
		// Try to connect to VMM
		client, err := vmm.NewVMM(socketPath)
		if err != nil {
			// Socket might not be ready yet
			time.Sleep(100 * time.Millisecond)
			continue
		}
		
		// Get VM info
		infoResp, err := client.GetVmInfoWithResponse(ctx)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		
		if infoResp.StatusCode() != 200 || infoResp.JSON200 == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		
		// Check if VM is running
		if infoResp.JSON200.State == vmm.Running {
			return nil
		}
		
		time.Sleep(100 * time.Millisecond)
	}
	
	return fmt.Errorf("VM did not reach running state within %v", timeout)
}

// cleanupOrphanedProcesses kills any Cloud Hypervisor processes from metadata
func cleanupOrphanedProcesses(t *testing.T, mgr *manager) {
	// Find all metadata files
	metaFiles, err := mgr.listMetadataFiles()
	if err != nil {
		return // No metadata files, nothing to clean
	}
	
	for _, metaFile := range metaFiles {
		// Extract instance ID from path
		id := filepath.Base(filepath.Dir(metaFile))
		
		// Load metadata
		meta, err := mgr.loadMetadata(id)
		if err != nil {
			continue
		}
		
		// If metadata has a PID, try to kill it
		if meta.CHPID != nil {
			pid := *meta.CHPID
			
			// Check if process exists
			if err := syscall.Kill(pid, 0); err == nil {
				t.Logf("Cleaning up orphaned Cloud Hypervisor process: PID %d (instance %s)", pid, id)
				syscall.Kill(pid, syscall.SIGKILL)
				
				// Wait for process to exit
				WaitForProcessExit(pid, 1*time.Second)
			}
		}
	}
}

func TestCreateAndDeleteInstance(t *testing.T) {
	// Require KVM access (don't skip, fail informatively)
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	manager, tmpDir := setupTestManager(t) // Automatically registers cleanup
	ctx := context.Background()

	// Get the image manager from the manager (we need it for image operations)
	imageManager, err := images.NewManager(paths.New(tmpDir), 1)
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
	systemManager := system.NewManager(paths.New(tmpDir))
	t.Log("Ensuring system files (downloads kernel ~70MB and builds initrd ~1MB)...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

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

	// Verify directories exist
	p := paths.New(tmpDir)
	assert.DirExists(t, p.InstanceDir(inst.Id))
	assert.FileExists(t, p.InstanceMetadata(inst.Id))
	assert.FileExists(t, p.InstanceOverlay(inst.Id))
	assert.FileExists(t, p.InstanceConfigDisk(inst.Id))

	// Wait for VM to be fully running
	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err, "VM should reach running state")

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

	// Poll for logs to contain nginx startup message
	var logs string
	foundNginxStartup := false
	for i := 0; i < 50; i++ { // Poll for up to 5 seconds (50 * 100ms)
		logs, err = manager.GetInstanceLogs(ctx, inst.Id, false, 100)
		require.NoError(t, err)
		
		if strings.Contains(logs, "start worker processes") {
			foundNginxStartup = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	
	t.Logf("Instance logs (last 100 lines):\n%s", logs)
	
	// Verify nginx started successfully
	assert.True(t, foundNginxStartup, "Nginx should have started worker processes within 5 seconds")

	// Delete instance
	t.Log("Deleting instance...")
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Verify cleanup
	assert.NoDirExists(t, p.InstanceDir(inst.Id))

	// Verify instance no longer exists
	_, err = manager.GetInstance(ctx, inst.Id)
	assert.ErrorIs(t, err, ErrNotFound)
	
	t.Log("Instance lifecycle test complete!")
}

func TestStorageOperations(t *testing.T) {
	// Test storage layer without starting VMs
	tmpDir := t.TempDir()

	imageManager, _ := images.NewManager(paths.New(tmpDir), 1)
	systemManager := system.NewManager(paths.New(tmpDir))
	maxOverlaySize := int64(100 * 1024 * 1024 * 1024) // 100GB
	manager := NewManager(paths.New(tmpDir), imageManager, systemManager, maxOverlaySize).(*manager)

	// Test metadata doesn't exist initially
	_, err := manager.loadMetadata("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)

	// Create instance metadata (stored fields only)
	stored := &StoredMetadata{
		Id:          "test-123",
		Name:        "test",
		Image:       "test:latest",
		Size:        1024 * 1024 * 1024,
		HotplugSize: 2048 * 1024 * 1024,
		OverlaySize: 10 * 1024 * 1024 * 1024,
		Vcpus:       2,
		Env:         map[string]string{"TEST": "value"},
		CreatedAt:   time.Now(),
		CHVersion:   vmm.V49_0,
		SocketPath:  "/tmp/test.sock",
		DataDir:     paths.New(tmpDir).InstanceDir("test-123"),
	}

	// Ensure directories
	err = manager.ensureDirectories(stored.Id)
	require.NoError(t, err)

	// Save metadata
	meta := &metadata{StoredMetadata: *stored}
	err = manager.saveMetadata(meta)
	require.NoError(t, err)

	// Load metadata
	loaded, err := manager.loadMetadata(stored.Id)
	require.NoError(t, err)
	assert.Equal(t, stored.Id, loaded.Id)
	assert.Equal(t, stored.Name, loaded.Name)
	// State is no longer stored, it's derived

	// List metadata files
	files, err := manager.listMetadataFiles()
	require.NoError(t, err)
	assert.Len(t, files, 1)

	// Delete instance data
	err = manager.deleteInstanceData(stored.Id)
	require.NoError(t, err)

	// Verify deletion
	_, err = manager.loadMetadata(stored.Id)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestStandbyAndRestore(t *testing.T) {
	// Require KVM access (don't skip, fail informatively)
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group (sudo usermod -aG kvm $USER)")
	}

	manager, tmpDir := setupTestManager(t) // Automatically registers cleanup
	ctx := context.Background()

	// Create image manager for pulling nginx
	imageManager, err := images.NewManager(paths.New(tmpDir), 1)
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
	systemManager := system.NewManager(paths.New(tmpDir))
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

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

	// Wait for VM to be fully running before standby
	err = waitForVMReady(ctx, inst.SocketPath, 5*time.Second)
	require.NoError(t, err, "VM should reach running state")

	// Standby instance
	t.Log("Standing by instance...")
	inst, err = manager.StandbyInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateStandby, inst.State)
	assert.True(t, inst.HasSnapshot)
	t.Log("Instance in standby")

	// Verify snapshot exists
	p := paths.New(tmpDir)
	snapshotDir := p.InstanceSnapshotLatest(inst.Id)
	assert.DirExists(t, snapshotDir)
	assert.FileExists(t, filepath.Join(snapshotDir, "memory-ranges"))
	// Cloud Hypervisor creates various snapshot files, just verify directory exists

	// Restore instance
	t.Log("Restoring instance...")
	inst, err = manager.RestoreInstance(ctx, inst.Id)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, inst.State)
	t.Log("Instance restored and running")

	// Cleanup (no sleep needed - DeleteInstance handles process cleanup)
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

