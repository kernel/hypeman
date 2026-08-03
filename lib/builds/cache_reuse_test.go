package builds

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/tags"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enableCacheVolumes attaches an enabled cache volume manager (backed by the
// test mock volume manager) to a test build manager.
func enableCacheVolumes(mgr *manager, volumeMgr *mockVolumeManager, config CacheVolumeConfig) {
	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		id := req.Name
		if req.Id != nil && *req.Id != "" {
			id = *req.Id
		}
		if vol, ok := volumeMgr.volumes[id]; ok {
			return vol, nil
		}
		vol := &volumes.Volume{
			Id:        id,
			Name:      req.Name,
			SizeGb:    req.SizeGb,
			Tags:      tags.Clone(req.Tags),
			CreatedAt: time.Now(),
		}
		volumeMgr.volumes[id] = vol
		return vol, nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr.cacheVolumes = newCacheVolumeManager(config, mgr.paths, volumeMgr, logger)
}

// TestExecuteBuild_CacheReuseSameScope verifies two builds of the same scope
// attach the same persistent cache volume and that the volume is retained
// after the builds complete.
func TestExecuteBuild_CacheReuseSameScope(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		CacheScope: "tenant-a",
	}
	policy := DefaultBuildPolicy()

	createReq := &instances.CreateInstanceRequest{}
	stoppedBuilderInstance(instanceMgr, createReq)

	prepareBuildOnDisk(t, mgr, "build-1", req)
	_, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)
	firstVolumes := createReq.Volumes

	require.Len(t, firstVolumes, 3)
	cacheVolID := firstVolumes[2].VolumeID
	assert.Equal(t, cacheVolumeID("tenant-a"), cacheVolID)
	assert.Equal(t, "/var/lib/buildkit", firstVolumes[2].MountPath)
	assert.False(t, firstVolumes[2].Readonly)

	// Retained after the build — a second same-scope build reuses it.
	_, err = volumeMgr.GetVolume(ctx, cacheVolID)
	require.NoError(t, err, "cache volume must be retained after the build")

	createReq2 := &instances.CreateInstanceRequest{}
	stoppedBuilderInstance(instanceMgr, createReq2)
	prepareBuildOnDisk(t, mgr, "build-2", req)
	_, err = mgr.executeBuild(ctx, "build-2", req, &policy)
	require.NoError(t, err)

	require.Len(t, createReq2.Volumes, 3)
	assert.Equal(t, cacheVolID, createReq2.Volumes[2].VolumeID, "same scope reuses the same cache volume")

	// Last-used metadata was updated by both builds.
	mgr.cacheVolumes.mu.Lock()
	_, ok := mgr.cacheVolumes.lastUsed[cacheVolID]
	mgr.cacheVolumes.mu.Unlock()
	assert.True(t, ok, "last-used metadata recorded")
}

// TestExecuteBuild_CacheTenantIsolation verifies different scopes attach
// different cache volumes, so one tenant's cache is never visible to another.
func TestExecuteBuild_CacheTenantIsolation(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	policy := DefaultBuildPolicy()

	volIDs := make(map[string]string)
	for _, scope := range []string{"tenant-a", "tenant-b"} {
		req := CreateBuildRequest{
			Dockerfile: "FROM alpine\nRUN echo hello",
			CacheScope: scope,
		}
		id := "build-" + scope
		prepareBuildOnDisk(t, mgr, id, req)

		createReq := &instances.CreateInstanceRequest{}
		stoppedBuilderInstance(instanceMgr, createReq)
		_, err := mgr.executeBuild(ctx, id, req, &policy)
		require.NoError(t, err)

		require.Len(t, createReq.Volumes, 3)
		volIDs[scope] = createReq.Volumes[2].VolumeID
	}

	assert.NotEqual(t, volIDs["tenant-a"], volIDs["tenant-b"], "each scope gets its own cache volume")
	assert.Equal(t, cacheVolumeID("tenant-a"), volIDs["tenant-a"])
	assert.Equal(t, cacheVolumeID("tenant-b"), volIDs["tenant-b"])
}

// TestExecuteBuild_CacheSerialization verifies a build holds the per-scope
// lock for its whole duration: a second build of the same scope cannot start
// its instance while the first is still running.
func TestExecuteBuild_CacheSerialization(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		CacheScope: "tenant-a",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)
	stoppedBuilderInstance(instanceMgr, nil)

	// Hold the scope lock as an in-flight build would.
	unlock := mgr.cacheVolumes.lockScope("tenant-a")

	done := make(chan struct{})
	go func() {
		defer close(done)
		policy := DefaultBuildPolicy()
		mgr.executeBuild(ctx, "build-1", req, &policy)
	}()

	// The build must not reach instance creation while the lock is held.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, 0, instanceMgr.createCallCount, "build started while scope lock was held")

	unlock()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("build did not finish after scope lock release")
	}
	assert.Equal(t, 1, instanceMgr.createCallCount)
}

