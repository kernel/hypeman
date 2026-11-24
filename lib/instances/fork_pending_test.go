package instances

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/forkvm"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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
	// A restore cut short leaves the payload's copy as the instance's metadata.
	data, err := os.ReadFile(filepath.Join(mgr.paths.SnapshotGuestDir(ofInterrupted.Id), "metadata.json"))
	require.NoError(t, err)
	var copied metadata
	require.NoError(t, json.Unmarshal(data, &copied))
	assert.Equal(t, interrupted.Id, copied.Id)
	assert.Nil(t, copied.ForkPending)

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

	reader := otelmetric.NewManualReader()
	metrics, err := newInstanceMetrics(otelmetric.NewMeterProvider(otelmetric.WithReader(reader)).Meter("test"), nil, mgr)
	require.NoError(t, err)
	mgr.metrics = metrics

	var logs bytes.Buffer
	mgr.RecoverInterruptedForks(logger.AddToContext(ctx, slog.New(slog.NewTextHandler(&logs, nil))))

	_, err = mgr.loadMetadata("fork-recover-interrupted")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NoDirExists(t, mgr.paths.InstanceDir("fork-recover-interrupted"))

	inFlight, err := mgr.loadMetadata("fork-recover-in-flight")
	require.NoError(t, err, "a fork whose process is still running must be left alone")
	assert.NotNil(t, inFlight.ForkPending)

	_, err = mgr.loadMetadata("fork-recover-finished")
	require.NoError(t, err)

	assert.Contains(t, logs.String(), "deleted=1 failed=0 owner_running=1")
	for _, id := range []string{"fork-recover-no-stat", "fork-recover-corrupt"} {
		assert.DirExists(t, mgr.paths.InstanceDir(id))
		assert.Contains(t, logs.String(), id, "an instance recovery cannot read must be logged")
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	counted, ok := findMetric(t, rm, "hypeman_instances_interrupted_forks_total").Data.(metricdata.Sum[int64])
	require.True(t, ok)
	byStatus := map[string]int64{}
	for _, point := range counted.DataPoints {
		byStatus[metricLabel(t, point.Attributes, "status")] = point.Value
	}
	assert.Equal(t, map[string]int64{"deleted": 1, "owner_running": 1}, byStatus)
}

// A directory sync can take as long as the disk does. A restore waiting to
// alias a source directory must not wait on it: while it waits, no metadata
// on the host can be read.
func TestDurableMetadataSaveSyncsWithoutAliasLock(t *testing.T) {
	mgr, _ := setupTestManager(t)

	id := "fork-sync-unlocked"
	createStoppedSnapshotSourceFixture(t, mgr, id, id, mgr.defaultHypervisor)
	meta, err := mgr.loadMetadata(id)
	require.NoError(t, err)

	aliased := false
	mgr.syncDirFn = func(dir *os.File) error {
		taken := make(chan struct{})
		go func() {
			hypervisor.LockSnapshotSourceAliasMutation()()
			close(taken)
		}()
		select {
		case <-taken:
			aliased = true
		case <-time.After(5 * time.Second):
		}
		return dir.Sync()
	}

	require.NoError(t, mgr.saveMetadataDurably(meta))
	assert.True(t, aliased, "the alias lock was held across the directory sync")
}

// Clearing the mark is synced to disk, so a host crash cannot undo it after
// the caller got a success. If it cannot be synced the request fails and the
// fork is deleted. A fork that is not deleted is still marked.
func TestForkFailsWhenClearingItsMarkCannotBeSynced(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-sync-source"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, mgr.defaultHypervisor)
	var synced string
	mgr.syncDirFn = func(dir *os.File) error {
		synced = dir.Name()
		return errors.New("sync failed")
	}

	_, err := mgr.ForkInstance(ctx, sourceID, ForkInstanceRequest{Name: "fork-sync-copy"})
	require.ErrorContains(t, err, "save completed fork metadata")

	require.NotEmpty(t, synced, "the fork's directory must be synced")
	assert.NoDirExists(t, synced)

	// The cleared metadata is already in place when the sync fails.
	kept, _, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{Name: "fork-sync-kept"}, false)
	require.NoError(t, err)
	require.ErrorContains(t, mgr.completeFork(kept), "save completed fork metadata")
	stored, err := mgr.loadMetadata(kept.Id)
	require.NoError(t, err)
	assert.Equal(t, currentForkOwner(), stored.ForkPending)
}

// contextRecordingDeviceManager records whether the context a device release
// ran with had been cancelled.
type contextRecordingDeviceManager struct {
	devices.Manager
	released chan error
}

func (m contextRecordingDeviceManager) ReleaseInstanceDevice(ctx context.Context, deviceID, instanceID string) error {
	m.released <- ctx.Err()
	return nil
}

// A fork request often fails because its caller went away, which cancels its
// context. The fork must be deleted all the same.
func TestFailedForkIsDeletedAfterItsRequestIsCancelled(t *testing.T) {
	mgr, _ := setupTestManager(t)

	recorder := contextRecordingDeviceManager{released: make(chan error, 1)}
	mgr.deviceManager = recorder

	id := "fork-cancelled-request"
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
		ForkPending:    currentForkOwner(),
	}}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cause := errors.New("caller went away")
	require.Equal(t, cause, mgr.discardFork(ctx, id, cause))

	require.NoError(t, <-recorder.released, "the delete ran with the cancelled request context")
	assert.NoDirExists(t, mgr.paths.InstanceDir(id))
}

