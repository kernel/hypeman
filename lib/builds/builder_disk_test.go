package builds

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/builders"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prepareBuildOnDisk writes the metadata, source, and config files
// executeBuild expects, without going through CreateBuild (which would start
// a background build goroutine via the queue).
func prepareBuildOnDisk(t *testing.T, mgr *manager, id string, req CreateBuildRequest) {
	t.Helper()

	meta := &buildMetadata{
		ID:        id,
		Status:    StatusQueued,
		Request:   &req,
		CreatedAt: time.Now(),
	}
	require.NoError(t, writeMetadata(mgr.paths, meta))
	require.NoError(t, mgr.storeSource(id, []byte("fake-tarball-data")))

	config := &BuildConfig{
		JobID:          id,
		RegistryURL:    mgr.config.RegistryURL,
		SourcePath:     "/src",
		Dockerfile:     req.Dockerfile,
		TimeoutSeconds: 600,
		NetworkMode:    "isolated",
	}
	require.NoError(t, writeBuildConfig(mgr.paths, id, config))
}

// stoppedBuilderInstance returns a CreateInstance hook that records the
// request and reports the instance as already stopped, so waitForResult
// returns quickly with a failed result.
func stoppedBuilderInstance(instanceMgr *mockInstanceManager, gotReq *instances.CreateInstanceRequest) {
	instanceMgr.createFunc = func(ctx context.Context, req instances.CreateInstanceRequest) (*instances.Instance, error) {
		if gotReq != nil {
			*gotReq = req
		}
		inst := &instances.Instance{
			StoredMetadata: instances.StoredMetadata{
				Id:   "inst-" + req.Name,
				Name: req.Name,
			},
			State: instances.StateStopped,
		}
		instanceMgr.instances[inst.Id] = inst
		return inst, nil
	}
}

// TestExecuteBuild_BuilderDiskLifecycle runs executeBuild against a builder
// and verifies the disk is attached at /var/lib/buildkit read-write, the
// system-mount carveout is set, and the builder is released (last_used_at
// stamped) even though the build fails.
func TestExecuteBuild_BuilderDiskLifecycle(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	builder, err := mgr.builderManager.CreateBuilder(ctx, builders.CreateBuilderRequest{DiskSizeGb: 20})
	require.NoError(t, err)

	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		BuilderID:  builder.ID,
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	var createReq instances.CreateInstanceRequest
	stoppedBuilderInstance(instanceMgr, &createReq)

	policy := DefaultBuildPolicy()
	result, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Success) // instance stopped before reporting

	// Source, config, and the builder disk at the exact BuildKit root path.
	require.Len(t, createReq.Volumes, 3)
	diskAtt := createReq.Volumes[2]
	assert.Equal(t, builders.DiskVolumeID(builder.ID), diskAtt.VolumeID)
	assert.Equal(t, "/var/lib/buildkit", diskAtt.MountPath)
	assert.False(t, diskAtt.Readonly)
	assert.True(t, createReq.AllowSystemVolumeMounts)
	assert.Equal(t, []string{"/var/lib/buildkit"}, createReq.SystemVolumeMountPaths)

	// The builder was released after the build and last_used_at was stamped
	// even though the build failed.
	got, err := mgr.builderManager.GetBuilder(ctx, builder.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt, "last_used_at must be stamped even on failed builds")

	// The disk is retained, still attached to nothing, and the source/config
	// volumes were cleaned up.
	_, err = volumeMgr.GetVolume(ctx, "build-source-build-1")
	assert.ErrorIs(t, err, volumes.ErrNotFound)
	_, err = volumeMgr.GetVolume(ctx, "build-config-build-1")
	assert.ErrorIs(t, err, volumes.ErrNotFound)
}

