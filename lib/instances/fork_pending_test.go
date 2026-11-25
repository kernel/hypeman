package instances

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func savePendingForkFixture(t *testing.T, mgr *manager, id string, pending bool, change func(*StoredMetadata)) {
	t.Helper()
	require.NoError(t, mgr.ensureDirectories(id))
	now := time.Now()
	stored := StoredMetadata{
		Id:             id,
		Name:           id,
		CreatedAt:      now,
		StoppedAt:      &now,
		HypervisorType: hypervisor.TypeCloudHypervisor,
		SocketPath:     mgr.paths.InstanceSocket(id, "cloud-hypervisor.sock"),
		DataDir:        mgr.paths.InstanceDir(id),
		ForkPending:    pending,
	}
	if change != nil {
		change(&stored)
	}
	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: stored}))
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
	require.True(t, stored.ForkPending)

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

func TestRecoverInterruptedForksDeletesOnlyMarkedForks(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	savePendingForkFixture(t, mgr, "fork-recover-interrupted", true, nil)
	savePendingForkFixture(t, mgr, "fork-recover-finished", false, nil)
	// Whether these two are interrupted forks cannot be told. A
	// self-referential symlink fails stat with ELOOP, for root too.
	require.NoError(t, mgr.ensureDirectories("fork-recover-no-stat"))
	noStat := mgr.paths.InstanceMetadata("fork-recover-no-stat")
	require.NoError(t, os.Symlink(filepath.Base(noStat), noStat))
	require.NoError(t, mgr.ensureDirectories("fork-recover-corrupt"))
	require.NoError(t, os.WriteFile(mgr.paths.InstanceMetadata("fork-recover-corrupt"), []byte("{"), 0o644))
	// A fork copy that an older binary was killed in holds its source's
	// metadata. Deleting by it would stop the source's guest.
	savePendingForkFixture(t, mgr, "fork-recover-copied-source", true, nil)
	copied, err := os.ReadFile(mgr.paths.InstanceMetadata("fork-recover-copied-source"))
	require.NoError(t, err)
	require.NoError(t, mgr.ensureDirectories("fork-recover-partial-copy"))
	require.NoError(t, os.WriteFile(mgr.paths.InstanceMetadata("fork-recover-partial-copy"), copied, 0o644))
	source, err := mgr.loadMetadata("fork-recover-copied-source")
	require.NoError(t, err)
	source.ForkPending = false
	require.NoError(t, mgr.saveMetadata(source))

	reader := otelmetric.NewManualReader()
	metrics, err := newInstanceMetrics(otelmetric.NewMeterProvider(otelmetric.WithReader(reader)).Meter("test"), nil, mgr)
	require.NoError(t, err)
	mgr.metrics = metrics

	var logs bytes.Buffer
	mgr.RecoverInterruptedForks(logger.AddToContext(ctx, slog.New(slog.NewTextHandler(&logs, nil))))

	_, err = mgr.loadMetadata("fork-recover-interrupted")
	require.ErrorIs(t, err, ErrNotFound)
	assert.NoDirExists(t, mgr.paths.InstanceDir("fork-recover-interrupted"))

	_, err = mgr.loadMetadata("fork-recover-finished")
	require.NoError(t, err)

	assert.Contains(t, logs.String(), "deleted=1 failed=0")
	for _, id := range []string{"fork-recover-no-stat", "fork-recover-corrupt"} {
		assert.DirExists(t, mgr.paths.InstanceDir(id))
		assert.Contains(t, logs.String(), id, "an instance recovery cannot read must be logged")
	}
	assert.DirExists(t, mgr.paths.InstanceDir("fork-recover-partial-copy"))
	assert.DirExists(t, mgr.paths.InstanceDir("fork-recover-copied-source"))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	counted, ok := findMetric(t, rm, "hypeman_instances_interrupted_forks_total").Data.(metricdata.Sum[int64])
	require.True(t, ok)
	byStatus := map[string]int64{}
	for _, point := range counted.DataPoints {
		byStatus[metricLabel(t, point.Attributes, "status")] = point.Value
	}
	assert.Equal(t, map[string]int64{"deleted": 1}, byStatus)
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
	for _, id := range forks {
		savePendingForkFixture(t, mgr, id, true, func(stored *StoredMetadata) { stored.Devices = []string{id} })
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

// A failed fork's request may be failing because its caller went away.
func TestFailedForkIsDeletedAfterItsRequestIsCancelled(t *testing.T) {
	mgr, _ := setupTestManager(t)
	savePendingForkFixture(t, mgr, "fork-cancelled", true, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, mgr.cleanupForkInstanceOnError(ctx, "fork-cancelled"))
	assert.NoDirExists(t, mgr.paths.InstanceDir("fork-cancelled"))
}

func TestForkCopyOmitsSourceMetadata(t *testing.T) {
	m, _ := setupTestManager(t)
	const source, fork = "fork-copy-source", "fork-copy-fork"
	createStoppedSnapshotSourceFixture(t, m, source, source, m.defaultHypervisor)
	require.NoError(t, m.cloneGuestDirectoryForFork(t.Context(), m.paths.InstanceDir(source), m.paths.InstanceDir(fork), false))
	require.FileExists(t, m.paths.InstanceOverlay(fork))
	require.NoFileExists(t, m.paths.InstanceMetadata(fork))
}

// A pending fork is either still being made or about to be deleted, so nothing
// may bring it up, snapshot it or replace it through the API.
func TestPendingForkCannotBeBroughtUpOrReplaced(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "fork-refused-source"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, mgr.defaultHypervisor)
	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{Kind: SnapshotKindStopped, Name: "fork-refused-snapshot"})
	require.NoError(t, err)
	fork, _, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{Name: "fork-refused"}, false)
	require.NoError(t, err)

	refused := func(err error) {
		t.Helper()
		require.ErrorIs(t, err, ErrInvalidState)
		require.ErrorContains(t, err, "fork whose request has not completed")
	}
	_, err = mgr.StartInstance(ctx, fork.Id, StartInstanceRequest{})
	refused(err)
	_, err = mgr.RestartInstance(ctx, fork.Id)
	refused(err)
	_, err = mgr.RestoreInstance(ctx, fork.Id)
	refused(err)
	_, err = mgr.CreateSnapshot(ctx, fork.Id, CreateSnapshotRequest{Kind: SnapshotKindStopped, Name: "fork-refused-of-fork"})
	refused(err)

	// A snapshot restore would write metadata without the mark.
	record, err := mgr.loadSnapshotRecord(snapshot.Id)
	require.NoError(t, err)
	record.Snapshot.SourceInstanceID = fork.Id
	require.NoError(t, mgr.saveSnapshotRecord(record))
	_, err = mgr.RestoreSnapshot(ctx, fork.Id, snapshot.Id, RestoreSnapshotRequest{})
	refused(err)

	stored, err := mgr.loadMetadata(fork.Id)
	require.NoError(t, err)
	assert.True(t, stored.ForkPending)
	require.NoError(t, mgr.DeleteInstance(ctx, fork.Id), "a pending fork can still be deleted")
}

