package volumes

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/onkernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestManager(t *testing.T) (Manager, *paths.Paths, func()) {
	t.Helper()

	// Create temp dir
	tmpDir, err := os.MkdirTemp("", "volume-test-*")
	require.NoError(t, err)

	p := paths.New(tmpDir)

	// Create required directories
	require.NoError(t, os.MkdirAll(p.VolumesDir(), 0755))

	manager := NewManager(p, 0, nil) // 0 = unlimited storage

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return manager, p, cleanup
}

func TestMultiAttach_FirstAttachmentRW(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// First attachment as read-write should succeed
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   false,
	})
	assert.NoError(t, err)

	// Verify attachment
	vol, err = manager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	require.Len(t, vol.Attachments, 1)
	assert.Equal(t, "instance-1", vol.Attachments[0].InstanceID)
	assert.Equal(t, "/data", vol.Attachments[0].MountPath)
	assert.False(t, vol.Attachments[0].Readonly)
}

func TestMultiAttach_FirstAttachmentRO(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// First attachment as read-only should succeed
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   true,
	})
	assert.NoError(t, err)

	// Verify attachment
	vol, err = manager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	require.Len(t, vol.Attachments, 1)
	assert.True(t, vol.Attachments[0].Readonly)
}

func TestMultiAttach_RejectSecondAttachWhenRW(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// First attachment as read-write
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   false,
	})
	require.NoError(t, err)

	// Second attachment (either RO or RW) should fail
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-2",
		MountPath:  "/data",
		Readonly:   true, // Even RO should fail when existing is RW
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "exclusive read-write attachment")
}

func TestMultiAttach_AllowMultipleRO(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// First attachment as read-only
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   true,
	})
	require.NoError(t, err)

	// Second attachment as read-only should succeed
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-2",
		MountPath:  "/data",
		Readonly:   true,
	})
	assert.NoError(t, err)

	// Verify both attachments
	vol, err = manager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.Len(t, vol.Attachments, 2)
}

func TestMultiAttach_RejectRWWhenExistingRO(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// First attachment as read-only
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   true,
	})
	require.NoError(t, err)

	// Second attachment as read-write should fail
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-2",
		MountPath:  "/data",
		Readonly:   false,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot attach read-write")
}

func TestMultiAttach_RejectDuplicateInstance(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// First attachment
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   true,
	})
	require.NoError(t, err)

	// Same instance trying to attach again should fail
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/other",
		Readonly:   true,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already attached")
}

func TestDetach_RemovesSpecificAttachment(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// Attach to two instances
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   true,
	})
	require.NoError(t, err)

	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-2",
		MountPath:  "/data",
		Readonly:   true,
	})
	require.NoError(t, err)

	// Detach instance-1
	err = manager.DetachVolume(ctx, vol.Id, "instance-1")
	assert.NoError(t, err)

	// Verify only instance-2 remains
	vol, err = manager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	require.Len(t, vol.Attachments, 1)
	assert.Equal(t, "instance-2", vol.Attachments[0].InstanceID)
}

func TestDetach_ErrorIfNotAttached(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// Detach from instance that's not attached
	err = manager.DetachVolume(ctx, vol.Id, "instance-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not attached")
}

func TestDeleteVolume_RejectIfAttached(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "test-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// Attach it
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   true,
	})
	require.NoError(t, err)

	// Try to delete - should fail
	err = manager.DeleteVolume(ctx, vol.Id)
	assert.ErrorIs(t, err, ErrInUse)
}

func TestMultiAttach_ConcurrentAttachments(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "concurrent-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// Launch multiple goroutines trying to attach simultaneously
	const numGoroutines = 10
	results := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(instanceNum int) {
			err := manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
				InstanceID: fmt.Sprintf("instance-%d", instanceNum),
				MountPath:  "/data",
				Readonly:   true,
			})
			results <- err
		}(i)
	}

	// Collect results
	var successCount, errorCount int
	for i := 0; i < numGoroutines; i++ {
		err := <-results
		if err == nil {
			successCount++
		} else {
			errorCount++
		}
	}

	// All should succeed since all are read-only
	assert.Equal(t, numGoroutines, successCount, "All read-only attachments should succeed")
	assert.Equal(t, 0, errorCount, "No errors expected for concurrent read-only attachments")

	// Verify final state has all attachments
	vol, err = manager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.Len(t, vol.Attachments, numGoroutines, "Should have all attachments")
}

func TestMultiAttach_ConcurrentRWConflict(t *testing.T) {
	manager, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	// Create a volume
	vol, err := manager.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "rw-conflict-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// Launch multiple goroutines trying to attach read-write simultaneously
	const numGoroutines = 5
	results := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(instanceNum int) {
			err := manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
				InstanceID: fmt.Sprintf("instance-%d", instanceNum),
				MountPath:  "/data",
				Readonly:   false, // All trying read-write
			})
			results <- err
		}(i)
	}

	// Collect results
	var successCount, errorCount int
	for i := 0; i < numGoroutines; i++ {
		err := <-results
		if err == nil {
			successCount++
		} else {
			errorCount++
		}
	}

	// Only ONE should succeed (first one gets exclusive lock)
	assert.Equal(t, 1, successCount, "Exactly one read-write attachment should succeed")
	assert.Equal(t, numGoroutines-1, errorCount, "Others should fail due to exclusive lock")

	// Verify final state has exactly one attachment
	vol, err = manager.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.Len(t, vol.Attachments, 1, "Should have exactly one attachment")
	assert.False(t, vol.Attachments[0].Readonly, "Attachment should be read-write")
}
