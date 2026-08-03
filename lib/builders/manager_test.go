package builders

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockInstanceChecker implements instanceChecker for testing
type mockInstanceChecker struct {
	instances  map[string]*instances.Instance
	getErr     error
	deleteFunc func(id string) error
	deleted    []string
}

func (m *mockInstanceChecker) GetInstance(ctx context.Context, idOrName string) (*instances.Instance, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	if inst, ok := m.instances[idOrName]; ok {
		return inst, nil
	}
	return nil, instances.ErrNotFound
}

func (m *mockInstanceChecker) DeleteInstance(ctx context.Context, id string) error {
	m.deleted = append(m.deleted, id)
	if m.deleteFunc != nil {
		return m.deleteFunc(id)
	}
	delete(m.instances, id)
	return nil
}

func setupTestManager(t *testing.T, cfg Config) (*manager, volumes.Manager, *mockInstanceChecker, *paths.Paths) {
	t.Helper()
	p := paths.New(t.TempDir())
	volumeMgr := volumes.NewManager(p, 0, nil)
	instMgr := &mockInstanceChecker{instances: map[string]*instances.Instance{}}
	mgr, err := NewManager(p, cfg, volumeMgr, instMgr, slog.Default(), nil)
	require.NoError(t, err)
	return mgr.(*manager), volumeMgr, instMgr, p
}

func waitForStatus(t *testing.T, mgr Manager, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := mgr.GetBuilder(context.Background(), id)
		if err == nil && b.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, err := mgr.GetBuilder(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, want, b.Status, "builder did not reach status %q", want)
}

func TestCreateBuilder_Defaults(t *testing.T) {
	mgr, volumeMgr, _, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{Name: "cache"})
	require.NoError(t, err)
	assert.NotEmpty(t, b.ID)
	assert.Equal(t, "cache", b.Name)
	assert.Equal(t, DefaultDiskSizeGb, b.DiskSizeGb)
	assert.Equal(t, StatusReady, b.Status)
	assert.Equal(t, DiskVolumeID(b.ID), b.DiskVolumeID)
	assert.Nil(t, b.LastUsedAt)

	// Disk is provisioned eagerly with the managed-by tag.
	vol, err := volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	require.NoError(t, err)
	assert.Equal(t, DefaultDiskSizeGb, vol.SizeGb)
	assert.Equal(t, managedByTagValue, vol.Tags[volumes.SystemTagNamespace+"managed-by"])
}

func TestCreateBuilder_CallerSuppliedID(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{})

	id := "team-cache-1"
	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{ID: &id, DiskSizeGb: 20})
	require.NoError(t, err)
	assert.Equal(t, id, b.ID)
	assert.Equal(t, "builder-disk-team-cache-1", b.DiskVolumeID)
	assert.Equal(t, 20, b.DiskSizeGb)

	// Same ID is a conflict, supporting control-plane idempotency.
	_, err = mgr.CreateBuilder(context.Background(), CreateBuilderRequest{ID: &id})
	assert.ErrorIs(t, err, ErrAlreadyExists)
}

func TestCreateBuilder_InvalidID(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{})

	for _, id := range []string{"has/slash", "has space", "..", "-leading-dash", "dot.name"} {
		_, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{ID: &id})
		assert.ErrorIs(t, err, ErrInvalidID, "id %q", id)
	}
}

func TestCreateBuilder_Quotas(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{MaxCount: 1, MaxDiskSizeGb: 60})

	_, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{DiskSizeGb: 61})
	assert.ErrorContains(t, err, "exceeds maximum")

	_, err = mgr.CreateBuilder(context.Background(), CreateBuilderRequest{DiskSizeGb: 60})
	require.NoError(t, err)

	_, err = mgr.CreateBuilder(context.Background(), CreateBuilderRequest{DiskSizeGb: 60})
	assert.ErrorIs(t, err, ErrQuotaExceeded)
}