// A compression job that ends just as a fork completes must not save a copy
// of the metadata it read before the mark was cleared.
func TestCompressionJobLeavesLockedInstanceMetadataAlone(t *testing.T) {
	mgr, _ := setupTestManager(t)

	id := "fork-pending-compressing"
	now := time.Now()
	policy := snapshotstore.SnapshotCompressionConfig{Enabled: true, Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd}
	savePendingForkFixture(t, mgr, id, true, func(stored *StoredMetadata) {
		stored.PendingStandbyCompression = &PendingStandbyCompression{Policy: policy, NotBefore: now}
	})
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
	assert.True(t, stored.ForkPending)
}

type forkTAPRelease struct {
	network.Manager
	ids []string
}

func (r *forkTAPRelease) ReleaseByInstanceID(_ context.Context, id string) error {
	r.ids = append(r.ids, id)
	return nil
}

// A restore creates the fork's TAP before it saves the IP and MAC, so a fork
// cut short there has a TAP that no allocation can be derived for.
func TestRecoveryReleasesTAPOfForkWithoutSavedAllocation(t *testing.T) {
	mgr, _ := setupTestManager(t)
	const fork = "fork-no-allocation"
	savePendingForkFixture(t, mgr, fork, true, func(stored *StoredMetadata) { stored.NetworkEnabled = true })
	alloc, err := mgr.networkManager.GetAllocation(t.Context(), fork)
	require.NoError(t, err)
	require.Nil(t, alloc)
	release := &forkTAPRelease{Manager: mgr.networkManager}
	mgr.networkManager = release

	mgr.RecoverInterruptedForks(t.Context())
	require.Equal(t, []string{fork}, release.ids)
	require.NoDirExists(t, mgr.paths.InstanceDir(fork))
}
