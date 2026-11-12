package instances

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exitedProcessOwner names a process that has already exited, the way a fork
// names the API process that crashed while making it.
func exitedProcessOwner(t *testing.T) *ForkOwner {
	t.Helper()
	process := exec.Command("true")
	require.NoError(t, process.Start())
	owner := &ForkOwner{PID: process.Process.Pid, StartTime: processStartTime(process.Process.Pid), BootID: hostBootID()}
	require.NoError(t, process.Wait())
	return owner
}

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
	require.NotNil(t, stored.ForkPending)
	assert.Equal(t, os.Getpid(), stored.ForkPending.PID, "the mark must name the process making the fork")

	// A snapshot of it must not pass the mark on to what is restored from it.
	ofInterrupted, err := mgr.CreateSnapshot(ctx, interrupted.Id, CreateSnapshotRequest{Kind: SnapshotKindStopped, Name: "fork-pending-of-interrupted"})
	require.NoError(t, err)
	record, err := mgr.loadSnapshotRecord(ofInterrupted.Id)
	require.NoError(t, err)
	assert.Nil(t, record.StoredMetadata.ForkPending)

	fromInstance, err := mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-pending-from-instance"})
	require.NoError(t, err)
	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{Kind: SnapshotKindStopped, Name: "fork-pending-snapshot"})
	require.NoError(t, err)
	fromSnapshot, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{Name: "fork-pending-from-snapshot", TargetState: StateStopped})
	require.NoError(t, err)

	for _, fork := range []*Instance{fromInstance, fromSnapshot} {
		assert.Nil(t, fork.ForkPending, fork.Name)
		stored, err := mgr.loadMetadata(fork.Id)
		require.NoError(t, err)
		assert.Nil(t, stored.ForkPending, fork.Name)
	}
}

func TestRecoverInterruptedForksDeletesOnlyForksWhoseProcessIsGone(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	save := func(id string, pending *ForkOwner) {
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
	save("fork-recover-interrupted", exitedProcessOwner(t))
	// Another API process on the same data directory is still making this one.
	save("fork-recover-in-flight", currentForkOwner())
	save("fork-recover-finished", nil)

	mgr.RecoverInterruptedForks(ctx)

	_, err := mgr.loadMetadata("fork-recover-interrupted")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NoDirExists(t, mgr.paths.InstanceDir("fork-recover-interrupted"))

	inFlight, err := mgr.loadMetadata("fork-recover-in-flight")
	require.NoError(t, err, "a fork whose process is still running must be left alone")
	assert.NotNil(t, inFlight.ForkPending)

	_, err = mgr.loadMetadata("fork-recover-finished")
	require.NoError(t, err)
}

func TestForkOwnerIsGoneOnlyWhenItsProcessIs(t *testing.T) {
	assert.False(t, currentForkOwner().gone(), "a running process")
	assert.True(t, exitedProcessOwner(t).gone(), "an exited process")
	// Without a start time or boot ID a live PID is all there is to go by.
	assert.False(t, ForkOwner{PID: os.Getpid()}.gone(), "a live PID with nothing else recorded")
}