// TestExecuteBuild_NoBuilderUnchanged pins the default path: a build without
// builder_id attaches exactly the source and config volumes, without the
// system-mount carveout, exactly as on main.
func TestExecuteBuild_NoBuilderUnchanged(t *testing.T) {
	mgr, instanceMgr, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	var createReq instances.CreateInstanceRequest
	stoppedBuilderInstance(instanceMgr, &createReq)

	policy := DefaultBuildPolicy()
	_, err := mgr.executeBuild(context.Background(), "build-1", req, &policy)
	require.NoError(t, err)

	expected := []instances.VolumeAttachment{
		{VolumeID: "build-source-build-1", MountPath: "/src", Readonly: false},
		{VolumeID: "build-config-build-1", MountPath: "/config", Readonly: true},
	}
	assert.Equal(t, expected, createReq.Volumes)
	assert.False(t, createReq.AllowSystemVolumeMounts)
	assert.Empty(t, createReq.SystemVolumeMountPaths)
}

// TestCreateBuild_BuilderNotFound verifies builds targeting a missing
// builder fail at create time.
func TestCreateBuild_BuilderNotFound(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	_, err := mgr.CreateBuild(context.Background(), CreateBuildRequest{
		Dockerfile: "FROM alpine",
		BuilderID:  "no-such-builder",
	}, []byte("src"))
	require.ErrorIs(t, err, builders.ErrNotFound)
}

// TestCreateBuild_GCBoundsWrittenForBuilder verifies the host computes
// BuildKit GC bounds from the builder disk size into the guest config, and
// leaves them zero for tmpfs-backed builds.
func TestCreateBuild_GCBoundsWrittenForBuilder(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	builder, err := mgr.builderManager.CreateBuilder(ctx, builders.CreateBuilderRequest{DiskSizeGb: 20})
	require.NoError(t, err)

	build, err := mgr.CreateBuild(ctx, CreateBuildRequest{
		Dockerfile: "FROM alpine",
		BuilderID:  builder.ID,
	}, []byte("src"))
	require.NoError(t, err)

	config, err := readBuildConfig(mgr.paths, build.ID)
	require.NoError(t, err)
	gb := int64(1024 * 1024 * 1024)
	assert.Equal(t, 20*gb*7/10, config.CacheGCReservedBytes)
	assert.Equal(t, 20*gb*9/10, config.CacheGCMaxUsedBytes)

	// Build response carries the builder ID.
	require.NotNil(t, build.BuilderID)
	assert.Equal(t, builder.ID, *build.BuilderID)

	// Without a builder the bounds stay zero (tmpfs path).
	build2, err := mgr.CreateBuild(ctx, CreateBuildRequest{
		Dockerfile: "FROM alpine",
	}, []byte("src"))
	require.NoError(t, err)
	config2, err := readBuildConfig(mgr.paths, build2.ID)
	require.NoError(t, err)
	assert.Zero(t, config2.CacheGCReservedBytes)
	assert.Zero(t, config2.CacheGCMaxUsedBytes)
	assert.Nil(t, build2.BuilderID)
}

// TestExecuteBuild_StaleHolderRecovery verifies that when a crashed build's
// VM still holds the builder disk, acquiring the builder for the next build
// deletes the stale holder and detaches surviving records, so the build
// proceeds. The builds and builders managers share one volume manager,
// matching production wiring.
func TestExecuteBuild_StaleHolderRecovery(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	builder, err := mgr.builderManager.CreateBuilder(ctx, builders.CreateBuilderRequest{DiskSizeGb: 20})
	require.NoError(t, err)

	req := CreateBuildRequest{
		Dockerfile: "FROM alpine",
		BuilderID:  builder.ID,
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	// A stale builder VM from a crashed build holds the disk, alongside an
	// orphan record whose instance is already gone. DeleteInstance's volume
	// detach is warn-only in production, so records like these leak.
	stale := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-stale", Name: "builder-build-0"},
		State:          instances.StateRunning,
	}
	instanceMgr.instances[stale.Id] = stale
	volID := builders.DiskVolumeID(builder.ID)
	require.NoError(t, volumeMgr.AttachVolume(ctx, volID, volumes.AttachVolumeRequest{
		InstanceID: stale.Id, MountPath: "/var/lib/buildkit",
	}))
	require.NoError(t, volumeMgr.AttachVolume(ctx, volID, volumes.AttachVolumeRequest{
		InstanceID: "inst-gone", MountPath: "/var/lib/buildkit",
	}))

	deleted := []string{}
	instanceMgr.deleteFunc = func(ctx context.Context, id string) error {
		deleted = append(deleted, id)
		delete(instanceMgr.instances, id)
		return nil
	}
	stoppedBuilderInstance(instanceMgr, nil)

	policy := DefaultBuildPolicy()
	_, err = mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err, "acquire must clear the stale holder instead of failing the build")

	assert.Contains(t, deleted, stale.Id)
	vol, err := volumeMgr.GetVolume(ctx, volID)
	require.NoError(t, err)
	assert.Empty(t, vol.Attachments, "surviving attachment records must be detached")

	// The builder was released after the build.
	got, err := mgr.builderManager.GetBuilder(ctx, builder.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.LastUsedAt)
}

