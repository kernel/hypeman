package volumes

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
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

	// Second attachment as RO should fail when existing is RW
	err = manager.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-2",
		MountPath:  "/data",
		Readonly:   true, // RO should fail when existing is RW
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "existing read-write attachment")
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
	assert.Contains(t, err.Error(), "cannot attach ReadWriteOnce")
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

func TestRWX_RejectWithoutNFSHost(t *testing.T) {
	mgr, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	vol, err := mgr.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "rwx-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// ReadWriteMany should fail because NFS host is not configured
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		AccessMode: AccessReadWriteMany,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "NFS host not configured")
}

func TestRWX_NFSInfoNilWhenNotServed(t *testing.T) {
	mgr, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	vol, err := mgr.CreateVolume(ctx, CreateVolumeRequest{
		Name:   "no-nfs-vol",
		SizeGb: 1,
	})
	require.NoError(t, err)

	// Single rw attachment — no NFS
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "instance-1",
		MountPath:  "/data",
		Readonly:   false,
	})
	require.NoError(t, err)

	nfsInfo, err := mgr.GetVolumeNFSInfo(ctx, vol.Id)
	require.NoError(t, err)
	assert.Nil(t, nfsInfo, "NFS info should be nil for single rw attachment")
}

func TestRWX_NFSMetadataPersistence(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "volume-nfs-persist-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.VolumeDir("vol-nfs-1"), 0755))

	// Save metadata with NFS info
	meta := &storedMetadata{
		Id:     "vol-nfs-1",
		Name:   "nfs-vol",
		SizeGb: 5,
		NFS: &storedNFSInfo{
			Host:       "10.100.0.1",
			ExportPath: "/data/volumes/vol-nfs-1/nfs_mount",
		},
		Attachments: []storedAttachment{
			{InstanceID: "inst-1", MountPath: "/data", Readonly: false, NFS: false},
			{InstanceID: "inst-2", MountPath: "/data", Readonly: false, NFS: true},
		},
	}
	require.NoError(t, saveMetadata(p, meta))

	// Reload and verify
	loaded, err := loadMetadata(p, "vol-nfs-1")
	require.NoError(t, err)
	require.NotNil(t, loaded.NFS)
	assert.Equal(t, "10.100.0.1", loaded.NFS.Host)
	assert.Equal(t, "/data/volumes/vol-nfs-1/nfs_mount", loaded.NFS.ExportPath)
	require.Len(t, loaded.Attachments, 2)
	assert.False(t, loaded.Attachments[0].NFS)
	assert.True(t, loaded.Attachments[1].NFS)

	// Verify domain conversion
	vol := (&manager{}).metadataToVolume(loaded)
	require.NotNil(t, vol.NFS)
	assert.Equal(t, "10.100.0.1", vol.NFS.Host)
	assert.True(t, vol.Attachments[1].NFS)
}

func TestRWX_DetachClearsNFSWhenNoConsumers(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "volume-nfs-detach-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.VolumeDir("vol-detach-1"), 0755))

	// Set up metadata with NFS and two attachments (one NFS, one not)
	meta := &storedMetadata{
		Id:     "vol-detach-1",
		Name:   "detach-vol",
		SizeGb: 5,
		NFS: &storedNFSInfo{
			Host:       "10.100.0.1",
			ExportPath: "/data/volumes/vol-detach-1/nfs_mount",
		},
		Attachments: []storedAttachment{
			{InstanceID: "inst-1", MountPath: "/data", Readonly: false, NFS: false},
			{InstanceID: "inst-2", MountPath: "/data", Readonly: false, NFS: true},
		},
	}
	require.NoError(t, saveMetadata(p, meta))

	mgr := &manager{
		paths: p,
		nfs:   newNFSManager(p),
	}
	ctx := context.Background()

	// Detach the NFS consumer
	err = mgr.DetachVolume(ctx, "vol-detach-1", "inst-2")
	require.NoError(t, err)

	// Verify NFS info is cleared (no NFS consumers remain)
	loaded, err := loadMetadata(p, "vol-detach-1")
	require.NoError(t, err)
	assert.Nil(t, loaded.NFS, "NFS info should be cleared when no NFS consumers remain")
	require.Len(t, loaded.Attachments, 1)
}

