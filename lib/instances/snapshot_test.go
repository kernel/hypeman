package instances

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoppedSnapshotLifecycleAndForkAfterSourceDeletion(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-stopped-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, "snapshot-stopped-src", hvType)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "stopped-baseline",
	})
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStopped, snap.Kind)

	restored, err := mgr.RestoreSnapshot(ctx, sourceID, snap.Id, RestoreSnapshotRequest{
		TargetState:      StateStopped,
		TargetHypervisor: hvType,
	})
	require.NoError(t, err)
	require.Equal(t, StateStopped, restored.State)
	require.Equal(t, hvType, restored.HypervisorType)

	require.NoError(t, mgr.DeleteInstance(ctx, sourceID))

	got, err := mgr.GetSnapshot(ctx, snap.Id)
	require.NoError(t, err)
	require.Equal(t, snap.Id, got.Id)

	forked, err := mgr.ForkSnapshot(ctx, snap.Id, ForkSnapshotRequest{
		Name:             "snapshot-stopped-fork",
		TargetState:      StateStopped,
		TargetHypervisor: hvType,
	})
	require.NoError(t, err)
	require.Equal(t, StateStopped, forked.State)
	require.Equal(t, hvType, forked.HypervisorType)

	forkMeta, err := mgr.loadMetadata(forked.Id)
	require.NoError(t, err)
	require.Equal(t, snap.Id, forkMeta.ForkOfSnapshot)

	err = mgr.DeleteSnapshot(ctx, snap.Id)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)

	require.NoError(t, mgr.DeleteInstance(ctx, forked.Id))
	require.NoError(t, mgr.DeleteSnapshot(ctx, snap.Id))
	_, err = mgr.GetSnapshot(ctx, snap.Id)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestStandbySnapshotRejectsTargetHypervisorOverride(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-standby-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-standby-src", hvType)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-baseline",
	})
	require.NoError(t, err)

	_, err = mgr.RestoreSnapshot(ctx, sourceID, snap.Id, RestoreSnapshotRequest{
		TargetState:      StateStandby,
		TargetHypervisor: hvType,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestRestoreSnapshotCancelsSourceInstanceCompressionJob(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-restore-src"
	snapshotID := "snapshot-restore-race"

	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-restore-src", hvType)

	snapshotGuestDir := mgr.paths.SnapshotGuestDir(snapshotID)
	require.NoError(t, os.MkdirAll(mgr.paths.SnapshotDir(snapshotID), 0755))
	require.NoError(t, mgr.copySnapshotPayload(sourceID, snapshotGuestDir))

	sourceMeta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	require.NoError(t, mgr.saveSnapshotRecord(&snapshotRecord{
		Snapshot: Snapshot{
			Id:               snapshotID,
			Name:             "restore-race",
			Kind:             SnapshotKindStandby,
			SourceInstanceID: sourceID,
			SourceName:       sourceMeta.Name,
			SourceHypervisor: hvType,
			CreatedAt:        time.Now(),
			SizeBytes:        1,
		},
		StoredMetadata: sourceMeta.StoredMetadata,
	}))

	var instanceCanceled atomic.Bool
	var snapshotCanceled atomic.Bool
	instanceDone := make(chan struct{})
	snapshotDone := make(chan struct{})

	mgr.compressionMu.Lock()
	mgr.compressionJobs[mgr.snapshotJobKeyForInstance(sourceID)] = &compressionJob{
		cancel: func() {
			instanceCanceled.Store(true)
			select {
			case <-instanceDone:
			default:
				close(instanceDone)
			}
		},
		done: instanceDone,
		target: compressionTarget{
			Key:         mgr.snapshotJobKeyForInstance(sourceID),
			OwnerID:     sourceID,
			SnapshotDir: mgr.paths.InstanceSnapshotLatest(sourceID),
		},
	}
	mgr.compressionJobs[mgr.snapshotJobKeyForSnapshot(snapshotID)] = &compressionJob{
		cancel: func() {
			snapshotCanceled.Store(true)
			select {
			case <-snapshotDone:
			default:
				close(snapshotDone)
			}
		},
		done: snapshotDone,
		target: compressionTarget{
			Key:         mgr.snapshotJobKeyForSnapshot(snapshotID),
			SnapshotID:  snapshotID,
			SnapshotDir: snapshotGuestDir,
		},
	}
	mgr.compressionMu.Unlock()

	restored, err := mgr.RestoreSnapshot(ctx, sourceID, snapshotID, RestoreSnapshotRequest{
		TargetState: StateStandby,
	})
	require.NoError(t, err)
	require.Equal(t, StateStandby, restored.State)
	assert.True(t, snapshotCanceled.Load(), "snapshot compression job should be canceled before restore")
	assert.True(t, instanceCanceled.Load(), "instance compression job should be canceled before restore")
}

func TestCreateStandbySnapshotCancelsSourceInstanceCompressionJob(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-create-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-create-src", hvType)

	var instanceCanceled atomic.Bool
	instanceDone := make(chan struct{})

	mgr.compressionMu.Lock()
	mgr.compressionJobs[mgr.snapshotJobKeyForInstance(sourceID)] = &compressionJob{
		cancel: func() {
			instanceCanceled.Store(true)
			select {
			case <-instanceDone:
			default:
				close(instanceDone)
			}
		},
		done: instanceDone,
		target: compressionTarget{
			Key:         mgr.snapshotJobKeyForInstance(sourceID),
			OwnerID:     sourceID,
			SnapshotDir: mgr.paths.InstanceSnapshotLatest(sourceID),
		},
	}
	mgr.compressionMu.Unlock()

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-copy",
	})
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStandby, snap.Kind)
	assert.True(t, instanceCanceled.Load(), "instance compression job should be canceled before copying standby snapshot payload")
}