// TestExecuteBuild_RecoveryFailsLoudly verifies the build fails when the
// stale holder cannot be removed.
func TestExecuteBuild_RecoveryFailsLoudly(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	builder, err := mgr.builderManager.CreateBuilder(ctx, builders.CreateBuilderRequest{DiskSizeGb: 20})
	require.NoError(t, err)

	req := CreateBuildRequest{
		Dockerfile: "FROM alpine",
		BuilderID:  builder.ID,
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	// Live instance that cannot be deleted holds the disk.
	stale := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-stuck", Name: "builder-build-0"},
		State:          instances.StateRunning,
	}
	instanceMgr.instances[stale.Id] = stale
	volID := builders.DiskVolumeID(builder.ID)
	require.NoError(t, volumeMgr.AttachVolume(ctx, volID, volumes.AttachVolumeRequest{
		InstanceID: stale.Id, MountPath: "/var/lib/buildkit",
	}))
	instanceMgr.deleteFunc = func(ctx context.Context, id string) error {
		return errors.New("hypervisor unreachable")
	}

	policy := DefaultBuildPolicy()
	_, err = mgr.executeBuild(ctx, "build-1", req, &policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "holding builder disk")
}

// TestBuilderQueueSerialization verifies builds for one builder are
// serialized while other builds proceed, and that the queue reports
// active/queued state per builder.
func TestBuilderQueueSerialization(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	release := make(chan struct{})
	started := make(chan string, 3)
	req := CreateBuildRequest{BuilderID: "builder-a"}

	pos1 := mgr.queue.EnqueueSerial("build-1", req, mgr.buildSerialKey(req), func() {
		started <- "build-1"
		<-release
	})
	pos2 := mgr.queue.EnqueueSerial("build-2", req, mgr.buildSerialKey(req), func() {
		started <- "build-2"
		<-release
	})
	assert.Equal(t, 0, pos1)
	assert.Equal(t, 1, pos2)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first build did not start")
	}
	assert.Equal(t, 1, mgr.queue.ActiveCount(), "same-builder build must not occupy a slot while serialized")

	// Queue introspection used by the builders API.
	active := mgr.ActiveBuildForBuilder("builder-a")
	require.NotNil(t, active)
	assert.Equal(t, "build-1", *active)
	assert.Equal(t, []string{"build-2"}, mgr.QueuedBuildsForBuilder("builder-a"))
	assert.True(t, mgr.BuilderHasBuilds("builder-a"))
	assert.False(t, mgr.BuilderHasBuilds("builder-b"))

	// Releasing the serial key (after VM execution ends) lets the next
	// same-builder build start while build-1 finishes post-build work.
	mgr.queue.ReleaseSerialKey("build-1")
	select {
	case id := <-started:
		assert.Equal(t, "build-2", id)
	case <-time.After(2 * time.Second):
		t.Fatal("same-builder build did not start after serial key release")
	}

	close(release)
}

