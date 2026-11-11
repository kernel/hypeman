package instances

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkIsPendingOnlyUntilItsRequestSucceeds(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-pending-source"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, mgr.defaultHypervisor)

	// What a crash between saving the fork and finishing it leaves behind.
	interrupted, _, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{Name: "fork-pending-interrupted"}, false)
	require.NoError(t, err)
	stored, err := mgr.loadMetadata(interrupted.Id)
	require.NoError(t, err)
	assert.True(t, stored.ForkPending)

	// A snapshot of it must not pass the mark on to what is restored from it.
	ofInterrupted, err := mgr.CreateSnapshot(ctx, interrupted.Id, CreateSnapshotRequest{Kind: SnapshotKindStopped, Name: "fork-pending-of-interrupted"})
	require.NoError(t, err)
	record, err := mgr.loadSnapshotRecord(ofInterrupted.Id)
	require.NoError(t, err)
	assert.False(t, record.StoredMetadata.ForkPending)

	fromInstance, err := mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-pending-from-instance"})
	require.NoError(t, err)
	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{Kind: SnapshotKindStopped, Name: "fork-pending-snapshot"})
	require.NoError(t, err)
	fromSnapshot, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{Name: "fork-pending-from-snapshot", TargetState: StateStopped})
	require.NoError(t, err)

	for _, fork := range []*Instance{fromInstance, fromSnapshot} {
		assert.False(t, fork.ForkPending, fork.Name)
		stored, err := mgr.loadMetadata(fork.Id)
		require.NoError(t, err)
		assert.False(t, stored.ForkPending, fork.Name)
	}
}

func TestRecoverInterruptedForksDeletesOnlyPendingForks(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	save := func(id string, pending bool) {
		require.NoError(t, mgr.ensureDirectories(id))
		now := time.Now()
		require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id:                id,
			Name:              id,
			Image:             integrationTestImageRef(t, "docker.io/library/alpine:latest"),
			CreatedAt:         now,
			StoppedAt:         &now,
			HypervisorType:    hypervisor.TypeCloudHypervisor,
			HypervisorVersion: "test",
			SocketPath:        mgr.paths.InstanceSocket(id, "cloud-hypervisor.sock"),
			DataDir:           mgr.paths.InstanceDir(id),
			VsockCID:          generateVsockCID(id),
			VsockSocket:       mgr.paths.InstanceVsockSocket(id),
			ForkPending:       pending,
		}}))
	}
	save("fork-recover-interrupted", true)
	save("fork-recover-finished", false)

	mgr.RecoverInterruptedForks(ctx)

	_, err := mgr.loadMetadata("fork-recover-interrupted")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NoDirExists(t, mgr.paths.InstanceDir("fork-recover-interrupted"))
	_, err = mgr.loadMetadata("fork-recover-finished")
	require.NoError(t, err)
}
