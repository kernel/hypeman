package instances

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitForExecAgent polls until exec-agent is ready
func waitForExecAgent(ctx context.Context, mgr *manager, instanceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		logs, err := collectLogs(ctx, mgr, instanceID, 100)
		if err == nil && strings.Contains(logs, "[exec-agent] listening on vsock port 2222") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

// execWithRetry runs a command with retries until exec-agent is ready
func execWithRetry(ctx context.Context, vsockSocket string, command []string) (string, int, error) {
	var output string
	var code int
	var err error

	for i := 0; i < 10; i++ {
		output, code, err = execCommand(ctx, vsockSocket, command...)
		if err == nil {
			return output, code, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return output, code, err
}

// TestVolumeMultiAttachReadOnly tests that a volume can be:
// 1. Attached read-write to one instance, written to
// 2. Detached (by deleting the instance)
// 3. Attached read-only to multiple instances simultaneously
// 4. Data persists and is readable from all instances
func TestVolumeMultiAttachReadOnly(t *testing.T) {
	// Require KVM
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group")
	}

	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Setup: prepare image and system files
	imageManager, err := images.NewManager(p, 1)
	require.NoError(t, err)

	t.Log("Pulling alpine image...")
	_, err = imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, "docker.io/library/alpine:latest")
		if err == nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(1 * time.Second)
	}
	t.Log("Image ready")

	systemManager := system.NewManager(p)
	t.Log("Ensuring system files...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Create volume
	volumeManager := volumes.NewManager(p, 0)
	t.Log("Creating volume...")
	vol, err := volumeManager.CreateVolume(ctx, volumes.CreateVolumeRequest{
		Name:   "shared-data",
		SizeGb: 1,
	})
	require.NoError(t, err)
	t.Logf("Volume created: %s", vol.Id)

	// Phase 1: Create instance with volume attached read-write
	t.Log("Phase 1: Creating writer instance with read-write volume...")
	writerInst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "writer",
		Image:          "docker.io/library/alpine:latest",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Volumes: []VolumeAttachment{
			{VolumeID: vol.Id, MountPath: "/data", Readonly: false},
		},
	})
	require.NoError(t, err)
	t.Logf("Writer instance created: %s", writerInst.Id)

	// Wait for exec-agent
	err = waitForExecAgent(ctx, manager, writerInst.Id, 15*time.Second)
	require.NoError(t, err, "exec-agent should be ready")

	// Write test file, sync, and verify in one command to ensure data persistence
	t.Log("Writing test file to volume...")
	output, code, err := execWithRetry(ctx, writerInst.VsockSocket, []string{
		"/bin/sh", "-c", "echo 'Hello from writer' > /data/test.txt && sync && cat /data/test.txt",
	})
	require.NoError(t, err)
	require.Equal(t, 0, code, "Write+verify command should succeed")
	require.Contains(t, output, "Hello from writer", "File should contain test data")
	t.Log("Test file written successfully")

	// Delete writer instance (detaches volume)
	t.Log("Deleting writer instance...")
	err = manager.DeleteInstance(ctx, writerInst.Id)
	require.NoError(t, err)

	// Verify volume is detached
	vol, err = volumeManager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.Empty(t, vol.Attachments, "Volume should be detached")

	// Phase 2: Create two instances with the volume attached read-only
	t.Log("Phase 2: Creating two reader instances with read-only volume...")

	reader1, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "reader-1",
		Image:          "docker.io/library/alpine:latest",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Volumes: []VolumeAttachment{
			{VolumeID: vol.Id, MountPath: "/data", Readonly: true},
		},
	})
	require.NoError(t, err)
	t.Logf("Reader 1 created: %s", reader1.Id)

	// Reader 2 uses overlay mode: can read base data AND write to its own overlay
	reader2, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "reader-2-overlay",
		Image:          "docker.io/library/alpine:latest",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Volumes: []VolumeAttachment{
			{VolumeID: vol.Id, MountPath: "/data", Readonly: true, Overlay: true, OverlaySize: 100 * 1024 * 1024}, // 100MB overlay
		},
	})
	require.NoError(t, err)
	t.Logf("Reader 2 (overlay) created: %s", reader2.Id)

	// Verify volume has two attachments
	vol, err = volumeManager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.Len(t, vol.Attachments, 2, "Volume should have 2 attachments")

	// Wait for exec-agent on both readers
	err = waitForExecAgent(ctx, manager, reader1.Id, 15*time.Second)
	require.NoError(t, err, "reader-1 exec-agent should be ready")

	err = waitForExecAgent(ctx, manager, reader2.Id, 15*time.Second)
	require.NoError(t, err, "reader-2 exec-agent should be ready")

	// Verify data is readable from reader-1
	t.Log("Verifying data from reader-1...")
	output1, code, err := execWithRetry(ctx, reader1.VsockSocket, []string{"cat", "/data/test.txt"})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Contains(t, output1, "Hello from writer", "Reader 1 should see the file")

	// Verify data is readable from reader-2 (overlay mode)
	t.Log("Verifying data from reader-2 (overlay)...")
	output2, code, err := execWithRetry(ctx, reader2.VsockSocket, []string{"cat", "/data/test.txt"})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	assert.Contains(t, output2, "Hello from writer", "Reader 2 should see the file from base volume")

	// Verify overlay allows writes: append to the file and verify in one command
	t.Log("Verifying overlay allows writes (append to file)...")
	output2, code, err = execWithRetry(ctx, reader2.VsockSocket, []string{
		"/bin/sh", "-c", "echo 'Appended by overlay' >> /data/test.txt && sync && cat /data/test.txt",
	})
	require.NoError(t, err)
	require.Equal(t, 0, code, "Append to file should succeed with overlay")
	assert.Contains(t, output2, "Hello from writer", "Reader 2 should still see original data")
	assert.Contains(t, output2, "Appended by overlay", "Reader 2 should see appended data")

	// Verify reader-1 does NOT see the appended data AND write fails (all in one command)
	t.Log("Verifying read-only enforcement and isolation on reader-1...")
	output1, code, err = execWithRetry(ctx, reader1.VsockSocket, []string{
		"/bin/sh", "-c", "cat /data/test.txt && echo 'illegal' > /data/illegal.txt",
	})
	require.NoError(t, err, "Exec should succeed even if write command fails")
	// Code should be non-zero because the write fails
	assert.NotEqual(t, 0, code, "Write to read-only volume should fail with non-zero exit")
	assert.Contains(t, output1, "Hello from writer", "Reader 1 should see original data")
	assert.NotContains(t, output1, "Appended by overlay", "Reader 1 should NOT see overlay data (isolated)")

	t.Log("Multi-attach with overlay test passed!")

	// Cleanup
	t.Log("Cleaning up...")
	manager.DeleteInstance(ctx, reader1.Id)
	manager.DeleteInstance(ctx, reader2.Id)
	volumeManager.DeleteVolume(ctx, vol.Id)
}