func TestGetBuilder_NotFound(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{})
	_, err := mgr.GetBuilder(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListBuilders(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{})

	list, err := mgr.ListBuilders(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, list)
	assert.Empty(t, list)

	_, err = mgr.CreateBuilder(context.Background(), CreateBuilderRequest{Name: "a"})
	require.NoError(t, err)
	_, err = mgr.CreateBuilder(context.Background(), CreateBuilderRequest{Name: "a"}) // non-unique name ok
	require.NoError(t, err)

	list, err = mgr.ListBuilders(context.Background())
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestBuilderSurvivesRestart(t *testing.T) {
	mgr, volumeMgr, instMgr, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{Name: "cache", Tags: tags.Tags{"team": "x"}})
	require.NoError(t, err)

	// New manager over the same data dir loads on-disk state.
	restarted, err := NewManager(p, Config{}, volumeMgr, instMgr, slog.Default(), nil)
	require.NoError(t, err)

	got, err := restarted.GetBuilder(context.Background(), b.ID)
	require.NoError(t, err)
	assert.Equal(t, b.ID, got.ID)
	assert.Equal(t, "cache", got.Name)
	assert.Equal(t, tags.Tags{"team": "x"}, got.Tags)
	assert.Equal(t, StatusReady, got.Status)
}

func TestDeleteBuilder(t *testing.T) {
	mgr, volumeMgr, _, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	require.NoError(t, mgr.DeleteBuilder(context.Background(), b.ID))

	_, err = mgr.GetBuilder(context.Background(), b.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	assert.ErrorIs(t, err, volumes.ErrNotFound)
	_, err = os.Stat(p.BuilderDir(b.ID))
	assert.True(t, os.IsNotExist(err))

	assert.ErrorIs(t, mgr.DeleteBuilder(context.Background(), b.ID), ErrNotFound)
}

func TestDeleteBuilder_InUse(t *testing.T) {
	mgr, volumeMgr, _, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	// While acquired.
	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	require.NoError(t, err)
	assert.ErrorIs(t, mgr.DeleteBuilder(context.Background(), b.ID), ErrInUse)
	require.NoError(t, mgr.ReleaseBuild(context.Background(), b.ID, "build-1"))

	// While attached.
	err = volumeMgr.AttachVolume(context.Background(), b.DiskVolumeID, volumes.AttachVolumeRequest{
		InstanceID: "inst-1", MountPath: "/var/lib/buildkit",
	})
	require.NoError(t, err)
	assert.ErrorIs(t, mgr.DeleteBuilder(context.Background(), b.ID), ErrInUse)
}

func TestAcquireRelease_Exclusivity(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	require.NoError(t, err)

	// Second acquisition is rejected, even by the same build.
	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-2")
	assert.ErrorIs(t, err, ErrInUse)

	// Wrong holder cannot release.
	assert.Error(t, mgr.ReleaseBuild(context.Background(), b.ID, "build-2"))

	require.NoError(t, mgr.ReleaseBuild(context.Background(), b.ID, "build-1"))
	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-2")
	require.NoError(t, err)
}

func TestReleaseBuild_StampsLastUsed(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	require.NoError(t, err)
	require.NoError(t, mgr.ReleaseBuild(context.Background(), b.ID, "build-1"))

	got, err := mgr.GetBuilder(context.Background(), b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt)
	assert.WithinDuration(t, time.Now(), *got.LastUsedAt, time.Minute)
}

func TestAcquireForBuild_RecreatesMissingDisk(t *testing.T) {
	mgr, volumeMgr, _, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)
	require.NoError(t, volumeMgr.DeleteVolume(context.Background(), b.DiskVolumeID))

	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	require.NoError(t, err)

	_, err = volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	assert.NoError(t, err, "missing disk must be recreated empty as best-effort recovery")
}

func TestAcquireForBuild_ClearsStaleAttachments(t *testing.T) {
	mgr, volumeMgr, instMgr, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	// A stale builder VM from a crashed build holds the disk, alongside an
	// orphan record whose instance is already gone.
	instMgr.instances["inst-stale"] = &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-stale", Name: "builder-build-0"},
		State:          instances.StateRunning,
	}
	require.NoError(t, volumeMgr.AttachVolume(context.Background(), b.DiskVolumeID, volumes.AttachVolumeRequest{
		InstanceID: "inst-stale", MountPath: "/var/lib/buildkit", Readonly: true,
	}))
	require.NoError(t, volumeMgr.AttachVolume(context.Background(), b.DiskVolumeID, volumes.AttachVolumeRequest{
		InstanceID: "inst-gone", MountPath: "/var/lib/buildkit", Readonly: true,
	}))

	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	require.NoError(t, err, "acquire must clear stale attachments instead of failing")

	assert.Contains(t, instMgr.deleted, "inst-stale")
	vol, err := volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	require.NoError(t, err)
	assert.Empty(t, vol.Attachments, "stale holder and orphan records must be cleared")
}