func TestCreateStandbySnapshotFromCompressedSourceCopiesRawMemory(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-create-compressed-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-create-compressed-src", hvType)

	rawPath := filepath.Join(mgr.paths.InstanceSnapshotLatest(sourceID), "memory-ranges")
	require.NoError(t, os.WriteFile(rawPath, []byte("some guest memory"), 0644))
	_, _, err := compressSnapshotMemoryFile(ctx, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-from-compressed",
	})
	require.NoError(t, err)

	snapshotDir := mgr.paths.SnapshotGuestDir(snap.Id)
	_, ok := findRawSnapshotMemoryFile(snapshotDir)
	assert.True(t, ok, "snapshot copy should contain raw memory after preparing a compressed standby source")
	_, _, ok = findCompressedSnapshotMemoryFile(snapshotDir)
	assert.False(t, ok, "snapshot copy should not inherit compressed memory artifacts from the source standby instance")
}

func TestForkSnapshotFromCompressedSourceCopiesRawMemory(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-fork-compressed-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-fork-compressed-src", hvType)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-for-fork-compressed",
	})
	require.NoError(t, err)

	snapshotDir := mgr.paths.SnapshotGuestDir(snap.Id)
	rawPath := filepath.Join(snapshotDir, "memory-ranges")
	snapshotConfigPath := filepath.Join(snapshotDir, "snapshots", "snapshot-latest", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0o755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(rawPath, []byte("some guest memory"), 0o644))
	_, _, err = compressSnapshotMemoryFile(ctx, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)

	forked, err := mgr.ForkSnapshot(ctx, snap.Id, ForkSnapshotRequest{
		Name:        "snapshot-fork-compressed",
		TargetState: StateStopped,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), forked.Id) })

	forkSnapshotDir := mgr.paths.InstanceDir(forked.Id)
	_, ok := findRawSnapshotMemoryFile(forkSnapshotDir)
	assert.True(t, ok, "forked snapshot payload should contain raw memory after preparing a compressed snapshot source")
	_, _, ok = findCompressedSnapshotMemoryFile(forkSnapshotDir)
	assert.False(t, ok, "forked snapshot payload should not retain compressed memory artifacts from the source snapshot")
}

func TestDeleteSnapshotFailsWhenForkMetadataUnreadable(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-delete-refcount-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, "snapshot-delete-refcount-src", hvType)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "delete-refcount",
	})
	require.NoError(t, err)

	badID := "snapshot-delete-bad-metadata"
	require.NoError(t, mgr.ensureDirectories(badID))
	require.NoError(t, os.WriteFile(mgr.paths.InstanceMetadata(badID), []byte("{"), 0644))

	err = mgr.DeleteSnapshot(ctx, snap.Id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal instance metadata")
}

func TestForkSnapshotHardlinksRawMemoryFile(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-fork-hardlink-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-fork-hardlink-src", hvType)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-for-fork-hardlink",
	})
	require.NoError(t, err)

	memContents := []byte("guest memory bytes for hardlink test")
	snapshotDir := mgr.paths.SnapshotGuestDir(snap.Id)
	snapshotConfigPath := filepath.Join(snapshotDir, "snapshots", "snapshot-latest", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0644))
	snapshotMem := filepath.Join(snapshotDir, "snapshots", "snapshot-latest", "memory")
	require.NoError(t, os.WriteFile(snapshotMem, memContents, 0644))

	srcInfo, err := os.Stat(snapshotMem)
	require.NoError(t, err)

	forked, err := mgr.ForkSnapshot(ctx, snap.Id, ForkSnapshotRequest{
		Name:        "snapshot-fork-hardlink",
		TargetState: StateStandby,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), forked.Id) })

	forkMem := filepath.Join(mgr.paths.InstanceSnapshotLatest(forked.Id), "memory")
	forkInfo, err := os.Stat(forkMem)
	require.NoError(t, err)
	assert.True(t, os.SameFile(srcInfo, forkInfo), "fork mem-file should share an inode with the snapshot mem-file")

	got, err := os.ReadFile(forkMem)
	require.NoError(t, err)
	assert.Equal(t, memContents, got)

	err = mgr.DeleteSnapshot(ctx, snap.Id)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidState)
}

func createStoppedSnapshotSourceFixture(t *testing.T, mgr *manager, id, name string, hvType hypervisor.Type) {
	t.Helper()
	require.NoError(t, mgr.ensureDirectories(id))

	starter, err := mgr.getVMStarter(hvType)
	require.NoError(t, err)

	now := time.Now()
	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                id,
		Name:              name,
		Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		CreatedAt:         now,
		StoppedAt:         &now,
		HypervisorType:    hvType,
		HypervisorVersion: "test",
		SocketPath:        mgr.paths.InstanceSocket(id, starter.SocketName()),
		DataDir:           mgr.paths.InstanceDir(id),
		VsockCID:          generateVsockCID(id),
		VsockSocket:       mgr.paths.InstanceSocket(id, hypervisor.VsockSocketNameForType(hvType)),
		NetworkEnabled:    false,
	}}
	require.NoError(t, mgr.saveMetadata(meta))
	require.NoError(t, os.WriteFile(mgr.paths.InstanceOverlay(id), []byte("overlay"), 0644))
	require.NoError(t, os.WriteFile(mgr.paths.InstanceConfigDisk(id), []byte("config"), 0644))
}

func createStandbySnapshotSourceFixture(t *testing.T, mgr *manager, id, name string, hvType hypervisor.Type) {
	t.Helper()
	createStoppedSnapshotSourceFixture(t, mgr, id, name, hvType)
	snapshotDir := mgr.paths.InstanceSnapshotLatest(id)
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "state"), []byte("snapshot"), 0644))
}
