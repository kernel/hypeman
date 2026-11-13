package instances

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
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
	// Whether these two are interrupted forks cannot be told. A
	// self-referential symlink fails stat with ELOOP, for root too.
	require.NoError(t, mgr.ensureDirectories("fork-recover-no-stat"))
	noStat := mgr.paths.InstanceMetadata("fork-recover-no-stat")
	require.NoError(t, os.Symlink(filepath.Base(noStat), noStat))
	require.NoError(t, mgr.ensureDirectories("fork-recover-corrupt"))
	require.NoError(t, os.WriteFile(mgr.paths.InstanceMetadata("fork-recover-corrupt"), []byte("{"), 0o644))

	var logs bytes.Buffer
	mgr.RecoverInterruptedForks(logger.AddToContext(ctx, slog.New(slog.NewTextHandler(&logs, nil))))

	_, err := mgr.loadMetadata("fork-recover-interrupted")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NoDirExists(t, mgr.paths.InstanceDir("fork-recover-interrupted"))

	inFlight, err := mgr.loadMetadata("fork-recover-in-flight")
	require.NoError(t, err, "a fork whose process is still running must be left alone")
	assert.NotNil(t, inFlight.ForkPending)

	_, err = mgr.loadMetadata("fork-recover-finished")
	require.NoError(t, err)

	for _, id := range []string{"fork-recover-no-stat", "fork-recover-corrupt"} {
		assert.DirExists(t, mgr.paths.InstanceDir(id))
		assert.Contains(t, logs.String(), id, "an instance recovery cannot read must be logged")
	}
}

// A compression job that ends just as a fork completes must not save a copy
// of the metadata it read before the mark was cleared.
func TestCompressionJobLeavesLockedInstanceMetadataAlone(t *testing.T) {
	mgr, _ := setupTestManager(t)

	id := "fork-pending-compressing"
	require.NoError(t, mgr.ensureDirectories(id))
	now := time.Now()
	policy := snapshotstore.SnapshotCompressionConfig{Enabled: true, Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd}
	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:                        id,
		Name:                      id,
		CreatedAt:                 now,
		StoppedAt:                 &now,
		HypervisorType:            hypervisor.TypeCloudHypervisor,
		DataDir:                   mgr.paths.InstanceDir(id),
		ForkPending:               currentForkOwner(),
		PendingStandbyCompression: &PendingStandbyCompression{Policy: policy, NotBefore: now},
	}}))
	target := compressionTarget{
		Key:         mgr.snapshotJobKeyForInstance(id),
		OwnerID:     id,
		SnapshotDir: t.TempDir(), // nothing to compress: the job ends at once
		Source:      snapshotCompressionSourceStandby,
		Policy:      policy,
	}
	runJob := func() {
		mgr.startCompressionJob(context.Background(), target)
		require.NoError(t, mgr.waitCompressionJobContext(context.Background(), target.Key))
	}

	// completeFork holds this lock while it clears the mark.
	lock := mgr.getInstanceLock(id)
	lock.Lock()
	runJob()
	stored, err := mgr.loadMetadata(id)
	lock.Unlock()
	require.NoError(t, err)
	assert.NotNil(t, stored.PendingStandbyCompression, "the job wrote metadata while the instance was locked")

	runJob()
	stored, err = mgr.loadMetadata(id)
	require.NoError(t, err)
	assert.Nil(t, stored.PendingStandbyCompression)
	assert.NotNil(t, stored.ForkPending)
}

// detachGateDeviceManager holds every device detach until release is closed,
// reporting each one that arrives.
type detachGateDeviceManager struct {
	devices.Manager
	arrived chan string
	release chan struct{}
}

func (m detachGateDeviceManager) MarkDetached(ctx context.Context, deviceID string) error {
	m.arrived <- deviceID
	<-m.release
	return nil
}

func (m detachGateDeviceManager) UnbindFromVFIO(ctx context.Context, id string) error {
	return nil
}

// A restart that interrupts a burst of forks must not delete them one at a
// time: the API does not listen until recovery returns.
func TestRecoverInterruptedForksDeletesConcurrently(t *testing.T) {
	mgr, _ := setupTestManager(t)

	// Each fork names itself as its device, so the gate sees which delete it holds.
	forks := []string{"fork-recover-burst-1", "fork-recover-burst-2", "fork-recover-burst-3"}
	gate := detachGateDeviceManager{arrived: make(chan string, len(forks)), release: make(chan struct{})}
	mgr.deviceManager = gate

	owner := exitedProcessOwner(t)
	for _, id := range forks {
		require.NoError(t, mgr.ensureDirectories(id))
		now := time.Now()
		require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
			Id:             id,
			Name:           id,
			CreatedAt:      now,
			StoppedAt:      &now,
			HypervisorType: hypervisor.TypeCloudHypervisor,
			SocketPath:     mgr.paths.InstanceSocket(id, "cloud-hypervisor.sock"),
			DataDir:        mgr.paths.InstanceDir(id),
			Devices:        []string{id},
			ForkPending:    owner,
		}}))
	}

	recovered := make(chan struct{})
	go func() {
		defer close(recovered)
		mgr.RecoverInterruptedForks(context.Background())
	}()

	var inFlight []string
	for range forks {
		select {
		case id := <-gate.arrived:
			inFlight = append(inFlight, id)
		case <-time.After(5 * time.Second):
			close(gate.release)
			<-recovered
			t.Fatalf("deletes of %v did not overlap: only %v started", forks, inFlight)
		}
	}
	close(gate.release)
	<-recovered

	assert.ElementsMatch(t, forks, inFlight)
	for _, id := range forks {
		assert.NoDirExists(t, mgr.paths.InstanceDir(id))
	}
}

// The caller of a running fork gets an error when its source does not come
// back, so the fork must not stay behind under the name it asked for.
func TestRunningForkIsDeletedWhenItsSourceDoesNotComeBack(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	// A stopped source has nothing to restore from.
	sourceID := "fork-source-not-restored"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, mgr.defaultHypervisor)
	forked, _, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{Name: "fork-source-not-restored-copy"}, false)
	require.NoError(t, err)

	err = mgr.restoreSourceAfterRunningFork(ctx, sourceID, forked, nil)
	require.ErrorContains(t, err, "restore source instance after fork")

	_, err = mgr.loadMetadata(forked.Id)
	require.ErrorIs(t, err, ErrNotFound)
	assert.NoDirExists(t, mgr.paths.InstanceDir(forked.Id))
	_, err = mgr.loadMetadata(sourceID)
	require.NoError(t, err)
}

func TestForkOwnerIsGoneOnlyWhenItsProcessIs(t *testing.T) {
	assert.False(t, currentForkOwner().gone(), "a running process")
	assert.True(t, exitedProcessOwner(t).gone(), "an exited process")
	// Without a start time or boot ID a live PID is all there is to go by.
	assert.False(t, ForkOwner{PID: os.Getpid()}.gone(), "a live PID with nothing else recorded")
}