func TestAcquireForBuild_StaleAttachmentClearFailsLoudly(t *testing.T) {
	mgr, volumeMgr, instMgr, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	instMgr.instances["inst-stuck"] = &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-stuck", Name: "builder-build-0"},
		State:          instances.StateRunning,
	}
	require.NoError(t, volumeMgr.AttachVolume(context.Background(), b.DiskVolumeID, volumes.AttachVolumeRequest{
		InstanceID: "inst-stuck", MountPath: "/var/lib/buildkit",
	}))
	instMgr.deleteFunc = func(id string) error { return errors.New("hypervisor unreachable") }

	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holding builder disk")

	// The failed acquisition must not hold the builder.
	delete(instMgr.instances, "inst-stuck")
	instMgr.deleteFunc = nil
	require.NoError(t, volumeMgr.DetachVolume(context.Background(), b.DiskVolumeID, "inst-stuck"))
	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	assert.NoError(t, err)
}

func TestResetDisk(t *testing.T) {
	mgr, volumeMgr, _, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	// Marker inside the volume dir proves the disk was recreated, not kept.
	marker := p.VolumeDir(b.DiskVolumeID) + "/marker"
	require.NoError(t, os.WriteFile(marker, []byte("x"), 0644))

	require.NoError(t, mgr.ResetDisk(context.Background(), b.ID))
	waitForStatus(t, mgr, b.ID, StatusReady)

	_, err = os.Stat(marker)
	assert.True(t, os.IsNotExist(err), "old disk must be gone")
	vol, err := volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	require.NoError(t, err)
	assert.Equal(t, managedByTagValue, vol.Tags[volumes.SystemTagNamespace+"managed-by"])

	// Identity is preserved.
	got, err := mgr.GetBuilder(context.Background(), b.ID)
	require.NoError(t, err)
	assert.Equal(t, b.ID, got.ID)
	assert.True(t, b.CreatedAt.Equal(got.CreatedAt), "created_at must survive the reset")
}

func TestResetDisk_InUse(t *testing.T) {
	mgr, volumeMgr, _, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	_, err = mgr.AcquireForBuild(context.Background(), b.ID, "build-1")
	require.NoError(t, err)
	assert.ErrorIs(t, mgr.ResetDisk(context.Background(), b.ID), ErrInUse)
	require.NoError(t, mgr.ReleaseBuild(context.Background(), b.ID, "build-1"))

	err = volumeMgr.AttachVolume(context.Background(), b.DiskVolumeID, volumes.AttachVolumeRequest{
		InstanceID: "inst-1", MountPath: "/var/lib/buildkit",
	})
	require.NoError(t, err)
	assert.ErrorIs(t, mgr.ResetDisk(context.Background(), b.ID), ErrInUse)
}

func TestReconcile_MissingDisk(t *testing.T) {
	mgr, volumeMgr, instMgr, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)
	require.NoError(t, volumeMgr.DeleteVolume(context.Background(), b.DiskVolumeID))

	restarted, err := NewManager(p, Config{}, volumeMgr, instMgr, slog.Default(), nil)
	require.NoError(t, err)
	require.NoError(t, restarted.Start(context.Background()))

	_, err = volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	assert.NoError(t, err, "reconciliation must recreate a missing disk")
}

func TestReconcile_OrphanAttachment(t *testing.T) {
	mgr, volumeMgr, instMgr, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)
	err = volumeMgr.AttachVolume(context.Background(), b.DiskVolumeID, volumes.AttachVolumeRequest{
		InstanceID: "inst-gone", MountPath: "/var/lib/buildkit",
	})
	require.NoError(t, err)

	restarted, err := NewManager(p, Config{}, volumeMgr, instMgr, slog.Default(), nil)
	require.NoError(t, err)
	require.NoError(t, restarted.Start(context.Background()))

	vol, err := volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	require.NoError(t, err)
	assert.Empty(t, vol.Attachments, "attachment whose instance is gone must be detached")
}

func TestReconcile_StaleInstanceDeleted(t *testing.T) {
	mgr, volumeMgr, instMgr, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)
	err = volumeMgr.AttachVolume(context.Background(), b.DiskVolumeID, volumes.AttachVolumeRequest{
		InstanceID: "inst-stale", MountPath: "/var/lib/buildkit",
	})
	require.NoError(t, err)
	instMgr.instances["inst-stale"] = &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-stale"},
	}
	// Simulate DeleteInstance failing to detach: the record survives deletion.
	instMgr.deleteFunc = func(id string) error {
		delete(instMgr.instances, id)
		return nil
	}

	restarted, err := NewManager(p, Config{}, volumeMgr, instMgr, slog.Default(), nil)
	require.NoError(t, err)
	require.NoError(t, restarted.Start(context.Background()))

	assert.Contains(t, instMgr.deleted, "inst-stale")
	vol, err := volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	require.NoError(t, err)
	assert.Empty(t, vol.Attachments, "surviving attachment records must be detached after stale instance deletion")
}