func TestRWX_DetachKeepsNFSWithRemainingConsumers(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "volume-nfs-keep-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.VolumeDir("vol-keep-1"), 0755))

	// Set up metadata with NFS and three attachments (two NFS)
	meta := &storedMetadata{
		Id:     "vol-keep-1",
		Name:   "keep-vol",
		SizeGb: 5,
		NFS: &storedNFSInfo{
			Host:       "10.100.0.1",
			ExportPath: "/data/volumes/vol-keep-1/nfs_mount",
		},
		Attachments: []storedAttachment{
			{InstanceID: "inst-1", MountPath: "/data", Readonly: false, NFS: false},
			{InstanceID: "inst-2", MountPath: "/data", Readonly: false, NFS: true},
			{InstanceID: "inst-3", MountPath: "/data", Readonly: false, NFS: true},
		},
	}
	require.NoError(t, saveMetadata(p, meta))

	mgr := &manager{
		paths: p,
		nfs:   newNFSManager(p),
	}
	ctx := context.Background()

	// Detach one NFS consumer
	err = mgr.DetachVolume(ctx, "vol-keep-1", "inst-2")
	require.NoError(t, err)

	// Verify NFS info is still present (inst-3 still uses NFS)
	loaded, err := loadMetadata(p, "vol-keep-1")
	require.NoError(t, err)
	assert.NotNil(t, loaded.NFS, "NFS info should be kept when NFS consumers remain")
	require.Len(t, loaded.Attachments, 2)
}

// --- AccessMode tests ---

func TestAccessMode_ReadWriteOnceExclusive(t *testing.T) {
	mgr, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	vol, err := mgr.CreateVolume(ctx, CreateVolumeRequest{Name: "am-vol", SizeGb: 1})
	require.NoError(t, err)

	// First RWO succeeds
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "inst-1",
		MountPath:  "/data",
		AccessMode: AccessReadWriteOnce,
	})
	require.NoError(t, err)

	// Second RWO is rejected
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "inst-2",
		MountPath:  "/data",
		AccessMode: AccessReadWriteOnce,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot attach ReadWriteOnce")
}

func TestAccessMode_ReadOnlyManyAllowsMultiple(t *testing.T) {
	mgr, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	vol, err := mgr.CreateVolume(ctx, CreateVolumeRequest{Name: "rom-vol", SizeGb: 1})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
			InstanceID: fmt.Sprintf("inst-%d", i),
			MountPath:  "/data",
			AccessMode: AccessReadOnlyMany,
		})
		require.NoError(t, err)
	}

	vol, err = mgr.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.Len(t, vol.Attachments, 3)
	for _, att := range vol.Attachments {
		assert.True(t, att.Readonly)
		assert.False(t, att.NFS)
	}
}

