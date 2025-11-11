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
	// TODO: Full integration test requires real kernel/initramfs files
	// Run ./scripts/build-initrd.sh to create them, then update this test
	// to use real files from data/system/ directory
	
	t.Skip("Skipping: requires real kernel/initramfs files (run ./scripts/build-initrd.sh)")
}

func TestStorageOperations(t *testing.T) {
	// Test storage layer without starting VMs
	tmpDir := t.TempDir()

	imageManager := setupTestImageManager(t, tmpDir)
	systemManager := system.NewManager(tmpDir)
	manager := NewManager(tmpDir, imageManager, systemManager).(*manager)

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
	// TODO: Full integration test requires real kernel/initramfs files
	// Run ./scripts/build-initrd.sh to create them, then update this test
	
	t.Skip("Skipping: requires real kernel/initramfs files (run ./scripts/build-initrd.sh)")
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


// setupTestImageManager creates a mock image manager with a test image
func setupTestImageManager(t *testing.T, dataDir string) images.Manager {
	// Create test image directory
	imageDir := filepath.Join(dataDir, "images", "test-image:latest", "abc123def456")
	require.NoError(t, os.MkdirAll(imageDir, 0755))

	// Create dummy rootfs
	rootfsPath := filepath.Join(imageDir, "rootfs.erofs")
	require.NoError(t, os.WriteFile(rootfsPath, []byte{}, 0644))

	// Create metadata
	metadataPath := filepath.Join(imageDir, "metadata.json")
	metadataJSON := `{
		"name": "test-image:latest",
		"digest": "abc123def456789",
		"status": "ready",
		"entrypoint": ["/bin/sh"],
		"cmd": [],
		"env": {"PATH": "/usr/bin:/bin"},
		"working_dir": "/",
		"created_at": "2025-01-01T00:00:00Z"
	}`
	require.NoError(t, os.WriteFile(metadataPath, []byte(metadataJSON), 0644))

	// Return a simple mock that returns the test image
	return &mockImageManager{dataDir: dataDir}
}

// mockImageManager is a simple mock for testing
type mockImageManager struct {
	dataDir string
}

func (m *mockImageManager) ListImages(ctx context.Context) ([]images.Image, error) {
	return []images.Image{}, nil
}

func (m *mockImageManager) CreateImage(ctx context.Context, req images.CreateImageRequest) (*images.Image, error) {
	return nil, nil
}

func (m *mockImageManager) GetImage(ctx context.Context, name string) (*images.Image, error) {
	// Return test image
	return &images.Image{
		Name:       "test-image:latest",
		Digest:     "abc123def456789",
		Status:     images.StatusReady,
		Entrypoint: []string{"/bin/sh"},
		Cmd:        []string{},
		Env: map[string]string{
			"PATH": "/usr/bin:/bin",
		},
		WorkingDir: "/",
		CreatedAt:  time.Now(),
	}, nil
}

func (m *mockImageManager) DeleteImage(ctx context.Context, name string) error {
	return nil
}

func (m *mockImageManager) RecoverInterruptedBuilds() {}

