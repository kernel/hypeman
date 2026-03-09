package instances

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	snapshottest "github.com/kernel/hypeman/lib/snapshot/testsupport"
	"github.com/kernel/hypeman/lib/system"
	"github.com/stretchr/testify/require"
)

type snapshotScenarioConfig struct {
	hypervisor hypervisor.Type
	sourceName string
	snapshot   string
	forkName   string
	onError    func()
}

func runStandbySnapshotScenario(t *testing.T, mgr *manager, tmpDir string, cfg snapshotScenarioConfig) {
	t.Helper()

	ctx := context.Background()
	p := paths.New(tmpDir)

	onErr := func() {}
	if cfg.onError != nil {
		onErr = cfg.onError
	}
	requireNoErr := func(err error) {
		t.Helper()
		if err != nil {
			onErr()
		}
		require.NoError(t, err)
	}
	imageManager, err := images.NewManager(p, 1, nil)
	requireNoErr(err)
	snapshottest.EnsureImageReady(t, ctx, p, imageManager, integrationTestImageRef(t, "docker.io/library/alpine:latest"))

	systemManager := system.NewManager(p)
	requireNoErr(systemManager.EnsureSystemFiles(ctx))

	source, err := mgr.CreateInstance(ctx, CreateInstanceRequest{
		Name:           cfg.sourceName,
		Image:          integrationTestImageRef(t, "docker.io/library/alpine:latest"),
		Size:           1024 * 1024 * 1024,
		OverlaySize:    10 * 1024 * 1024 * 1024,
		Vcpus:          1,
		NetworkEnabled: false,
		Hypervisor:     cfg.hypervisor,
		Cmd:            []string{"sleep", "infinity"},
	})
	requireNoErr(err)

	sourceID := source.Id
	sourceDeleted := false
	t.Cleanup(func() {
		if !sourceDeleted {
			_ = mgr.DeleteInstance(context.Background(), sourceID)
		}
	})

	_, err = mgr.StandbyInstance(ctx, sourceID)
	requireNoErr(err)

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: cfg.snapshot,
	})
	requireNoErr(err)
	require.Equal(t, SnapshotKindStandby, snapshot.Kind)
	require.Equal(t, sourceID, snapshot.SourceInstanceID)

	filter := &ListSnapshotsFilter{SourceInstanceID: &sourceID}
	snapshots, err := mgr.ListSnapshots(ctx, filter)
	requireNoErr(err)
	require.NotEmpty(t, snapshots)

	gotSnapshot, err := mgr.GetSnapshot(ctx, snapshot.Id)
	requireNoErr(err)
	require.Equal(t, snapshot.Id, gotSnapshot.Id)

	requireNoErr(mgr.DeleteInstance(ctx, sourceID))
	sourceDeleted = true

	_, err = mgr.GetSnapshot(ctx, snapshot.Id)
	requireNoErr(err)

	forked, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        cfg.forkName,
		TargetState: StateStandby,
	})
	requireNoErr(err)
	require.Equal(t, StateStandby, forked.State)

	forkID := forked.Id
	t.Cleanup(func() { _ = mgr.DeleteInstance(context.Background(), forkID) })
	currentFork, err := mgr.GetInstance(ctx, forkID)
	requireNoErr(err)
	require.Equal(t, StateStandby, currentFork.State)
}