func TestAccessMode_ReadWriteManyUsesNFS(t *testing.T) {
	// Test via metadata (NFS loop mount requires real disk, so we test the stored state)
	tmpDir, err := os.MkdirTemp("", "volume-rwx-am-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.VolumeDir("vol-rwx-1"), 0755))

	// Simulate two RWX attachments with NFS already set up
	meta := &storedMetadata{
		Id:     "vol-rwx-1",
		Name:   "rwx-vol",
		SizeGb: 5,
		NFS: &storedNFSInfo{
			Host:       "10.100.0.1",
			ExportPath: "/data/volumes/vol-rwx-1/nfs_mount",
		},
		Attachments: []storedAttachment{
			{InstanceID: "inst-1", MountPath: "/data", Readonly: false, NFS: true, AccessMode: "ReadWriteMany"},
			{InstanceID: "inst-2", MountPath: "/data", Readonly: false, NFS: true, AccessMode: "ReadWriteMany"},
		},
	}
	require.NoError(t, saveMetadata(p, meta))

	// Verify round-trip
	loaded, err := loadMetadata(p, "vol-rwx-1")
	require.NoError(t, err)
	assert.Len(t, loaded.Attachments, 2)
	for _, att := range loaded.Attachments {
		assert.True(t, att.NFS)
		assert.False(t, att.Readonly)
		assert.Equal(t, "ReadWriteMany", att.AccessMode)
	}
	assert.NotNil(t, loaded.NFS)

	vol := (&manager{}).metadataToVolume(loaded)
	assert.Len(t, vol.Attachments, 2)
	for _, att := range vol.Attachments {
		assert.True(t, att.NFS)
	}
	assert.NotNil(t, vol.NFS)
}

func TestAccessMode_RWXRejectsWithRWOExisting(t *testing.T) {
	mgr, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	vol, err := mgr.CreateVolume(ctx, CreateVolumeRequest{Name: "conflict-vol", SizeGb: 1})
	require.NoError(t, err)

	// Attach as RWO (legacy readonly=false)
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "inst-1",
		MountPath:  "/data",
		Readonly:   false,
	})
	require.NoError(t, err)

	// RWX should be rejected — there's an existing RWO attachment
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "inst-2",
		MountPath:  "/data",
		AccessMode: AccessReadWriteMany,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "existing ReadWriteOnce attachment")
}

func TestAccessMode_RWORejectsWithRWXExisting(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "volume-rwo-rwx-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.VolumeDir("vol-rwo-rwx-1"), 0755))

	// Pre-set metadata with an existing RWX attachment
	meta := &storedMetadata{
		Id:     "vol-rwo-rwx-1",
		Name:   "rwo-rwx-vol",
		SizeGb: 5,
		NFS: &storedNFSInfo{
			Host:       "10.100.0.1",
			ExportPath: "/data/volumes/vol-rwo-rwx-1/nfs_mount",
		},
		Attachments: []storedAttachment{
			{InstanceID: "inst-1", MountPath: "/data", Readonly: false, NFS: true, AccessMode: "ReadWriteMany"},
		},
	}
	require.NoError(t, saveMetadata(p, meta))

	mgr := &manager{
		paths:   p,
		nfs:     newNFSManager(p),
		nfsHost: "10.100.0.1",
	}
	ctx := context.Background()

	// RWO should be rejected — there's an existing RWX attachment
	err = mgr.AttachVolume(ctx, "vol-rwo-rwx-1", AttachVolumeRequest{
		InstanceID: "inst-2",
		MountPath:  "/data",
		AccessMode: AccessReadWriteOnce,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "existing ReadWriteMany attachments")
}

func TestAccessMode_LegacyReadonlyDoesNotTriggerNFS(t *testing.T) {
	mgr, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	vol, err := mgr.CreateVolume(ctx, CreateVolumeRequest{Name: "legacy-vol", SizeGb: 1})
	require.NoError(t, err)

	// Legacy readonly=false → ReadWriteOnce (no NFS)
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "inst-1",
		MountPath:  "/data",
		Readonly:   false,
	})
	require.NoError(t, err)

	vol, err = mgr.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.False(t, vol.Attachments[0].NFS)
	assert.Nil(t, vol.NFS)
}