// A copy cut short before the fork saved its own metadata leaves the source's
// in the fork's directory. Recovery must not delete by it: that metadata names
// the source's guest and what the source holds.
func TestRecoverInterruptedForksLeavesDirectoryHoldingAnotherInstancesMetadata(t *testing.T) {
	mgr, _ := setupTestManager(t)

	// The source names itself as its device, so the gate sees a delete that goes by its metadata.
	gate := detachGateDeviceManager{arrived: make(chan string, 1), release: make(chan struct{})}
	close(gate.release)
	mgr.deviceManager = gate

	sourceID := "fork-recover-copied-source"
	require.NoError(t, mgr.ensureDirectories(sourceID))
	now := time.Now()
	source := &metadata{StoredMetadata: StoredMetadata{
		Id:             sourceID,
		Name:           sourceID,
		CreatedAt:      now,
		StoppedAt:      &now,
		HypervisorType: hypervisor.TypeCloudHypervisor,
		SocketPath:     mgr.paths.InstanceSocket(sourceID, "cloud-hypervisor.sock"),
		DataDir:        mgr.paths.InstanceDir(sourceID),
		Devices:        []string{sourceID},
		ForkPending:    exitedProcessOwner(t),
	}}
	require.NoError(t, mgr.saveMetadata(source))
	// The source was itself a fork still pending when it was copied, and has finished since.
	copyID := "fork-recover-copied"
	require.NoError(t, forkvm.CopyGuestDirectory(mgr.paths.InstanceDir(sourceID), mgr.paths.InstanceDir(copyID)))
	source.ForkPending = nil
	require.NoError(t, mgr.saveMetadata(source))

	var logs bytes.Buffer
	mgr.RecoverInterruptedForks(logger.AddToContext(context.Background(), slog.New(slog.NewTextHandler(&logs, nil))))

	assert.Empty(t, gate.arrived, "recovery detached the source's device")
	assert.DirExists(t, mgr.paths.InstanceDir(copyID))
	assert.Contains(t, logs.String(), "deleted=0 failed=0 owner_running=0")
}

// The process making a fork can finish it and exit after recovery read its
// mark. Recovery must go by the mark as it is once that process is gone.
func TestRecoverInterruptedForksKeepsForkFinishedSinceItsMarkWasRead(t *testing.T) {
	mgr, _ := setupTestManager(t)

	id := "fork-recover-just-finished"
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
		ForkPending:    exitedProcessOwner(t),
	}}))
	mgr.forkOwnerGoneFn = func(owner ForkOwner) bool {
		// Recovery has read the mark. The owner now clears it and exits.
		stored, err := mgr.loadMetadata(id)
		require.NoError(t, err)
		stored.ForkPending = nil
		require.NoError(t, mgr.saveMetadata(stored))
		return owner.gone()
	}

	mgr.RecoverInterruptedForks(context.Background())

	stored, err := mgr.loadMetadata(id)
	require.NoError(t, err, "a fork that finished must not be deleted")
	assert.Nil(t, stored.ForkPending)
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

func (m detachGateDeviceManager) ReleaseInstanceDevice(ctx context.Context, deviceID, instanceID string) error {
	m.arrived <- deviceID
	<-m.release
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

type forkRestoreAdmission struct {
	recordingResourceValidator
	reserve func(context.Context) error
}

func (v forkRestoreAdmission) ReserveAllocation(ctx context.Context, _ string, _ int, _, _, _, _, _ int64, _ bool) error {
	return v.reserve(ctx)
}

func TestRunningForkRestoresSourceOnCancelOrCapacityFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		wantAttempts int
	}{
		{"caller disconnected", context.Canceled, 1},
		{"only one guest fits", ErrInsufficientResources, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			m, source := newLifecycleNoopManagerWithInstance(t, StateStandby, now)
			m.vmStarters = map[hypervisor.Type]hypervisor.VMStarter{lifecycleNoopHypervisorType: concurrentForkPrepareTestStarter{}}
			meta, err := m.loadMetadata(source)
			require.NoError(t, err)
			meta.SkipGuestAgent = true
			meta.ProgramStartedAt = &now
			require.NoError(t, m.saveMetadata(meta))
			m.imageManager = readyFixtureImageManager{name: meta.Image}
			fork, _, err := m.forkInstanceFromStoppedOrStandby(t.Context(), source, ForkInstanceRequest{Name: "child"}, false)
			require.NoError(t, err)
			writeLifecycleNoopSocket(t, fork.SocketPath, hypervisor.StateRunning)
			t.Cleanup(func() { lifecycleNoopHypervisorStates.Delete(fork.SocketPath) })

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.err == context.Canceled {
				cancel()
			}
			attempts := 0
			m.resourceValidator = &forkRestoreAdmission{reserve: func(restoreCtx context.Context) error {
				attempts++
				_, bounded := restoreCtx.Deadline()
				require.True(t, bounded)
				require.NoError(t, restoreCtx.Err())
				if tc.err == ErrInsufficientResources {
					if _, err := os.Stat(fork.SocketPath); err == nil {
						return errors.New("memory occupied by child")
					}
				}
				return nil
			}}

			err = m.restoreSourceAfterRunningFork(ctx, source, fork, nil)
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.wantAttempts, attempts)
			require.NoDirExists(t, fork.DataDir)
			got, err := m.currentInstanceWithoutHydration(t.Context(), source)
			require.NoError(t, err)
			require.Equal(t, StateRunning, got.State)
		})
	}
}

func TestForkOwnerIsGoneOnlyWhenItsProcessIs(t *testing.T) {
	assert.False(t, currentForkOwner().gone(), "a running process")
	assert.True(t, exitedProcessOwner(t).gone(), "an exited process")
	// Without a start time or boot ID a live PID is all there is to go by.
	assert.False(t, ForkOwner{PID: os.Getpid()}.gone(), "a live PID with nothing else recorded")
}