// TestExecuteBuild_CacheDisabledKeepsDefaultPath verifies that with the local
// cache disabled a scoped build gets no cache volume.
func TestExecuteBuild_CacheDisabledKeepsDefaultPath(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		CacheScope: "tenant-a",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	createReq := &instances.CreateInstanceRequest{}
	stoppedBuilderInstance(instanceMgr, createReq)

	policy := DefaultBuildPolicy()
	_, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)

	require.Len(t, createReq.Volumes, 2, "no cache volume when disabled")
	for _, v := range volumeMgr.volumes {
		assert.NotContains(t, v.Name, cacheVolumeIDPrefix)
	}
}

// TestExecuteBuild_AdminBuildSkipsCacheVolume verifies admin builds never
// attach tenant cache volumes.
func TestExecuteBuild_AdminBuildSkipsCacheVolume(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile:   "FROM alpine\nRUN echo hello",
		CacheScope:   "tenant-a",
		IsAdminBuild: true,
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	createReq := &instances.CreateInstanceRequest{}
	stoppedBuilderInstance(instanceMgr, createReq)

	policy := DefaultBuildPolicy()
	_, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)

	require.Len(t, createReq.Volumes, 2, "admin builds skip the tenant cache volume")
}

// TestExecuteBuild_CacheVolumePrecedenceOverDiskRoot verifies the persistent
// cache volume wins when both features are enabled, and no ephemeral disk
// root volume is created.
func TestExecuteBuild_CacheVolumePrecedenceOverDiskRoot(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		CacheScope: "tenant-a",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	createReq := &instances.CreateInstanceRequest{}
	stoppedBuilderInstance(instanceMgr, createReq)

	policy := DefaultBuildPolicy()
	_, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)

	require.Len(t, createReq.Volumes, 3)
	assert.Equal(t, cacheVolumeID("tenant-a"), createReq.Volumes[2].VolumeID)
	for _, v := range volumeMgr.volumes {
		assert.NotContains(t, v.Name, "build-disk-", "no ephemeral disk root volume when the cache volume is used")
	}
}

// TestCreateBuild_CacheGCKeepBytes verifies the build config passed to the
// builder VM carries the bounded GC setting when a persistent cache volume
// will back the build.
// TestExecuteBuild_FailedEnsureLeavesNoLastUsed verifies that when cache
// volume setup fails, the deferred cleanup records no last-used entry for
// the volume that was never created.
func TestExecuteBuild_FailedEnsureLeavesNoLastUsed(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		CacheScope: "tenant-a",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	// An unmanaged volume squatting on the deterministic cache volume ID
	// makes ensureCacheVolume refuse to reuse it.
	cacheVolID := cacheVolumeID("tenant-a")
	volumeMgr.volumes[cacheVolID] = &volumes.Volume{Id: cacheVolID, Name: cacheVolID, SizeGb: 1}

	policy := DefaultBuildPolicy()
	_, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ensure build cache volume")

	mgr.cacheVolumes.mu.Lock()
	_, ok := mgr.cacheVolumes.lastUsed[cacheVolID]
	mgr.cacheVolumes.mu.Unlock()
	assert.False(t, ok, "no last-used entry for a volume that was never ensured")
}

func TestCreateBuild_CacheGCKeepBytes(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()

	readConfig := func(id string) *BuildConfig {
		data, err := os.ReadFile(mgr.paths.BuildConfig(id))
		require.NoError(t, err)
		var cfg BuildConfig
		require.NoError(t, json.Unmarshal(data, &cfg))
		return &cfg
	}

	// Scoped tenant build: bounded GC configured.
	scoped, err := mgr.CreateBuild(ctx, CreateBuildRequest{
		Dockerfile: "FROM alpine",
		CacheScope: "tenant-a",
	}, []byte("source"))
	require.NoError(t, err)
	expectedKeep := int64(30) * 1024 * 1024 * 1024 * 9 / 10
	assert.Equal(t, expectedKeep, readConfig(scoped.ID).CacheGCKeepBytes)

	// Unscoped build: no GC bound.
	unscoped, err := mgr.CreateBuild(ctx, CreateBuildRequest{
		Dockerfile: "FROM alpine",
	}, []byte("source"))
	require.NoError(t, err)
	assert.Zero(t, readConfig(unscoped.ID).CacheGCKeepBytes)

	// Admin build: no GC bound.
	admin, err := mgr.CreateBuild(ctx, CreateBuildRequest{
		Dockerfile:   "FROM alpine",
		CacheScope:   "tenant-a",
		IsAdminBuild: true,
	}, []byte("source"))
	require.NoError(t, err)
	assert.Zero(t, readConfig(admin.ID).CacheGCKeepBytes)
}

// TestBuildSerialKey verifies only builds that attach a persistent cache
// volume are serialized in the queue by scope.
func TestBuildSerialKey(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	// Cache disabled: no serialization.
	assert.Empty(t, mgr.buildSerialKey(CreateBuildRequest{CacheScope: "tenant-a"}))

	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})
	assert.Equal(t, "tenant-a", mgr.buildSerialKey(CreateBuildRequest{CacheScope: "tenant-a"}))
	assert.Empty(t, mgr.buildSerialKey(CreateBuildRequest{}), "unscoped builds are unconstrained")
	assert.Empty(t, mgr.buildSerialKey(CreateBuildRequest{CacheScope: "tenant-a", IsAdminBuild: true}), "admin builds are unconstrained")
}