func TestReconcile_InterruptedDelete(t *testing.T) {
	mgr, volumeMgr, instMgr, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	// Crash mid-delete: status persisted as deleting, disk and metadata remain.
	meta, err := loadMetadata(p, b.ID)
	require.NoError(t, err)
	meta.Status = StatusDeleting
	require.NoError(t, saveMetadata(p, meta))

	restarted, err := NewManager(p, Config{}, volumeMgr, instMgr, slog.Default(), nil)
	require.NoError(t, err)
	require.NoError(t, restarted.Start(context.Background()))

	_, err = restarted.GetBuilder(context.Background(), b.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	assert.ErrorIs(t, err, volumes.ErrNotFound)
}

func TestReconcile_InterruptedPrune(t *testing.T) {
	mgr, volumeMgr, instMgr, p := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	// Crash mid-prune after the old disk was deleted.
	require.NoError(t, volumeMgr.DeleteVolume(context.Background(), b.DiskVolumeID))
	meta, err := loadMetadata(p, b.ID)
	require.NoError(t, err)
	meta.Status = StatusPruning
	require.NoError(t, saveMetadata(p, meta))

	restarted, err := NewManager(p, Config{}, volumeMgr, instMgr, slog.Default(), nil)
	require.NoError(t, err)
	require.NoError(t, restarted.Start(context.Background()))

	got, err := restarted.GetBuilder(context.Background(), b.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusReady, got.Status)
	_, err = volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	assert.NoError(t, err, "interrupted prune must finish recreating the disk")
}

func TestIdleReaper(t *testing.T) {
	m, volumeMgr, _, p := setupTestManager(t, Config{IdleTTL: time.Hour})

	b, err := m.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)

	// Backdate last activity beyond the TTL.
	meta, err := loadMetadata(p, b.ID)
	require.NoError(t, err)
	old := time.Now().Add(-2 * time.Hour)
	meta.LastUsedAt = &old
	require.NoError(t, saveMetadata(p, meta))

	m.reapIdle(context.Background())

	_, err = m.GetBuilder(context.Background(), b.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = volumeMgr.GetVolume(context.Background(), b.DiskVolumeID)
	assert.ErrorIs(t, err, volumes.ErrNotFound)

	// A fresh builder is kept.
	b2, err := m.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)
	m.reapIdle(context.Background())
	_, err = m.GetBuilder(context.Background(), b2.ID)
	assert.NoError(t, err)

	// An acquired builder past TTL is kept.
	_, err = m.AcquireForBuild(context.Background(), b2.ID, "build-1")
	require.NoError(t, err)
	meta, err = loadMetadata(p, b2.ID)
	require.NoError(t, err)
	meta.LastUsedAt = &old
	require.NoError(t, saveMetadata(p, meta))
	m.reapIdle(context.Background())
	_, err = m.GetBuilder(context.Background(), b2.ID)
	assert.NoError(t, err, "acquired builder must not be reaped")
}

func TestValidateBuilderID(t *testing.T) {
	assert.NoError(t, ValidateBuilderID("abc"))
	assert.NoError(t, ValidateBuilderID("team-cache_1"))
	assert.ErrorIs(t, ValidateBuilderID(""), ErrInvalidID)
	assert.ErrorIs(t, ValidateBuilderID("../escape"), ErrInvalidID)
	assert.ErrorIs(t, ValidateBuilderID("a/b"), ErrInvalidID)
}

func TestResetDisk_NotFound(t *testing.T) {
	mgr, _, _, _ := setupTestManager(t, Config{})
	assert.ErrorIs(t, mgr.ResetDisk(context.Background(), "nope"), ErrNotFound)
}

func TestDeleteBuilder_ToleratesMissingDisk(t *testing.T) {
	mgr, volumeMgr, _, _ := setupTestManager(t, Config{})

	b, err := mgr.CreateBuilder(context.Background(), CreateBuilderRequest{})
	require.NoError(t, err)
	require.NoError(t, volumeMgr.DeleteVolume(context.Background(), b.DiskVolumeID))

	assert.NoError(t, mgr.DeleteBuilder(context.Background(), b.ID))
}
