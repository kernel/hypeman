package instances

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
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
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), forked.Id) })

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