// TestOverlayDiskCleanupOnDelete verifies that vol-overlays/ directory is removed
// when an instance with overlay volumes is deleted.
func TestOverlayDiskCleanupOnDelete(t *testing.T) {
	// Skip in short mode - this is an integration test
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Require KVM access
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Skip("/dev/kvm not available - skipping VM test")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Setup: prepare image and system files
	imageManager, err := images.NewManager(p, 1)
	require.NoError(t, err)

	t.Log("Pulling alpine image...")
	_, err = imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, "docker.io/library/alpine:latest")
		if err == nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(1 * time.Second)
	}

	systemManager := system.NewManager(p)
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Create volume
	volumeManager := volumes.NewManager(p, 0)
	vol, err := volumeManager.CreateVolume(ctx, volumes.CreateVolumeRequest{
		Name:   "cleanup-test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// Create instance with overlay volume
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "overlay-cleanup-test",
		Image:          "docker.io/library/alpine:latest",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Volumes: []VolumeAttachment{
			{VolumeID: vol.Id, MountPath: "/data", Readonly: true, Overlay: true, OverlaySize: 100 * 1024 * 1024},
		},
	})
	require.NoError(t, err)

	// Verify vol-overlays directory exists
	overlaysDir := p.InstanceVolumeOverlaysDir(inst.Id)
	_, err = os.Stat(overlaysDir)
	require.NoError(t, err, "vol-overlays directory should exist after instance creation")

	// Verify overlay disk file exists
	overlayDisk := p.InstanceVolumeOverlay(inst.Id, vol.Id)
	_, err = os.Stat(overlayDisk)
	require.NoError(t, err, "overlay disk file should exist after instance creation")

	// Delete the instance
	err = manager.DeleteInstance(ctx, inst.Id)
	require.NoError(t, err)

	// Verify instance directory is removed (which includes vol-overlays/)
	instanceDir := p.InstanceDir(inst.Id)
	_, err = os.Stat(instanceDir)
	assert.True(t, os.IsNotExist(err), "instance directory should be removed after deletion")

	// Cleanup
	volumeManager.DeleteVolume(ctx, vol.Id)
}

// createTestTarGz creates a tar.gz archive with the given files
func createTestTarGz(t *testing.T, files map[string][]byte) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range files {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(content)),
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write(content)
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())

	return &buf
}