func TestAccessMode_AccessModeWinsOverReadonly(t *testing.T) {
	mgr, _, cleanup := setupTestManager(t)
	defer cleanup()
	ctx := context.Background()

	vol, err := mgr.CreateVolume(ctx, CreateVolumeRequest{Name: "precedence-vol", SizeGb: 1})
	require.NoError(t, err)

	// readonly=true but access_mode=ReadWriteOnce → access_mode wins (rw)
	err = mgr.AttachVolume(ctx, vol.Id, AttachVolumeRequest{
		InstanceID: "inst-1",
		MountPath:  "/data",
		Readonly:   true,
		AccessMode: AccessReadWriteOnce,
	})
	require.NoError(t, err)

	vol, err = mgr.GetVolume(ctx, vol.Id)
	require.NoError(t, err)
	assert.False(t, vol.Attachments[0].Readonly, "access_mode=ReadWriteOnce should override readonly=true")
}

func TestAccessMode_ROManyRejectsWithRWXExisting(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "volume-rom-rwx-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	require.NoError(t, os.MkdirAll(p.VolumeDir("vol-rom-rwx-1"), 0755))

	// Pre-set metadata with an existing RWX attachment
	meta := &storedMetadata{
		Id:     "vol-rom-rwx-1",
		Name:   "rom-rwx-vol",
		SizeGb: 5,
		NFS: &storedNFSInfo{
			Host:       "10.100.0.1",
			ExportPath: "/data/volumes/vol-rom-rwx-1/nfs_mount",
		},
		Attachments: []storedAttachment{
			{InstanceID: "inst-1", MountPath: "/data", Readonly: false, NFS: true, AccessMode: "ReadWriteMany"},
		},
	}
	require.NoError(t, saveMetadata(p, meta))

	mgr := &manager{
		paths:   p,
		nfs:     newNFSManager(p),
		nfsHost: "10.100.0.1",
	}
	ctx := context.Background()

	// ReadOnlyMany should be rejected when RWX exists
	err = mgr.AttachVolume(ctx, "vol-rom-rwx-1", AttachVolumeRequest{
		InstanceID: "inst-2",
		MountPath:  "/data",
		AccessMode: AccessReadOnlyMany,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "existing ReadWriteMany attachments")
}

func TestAccessMode_ResolveAccessMode(t *testing.T) {
	tests := []struct {
		name     string
		req      AttachVolumeRequest
		expected AccessMode
	}{
		{"default (neither set)", AttachVolumeRequest{}, AccessReadWriteOnce},
		{"readonly=true", AttachVolumeRequest{Readonly: true}, AccessReadOnlyMany},
		{"readonly=false", AttachVolumeRequest{Readonly: false}, AccessReadWriteOnce},
		{"explicit RWO", AttachVolumeRequest{AccessMode: AccessReadWriteOnce}, AccessReadWriteOnce},
		{"explicit ROM", AttachVolumeRequest{AccessMode: AccessReadOnlyMany}, AccessReadOnlyMany},
		{"explicit RWX", AttachVolumeRequest{AccessMode: AccessReadWriteMany}, AccessReadWriteMany},
		{"access_mode wins over readonly", AttachVolumeRequest{Readonly: true, AccessMode: AccessReadWriteOnce}, AccessReadWriteOnce},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.req.ResolveAccessMode())
		})
	}
}

func TestCreateVolume_MetadataRoundTrip(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "volume-metadata-*")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	p := paths.New(tmpDir)
	meta := &storedMetadata{
		Id:          "vol-metadata-1",
		Name:        "tagged-vol",
		SizeGb:      10,
		Attachments: []storedAttachment{},
		Tags:        map[string]string{"team": "backend", "env": "staging"},
	}
	require.NoError(t, os.MkdirAll(p.VolumeDir(meta.Id), 0755))
	require.NoError(t, saveMetadata(p, meta))

	loaded, err := loadMetadata(p, meta.Id)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, loaded.Tags)

	vol := (&manager{}).metadataToVolume(loaded)
	require.Equal(t, map[string]string{"team": "backend", "env": "staging"}, vol.Tags)

	// Verify deep-copy behavior from persisted metadata.
	loaded.Tags["team"] = "mutated"
	require.Equal(t, "backend", vol.Tags["team"])
}