// TestExecuteBuild_RecoversStaleCacheVolumeHolder verifies that when a
// crashed build left its builder attached to the cache volume, the next
// same-scope build deletes the stale builder and retries instance creation
// instead of wedging the scope.
func TestExecuteBuild_RecoversStaleCacheVolumeHolder(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		CacheScope: "tenant-a",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	// Pre-create the cache volume and leave it attached to a stale builder
	// from a crashed build.
	cacheVolID, err := mgr.cacheVolumes.ensureCacheVolume(ctx, "tenant-a")
	require.NoError(t, err)
	stale := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-stale", Name: "builder-crashed"},
		State:          instances.StateRunning,
	}
	instanceMgr.instances[stale.Id] = stale
	volumeMgr.volumes[cacheVolID].Attachments = []volumes.Attachment{
		{InstanceID: stale.Id, MountPath: "/var/lib/buildkit"},
	}

	// Instance creation fails while the cache volume is held, then succeeds
	// once the stale builder is removed and its attachment is gone.
	createCalls := 0
	instanceMgr.createFunc = func(ctx context.Context, req instances.CreateInstanceRequest) (*instances.Instance, error) {
		createCalls++
		if len(volumeMgr.volumes[cacheVolID].Attachments) > 0 {
			return nil, volumes.ErrInUse
		}
		inst := &instances.Instance{
			StoredMetadata: instances.StoredMetadata{Id: "inst-" + req.Name, Name: req.Name},
			State:          instances.StateStopped,
		}
		instanceMgr.instances[inst.Id] = inst
		return inst, nil
	}

	policy := DefaultBuildPolicy()
	_, err = mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)
	assert.Equal(t, 2, createCalls, "instance creation retried after stale holder removal")
	_, getErr := instanceMgr.GetInstance(ctx, stale.Id)
	assert.ErrorIs(t, getErr, instances.ErrNotFound, "stale builder deleted")
	assert.Empty(t, volumeMgr.volumes[cacheVolID].Attachments, "attachment surviving builder deletion is detached")
}

// TestExecuteBuild_RecoversOrphanCacheVolumeAttachment verifies that stale
// recovery also handles orphaned volume attachments whose instance metadata is
// already gone.
func TestExecuteBuild_RecoversOrphanCacheVolumeAttachment(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	enableCacheVolumes(mgr, volumeMgr, CacheVolumeConfig{Enabled: true, SizeGB: 30})

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
		CacheScope: "tenant-a",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	cacheVolID, err := mgr.cacheVolumes.ensureCacheVolume(ctx, "tenant-a")
	require.NoError(t, err)
	volumeMgr.volumes[cacheVolID].Attachments = []volumes.Attachment{
		{InstanceID: "inst-orphan", MountPath: "/var/lib/buildkit"},
	}

	createCalls := 0
	instanceMgr.createFunc = func(ctx context.Context, req instances.CreateInstanceRequest) (*instances.Instance, error) {
		createCalls++
		if len(volumeMgr.volumes[cacheVolID].Attachments) > 0 {
			return nil, volumes.ErrInUse
		}
		inst := &instances.Instance{
			StoredMetadata: instances.StoredMetadata{Id: "inst-" + req.Name, Name: req.Name},
			State:          instances.StateStopped,
		}
		instanceMgr.instances[inst.Id] = inst
		return inst, nil
	}

	policy := DefaultBuildPolicy()
	_, err = mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)
	assert.Equal(t, 2, createCalls, "instance creation retried after orphan attachment removal")
	assert.Empty(t, volumeMgr.volumes[cacheVolID].Attachments, "orphan attachment detached from cache volume")
}

// TestDetachStaleCacheVolumeHolders_DeleteInstanceNotFoundDetachesSurvivingAttachment
// covers a concurrent delete landing between GetInstance and DeleteInstance:
// the stale builder is already gone, so the surviving cache volume
// attachment must still be detached instead of failing the build.
func TestDetachStaleCacheVolumeHolders_DeleteInstanceNotFoundDetachesSurvivingAttachment(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	stale := &instances.Instance{
		StoredMetadata: instances.StoredMetadata{Id: "inst-stale", Name: "builder-old"},
		State:          instances.StateRunning,
	}
	instanceMgr.instances[stale.Id] = stale
	instanceMgr.deleteFunc = func(ctx context.Context, id string) error {
		delete(instanceMgr.instances, id)
		return instances.ErrNotFound
	}

	volumeMgr.volumes["cache-vol"] = &volumes.Volume{
		Id:   "cache-vol",
		Name: "cache-vol",
		Attachments: []volumes.Attachment{
			{InstanceID: stale.Id, MountPath: "/var/lib/buildkit"},
		},
	}

	ctx := context.Background()
	require.NoError(t, mgr.detachStaleCacheVolumeHolders(ctx, "cache-vol"))
	assert.Empty(t, volumeMgr.volumes["cache-vol"].Attachments,
		"surviving attachment detached despite DeleteInstance ErrNotFound")
}