// TestVolumeFromArchive tests that a volume can be created from a tar.gz archive
// and the files are accessible when mounted to an instance
func TestVolumeFromArchive(t *testing.T) {
	// Require KVM
	if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
		t.Fatal("/dev/kvm not available - ensure KVM is enabled and user is in 'kvm' group")
	}

	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	manager, tmpDir := setupTestManager(t)
	ctx := context.Background()
	p := paths.New(tmpDir)

	// Setup: prepare image and system files
	imageManager, err := images.NewManager(p, 1)
	require.NoError(t, err)

	t.Log("Pulling alpine image...")
	_, err = imageManager.CreateImage(ctx, images.CreateImageRequest{
		Name: "docker.io/library/alpine:latest",
	})
	require.NoError(t, err)

	for i := 0; i < 60; i++ {
		img, err := imageManager.GetImage(ctx, "docker.io/library/alpine:latest")
		if err == nil && img.Status == images.StatusReady {
			break
		}
		time.Sleep(1 * time.Second)
	}
	t.Log("Image ready")

	systemManager := system.NewManager(p)
	t.Log("Ensuring system files...")
	err = systemManager.EnsureSystemFiles(ctx)
	require.NoError(t, err)
	t.Log("System files ready")

	// Create a tar.gz archive with test files
	t.Log("Creating test archive...")
	testFiles := map[string][]byte{
		"greeting.txt":         []byte("Hello from archive!"),
		"data/config.json":     []byte(`{"key": "value", "number": 42}`),
		"data/nested/deep.txt": []byte("Deep nested file content"),
	}
	archive := createTestTarGz(t, testFiles)

	// Create volume from archive
	volumeManager := volumes.NewManager(p, 0)
	t.Log("Creating volume from archive...")
	vol, err := volumeManager.CreateVolumeFromArchive(ctx, volumes.CreateVolumeFromArchiveRequest{
		Name:   "archive-data",
		SizeGb: 1,
	}, archive)
	require.NoError(t, err)
	t.Logf("Volume created: %s (size: %dGB)", vol.Id, vol.SizeGb)

	// Create instance with the volume attached
	t.Log("Creating instance with archive volume...")
	inst, err := manager.CreateInstance(ctx, CreateInstanceRequest{
		Name:           "archive-reader",
		Image:          "docker.io/library/alpine:latest",
		Size:           512 * 1024 * 1024,
		HotplugSize:    512 * 1024 * 1024,
		OverlaySize:    1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Volumes: []VolumeAttachment{
			{VolumeID: vol.Id, MountPath: "/archive", Readonly: true},
		},
	})
	require.NoError(t, err)
	t.Logf("Instance created: %s", inst.Id)

	// Wait for exec-agent
	err = waitForExecAgent(ctx, manager, inst.Id, 15*time.Second)
	require.NoError(t, err, "exec-agent should be ready")

	// Verify files from archive are present
	t.Log("Verifying archive files are accessible...")

	// Check greeting.txt
	output, code, err := execWithRetry(ctx, inst.VsockSocket, []string{"cat", "/archive/greeting.txt"})
	require.NoError(t, err)
	require.Equal(t, 0, code, "cat greeting.txt should succeed")
	assert.Equal(t, "Hello from archive!", strings.TrimSpace(output))
	t.Log("✓ greeting.txt verified")

	// Check data/config.json
	output, code, err = execWithRetry(ctx, inst.VsockSocket, []string{"cat", "/archive/data/config.json"})
	require.NoError(t, err)
	require.Equal(t, 0, code, "cat config.json should succeed")
	assert.Contains(t, output, `"key": "value"`)
	assert.Contains(t, output, `"number": 42`)
	t.Log("✓ data/config.json verified")

	// Check deeply nested file
	output, code, err = execWithRetry(ctx, inst.VsockSocket, []string{"cat", "/archive/data/nested/deep.txt"})
	require.NoError(t, err)
	require.Equal(t, 0, code, "cat deep.txt should succeed")
	assert.Equal(t, "Deep nested file content", strings.TrimSpace(output))
	t.Log("✓ data/nested/deep.txt verified")

	// List directory to confirm structure
	output, code, err = execWithRetry(ctx, inst.VsockSocket, []string{"find", "/archive", "-type", "f"})
	require.NoError(t, err)
	require.Equal(t, 0, code, "find should succeed")
	assert.Contains(t, output, "/archive/greeting.txt")
	assert.Contains(t, output, "/archive/data/config.json")
	assert.Contains(t, output, "/archive/data/nested/deep.txt")
	t.Log("✓ Directory structure verified")

	t.Log("Volume from archive test passed!")

	// Cleanup
	t.Log("Cleaning up...")
	manager.DeleteInstance(ctx, inst.Id)
	volumeManager.DeleteVolume(ctx, vol.Id)
}
