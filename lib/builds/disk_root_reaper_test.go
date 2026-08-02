package builds

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeBuildMetadataForReaper writes build metadata with the given status
// directly to disk.
func writeBuildMetadataForReaper(t *testing.T, mgr *manager, id, status string) {
	t.Helper()
	meta := &buildMetadata{
		ID:        id,
		Status:    status,
		Request:   &CreateBuildRequest{Dockerfile: "FROM alpine"},
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(mgr.paths, meta))
}

// addDiskRootVolume registers a build-disk-* volume in the mock volume manager.
func addDiskRootVolume(volumeMgr *mockVolumeManager, id string) {
	volumeMgr.volumes[id] = &volumes.Volume{Id: id, Name: id}
}

func TestSweepOrphanedDiskRootVolumes_ReapsTerminalBuild(t *testing.T) {
	for _, status := range []string{StatusReady, StatusFailed, StatusCancelled} {
		t.Run(status, func(t *testing.T) {
			mgr, _, volumeMgr, tempDir := setupTestManager(t)
			defer os.RemoveAll(tempDir)

			writeBuildMetadataForReaper(t, mgr, "build-1", status)
			addDiskRootVolume(volumeMgr, "build-disk-build-1")

			mgr.sweepOrphanedDiskRootVolumes(context.Background())

			_, err := volumeMgr.GetVolume(context.Background(), "build-disk-build-1")
			assert.ErrorIs(t, err, volumes.ErrNotFound)
		})
	}
}

func TestSweepOrphanedDiskRootVolumes_ReapsMissingMetadata(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	// No metadata on disk: the build crashed so hard nothing was recorded,
	// or the metadata was already cleaned up.
	addDiskRootVolume(volumeMgr, "build-disk-build-1")

	mgr.sweepOrphanedDiskRootVolumes(context.Background())

	_, err := volumeMgr.GetVolume(context.Background(), "build-disk-build-1")
	assert.ErrorIs(t, err, volumes.ErrNotFound)
}

func TestSweepOrphanedDiskRootVolumes_SkipsActiveBuild(t *testing.T) {
	for _, status := range []string{StatusQueued, StatusBuilding, StatusPushing} {
		t.Run(status, func(t *testing.T) {
			mgr, _, volumeMgr, tempDir := setupTestManager(t)
			defer os.RemoveAll(tempDir)

			writeBuildMetadataForReaper(t, mgr, "build-1", status)
			addDiskRootVolume(volumeMgr, "build-disk-build-1")

			mgr.sweepOrphanedDiskRootVolumes(context.Background())

			_, err := volumeMgr.GetVolume(context.Background(), "build-disk-build-1")
			require.NoError(t, err)
			assert.Equal(t, 0, volumeMgr.deleteCallCount)
		})
	}
}

func TestSweepOrphanedDiskRootVolumes_SkipsErrInUse(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	writeBuildMetadataForReaper(t, mgr, "build-1", StatusFailed)
	addDiskRootVolume(volumeMgr, "build-disk-build-1")
	volumeMgr.deleteFunc = func(ctx context.Context, id string) error {
		return volumes.ErrInUse
	}

	mgr.sweepOrphanedDiskRootVolumes(context.Background())

	_, err := volumeMgr.GetVolume(context.Background(), "build-disk-build-1")
	require.NoError(t, err, "volume still attached to a builder VM must be left alone")
}

func TestSweepOrphanedDiskRootVolumes_SkipsErrNotFound(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	writeBuildMetadataForReaper(t, mgr, "build-1", StatusFailed)
	addDiskRootVolume(volumeMgr, "build-disk-build-1")
	volumeMgr.deleteFunc = func(ctx context.Context, id string) error {
		return volumes.ErrNotFound
	}

	// Must not panic or error; the volume disappeared between list and delete.
	mgr.sweepOrphanedDiskRootVolumes(context.Background())
	assert.Equal(t, 1, volumeMgr.deleteCallCount)
}

func TestSweepOrphanedDiskRootVolumes_IgnoresNonMatchingNames(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	writeBuildMetadataForReaper(t, mgr, "build-1", StatusFailed)
	for _, id := range []string{
		"build-source-build-1", // source volume, own lifecycle
		"build-config-build-1", // config volume, own lifecycle
		"build-disk-",          // no build ID suffix
		"build-disk",           // missing dash
		"data-build-disk-1",    // prefix not at start
		"tenant-data",
	} {
		addDiskRootVolume(volumeMgr, id)
	}

	mgr.sweepOrphanedDiskRootVolumes(context.Background())

	assert.Equal(t, 0, volumeMgr.deleteCallCount)
	assert.Len(t, volumeMgr.volumes, 6)
}

func TestSweepOrphanedDiskRootVolumes_RunsWhenDiskRootDisabled(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	// DiskRootEnabled is false in the default test config; orphans from when
	// the feature was on must still be reaped.
	require.False(t, mgr.config.DiskRootEnabled)
	writeBuildMetadataForReaper(t, mgr, "build-1", StatusFailed)
	addDiskRootVolume(volumeMgr, "build-disk-build-1")

	mgr.sweepOrphanedDiskRootVolumes(context.Background())

	_, err := volumeMgr.GetVolume(context.Background(), "build-disk-build-1")
	assert.ErrorIs(t, err, volumes.ErrNotFound)
}

func TestSweepOrphanedDiskRootVolumes_ListError(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	errMgr := &mockVolumeManager{listFunc: func(ctx context.Context) ([]volumes.Volume, error) {
		return nil, errors.New("storage unavailable")
	}}
	mgr.volumeManager = errMgr

	// Must not panic; the sweep is best-effort.
	mgr.sweepOrphanedDiskRootVolumes(context.Background())
	assert.Equal(t, 0, errMgr.deleteCallCount)
}