// TestBuilderDeleteAndPruneBlockedByQueuedBuilds verifies the activity
// checker wiring: delete and prune return ErrInUse while builds for the
// builder are queued or running, not only while acquired.
func TestBuilderDeleteAndPruneBlockedByQueuedBuilds(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	builder, err := mgr.builderManager.CreateBuilder(ctx, builders.CreateBuilderRequest{})
	require.NoError(t, err)
	mgr.builderManager.SetBuildActivityChecker(mgr.BuilderHasBuilds)

	release := make(chan struct{})
	defer close(release)
	req := CreateBuildRequest{BuilderID: builder.ID}
	mgr.queue.EnqueueSerial("build-1", req, builder.ID, func() { <-release })
	mgr.queue.EnqueueSerial("build-2", req, builder.ID, func() { <-release })

	err = mgr.builderManager.DeleteBuilder(ctx, builder.ID)
	assert.ErrorIs(t, err, builders.ErrInUse, "delete must be blocked by a running build")
	_, err = mgr.builderManager.ResetDisk(ctx, builder.ID)
	assert.ErrorIs(t, err, builders.ErrInUse, "prune must be blocked by a running build")

	// Finish build-1; build-2 starts. Still blocked.
	mgr.queue.ReleaseSerialKey("build-1")
	mgr.queue.MarkComplete("build-1")
	time.Sleep(50 * time.Millisecond)
	err = mgr.builderManager.DeleteBuilder(ctx, builder.ID)
	assert.ErrorIs(t, err, builders.ErrInUse, "delete must be blocked by a queued-then-running build")
}

// TestExecuteBuild_ReleaseRetriedOnPersistFailure fails the builder
// metadata write for the first release attempts and verifies the deferred
// retry still releases the builder: without it the hold would leak and
// every later build for the builder would fail AcquireForBuild until
// restart.
func TestExecuteBuild_ReleaseRetriedOnPersistFailure(t *testing.T) {
	mgr, instanceMgr, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	builder, err := mgr.builderManager.CreateBuilder(ctx, builders.CreateBuilderRequest{DiskSizeGb: 20})
	require.NoError(t, err)

	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		BuilderID:  builder.ID,
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)
	stoppedBuilderInstance(instanceMgr, nil)

	// A directory occupying the temp path fails the atomic write even for
	// a root test runner; clear it after the first attempt fails.
	tmpPath := mgr.paths.BuilderMetadata(builder.ID) + ".tmp"
	require.NoError(t, os.Mkdir(tmpPath, 0755))
	defer os.Remove(tmpPath)
	go func() {
		time.Sleep(1100 * time.Millisecond)
		os.Remove(tmpPath)
	}()

	policy := DefaultBuildPolicy()
	_, err = mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)

	// The retried release succeeded: the builder is immediately
	// re-acquirable and last_used_at was stamped.
	_, err = mgr.builderManager.AcquireForBuild(ctx, builder.ID, "build-2")
	require.NoError(t, err, "release retry must clear the hold")
	got, err := mgr.builderManager.GetBuilder(ctx, builder.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastUsedAt)
}

// TestBuilderHasBuilds_CountsDiskPendingBeforeRecovery persists a queued
// build without enqueueing it (the state between restart and startup
// recovery, which runs only after the builder image is ready) and
// verifies it still counts as builder activity.
func TestBuilderHasBuilds_CountsDiskPendingBeforeRecovery(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	require.NoError(t, writeMetadata(mgr.paths, &buildMetadata{
		ID:        "build-disk",
		Status:    StatusQueued,
		Request:   &CreateBuildRequest{BuilderID: "builder-a"},
		CreatedAt: time.Now(),
	}))

	assert.True(t, mgr.BuilderHasBuilds("builder-a"), "disk-pending build must count before recovery")
	assert.False(t, mgr.BuilderHasBuilds("builder-b"))

	// After recovery completes with nothing left pending, an empty queue
	// reports idle.
	require.NoError(t, deleteBuild(mgr.paths, "build-disk"))
	mgr.RecoverPendingBuilds()
	assert.False(t, mgr.BuilderHasBuilds("builder-a"))
}
