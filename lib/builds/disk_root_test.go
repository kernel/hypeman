package builds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupDiskRootVolume_Disabled(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	volID, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.NoError(t, err)
	assert.Empty(t, volID)
	assert.Equal(t, 0, volumeMgr.createCallCount)
}

func TestSetupDiskRootVolume_DefaultSize(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	var gotReq volumes.CreateVolumeRequest
	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		gotReq = req
		return &volumes.Volume{Id: *req.Id, Name: req.Name, SizeGb: req.SizeGb}, nil
	}

	volID, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.NoError(t, err)
	assert.Equal(t, "build-disk-build-1", volID)
	require.NotNil(t, gotReq.Id)
	assert.Equal(t, "build-disk-build-1", *gotReq.Id)
	assert.Equal(t, DefaultDiskRootSizeGB, gotReq.SizeGb)
}

func TestSetupDiskRootVolume_ConfiguredSize(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true
	mgr.config.DiskRootSizeGB = 42

	var gotReq volumes.CreateVolumeRequest
	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		gotReq = req
		return &volumes.Volume{Id: *req.Id, Name: req.Name, SizeGb: req.SizeGb}, nil
	}

	_, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.NoError(t, err)
	assert.Equal(t, 42, gotReq.SizeGb)
}

func TestSetupDiskRootVolume_CreateError(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		return nil, errors.New("disk full")
	}

	volID, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.Error(t, err)
	assert.Empty(t, volID)
}

func TestSetupDiskRootVolume_LeftoverFromCrash(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	// Simulate a volume left behind by a crashed build: the first create
	// fails with ErrAlreadyExists, the leftover is deleted, and the retry
	// succeeds.
	created := false
	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		if !created {
			created = true
			return nil, volumes.ErrAlreadyExists
		}
		return &volumes.Volume{Id: *req.Id, Name: req.Name, SizeGb: req.SizeGb}, nil
	}

	volID, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.NoError(t, err)
	assert.Equal(t, "build-disk-build-1", volID)
	assert.Equal(t, 1, volumeMgr.deleteCallCount)
	assert.Equal(t, 2, volumeMgr.createCallCount)
}

func TestSetupDiskRootVolume_LeftoverDeleteError(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		return nil, volumes.ErrAlreadyExists
	}
	volumeMgr.deleteFunc = func(ctx context.Context, id string) error {
		return errors.New("volume attached")
	}

	volID, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.Error(t, err)
	assert.Empty(t, volID)
}

// TestSetupDiskRootVolume_LeftoverAttachedToStaleBuilder verifies recovery
// when a crash left the volume attached to a surviving builder VM: the first
// delete fails with ErrInUse, the stale builder is deleted (detaching the
// volume), and the retry succeeds.
func TestSetupDiskRootVolume_LeftoverAttachedToStaleBuilder(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	created := false
	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		if !created {
			created = true
			return nil, volumes.ErrAlreadyExists
		}
		return &volumes.Volume{Id: *req.Id, Name: req.Name, SizeGb: req.SizeGb}, nil
	}
	builderDeleted := false
	volumeMgr.deleteFunc = func(ctx context.Context, id string) error {
		if !builderDeleted {
			return volumes.ErrInUse
		}
		return nil
	}
	instanceMgr.getFunc = func(ctx context.Context, id string) (*instances.Instance, error) {
		if id == "builder-build-1" {
			return &instances.Instance{
				StoredMetadata: instances.StoredMetadata{Id: "inst-builder-build-1", Name: "builder-build-1"},
			}, nil
		}
		return nil, instances.ErrNotFound
	}
	instanceMgr.deleteFunc = func(ctx context.Context, id string) error {
		assert.Equal(t, "inst-builder-build-1", id)
		builderDeleted = true
		return nil
	}

	volID, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.NoError(t, err)
	assert.Equal(t, "build-disk-build-1", volID)
	assert.True(t, builderDeleted, "stale builder holding the volume must be deleted")
	assert.Equal(t, 2, volumeMgr.deleteCallCount)
	assert.Equal(t, 2, volumeMgr.createCallCount)
}

// TestSetupDiskRootVolume_LeftoverInUseWithoutBuilder verifies recovery fails
// loudly when the leftover volume is attached but no stale builder exists to
// clean up.
func TestSetupDiskRootVolume_LeftoverInUseWithoutBuilder(t *testing.T) {
	mgr, _, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		return nil, volumes.ErrAlreadyExists
	}
	volumeMgr.deleteFunc = func(ctx context.Context, id string) error {
		return volumes.ErrInUse
	}

	volID, err := mgr.setupDiskRootVolume(context.Background(), "build-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale builder")
	assert.Empty(t, volID)
}

// TestCreateBuildConfigVolume_UniqueTempPath verifies the config disk is
// written to a unique per-call path, so concurrent builds reusing a build ID
// (as parallel test runs sharing TMPDIR do) never collide on a fixed file.
func TestCreateBuildConfigVolume_UniqueTempPath(t *testing.T) {
	mgr, _, _, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	req := CreateBuildRequest{Dockerfile: "FROM alpine"}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	path1, err := mgr.createBuildConfigVolume("build-1", "build-config-build-1")
	require.NoError(t, err)
	defer os.RemoveAll(filepath.Dir(path1))
	path2, err := mgr.createBuildConfigVolume("build-1", "build-config-build-1")
	require.NoError(t, err)
	defer os.RemoveAll(filepath.Dir(path2))

	assert.NotEqual(t, path1, path2)
	for _, p := range []string{path1, path2} {
		_, err := os.Stat(p)
		require.NoError(t, err)
	}
}

func TestBuilderVolumeAttachments(t *testing.T) {
	attachments := builderVolumeAttachments("src-vol", "cfg-vol", "")
	require.Len(t, attachments, 2)
	assert.Equal(t, "/src", attachments[0].MountPath)
	assert.False(t, attachments[0].Readonly)
	assert.Equal(t, "/config", attachments[1].MountPath)
	assert.True(t, attachments[1].Readonly)

	attachments = builderVolumeAttachments("src-vol", "cfg-vol", "disk-vol")
	require.Len(t, attachments, 3)
	assert.Equal(t, "disk-vol", attachments[2].VolumeID)
	assert.Equal(t, "/var/lib/buildkit", attachments[2].MountPath)
	assert.False(t, attachments[2].Readonly)
}

// prepareBuildOnDisk writes the metadata, source, and config files executeBuild
// expects, without going through CreateBuild (which would start a background
// build goroutine via the queue).
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
// returns quickly.
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

// TestExecuteBuild_DiskRootLifecycle runs executeBuild with the disk root
// feature enabled and verifies the volume is created, attached at
// /var/lib/buildkit, and deleted when the build finishes. It uses mock
// managers only — no privileged mounts.
func TestExecuteBuild_DiskRootLifecycle(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	var createReq instances.CreateInstanceRequest
	stoppedBuilderInstance(instanceMgr, &createReq)

	policy := DefaultBuildPolicy()
	result, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Success)

	// Attached at /var/lib/buildkit in the builder instance request.
	require.Len(t, createReq.Volumes, 3)
	assert.Equal(t, "build-disk-build-1", createReq.Volumes[2].VolumeID)
	assert.Equal(t, "/var/lib/buildkit", createReq.Volumes[2].MountPath)
	assert.False(t, createReq.Volumes[2].Readonly)

	// Deleted with the builder VM.
	_, err = volumeMgr.GetVolume(ctx, "build-disk-build-1")
	assert.ErrorIs(t, err, volumes.ErrNotFound)
	assert.GreaterOrEqual(t, volumeMgr.deleteCallCount, 1)
}

// TestExecuteBuild_DiskRootDisabledLeavesDefaultPath verifies that with the
// feature disabled no disk root volume is created and the builder instance
// only gets the source and config attachments.
func TestExecuteBuild_DiskRootDisabledLeavesDefaultPath(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	var createReq instances.CreateInstanceRequest
	stoppedBuilderInstance(instanceMgr, &createReq)

	policy := DefaultBuildPolicy()
	_, err := mgr.executeBuild(ctx, "build-1", req, &policy)
	require.NoError(t, err)

	require.Len(t, createReq.Volumes, 2)
	assert.Equal(t, "/src", createReq.Volumes[0].MountPath)
	assert.Equal(t, "/config", createReq.Volumes[1].MountPath)
	for _, v := range volumeMgr.volumes {
		assert.NotContains(t, v.Name, "build-disk-")
	}
}

// TestExecuteBuild_DiskRootCreateErrorFailsBuild verifies a volume creation
// failure fails the build before the builder instance is created.
func TestExecuteBuild_DiskRootCreateErrorFailsBuild(t *testing.T) {
	mgr, instanceMgr, volumeMgr, tempDir := setupTestManager(t)
	defer os.RemoveAll(tempDir)
	mgr.config.DiskRootEnabled = true

	ctx := context.Background()
	req := CreateBuildRequest{
		Dockerfile: "FROM alpine\nRUN echo hello",
	}
	prepareBuildOnDisk(t, mgr, "build-1", req)

	volumeMgr.createFunc = func(ctx context.Context, req volumes.CreateVolumeRequest) (*volumes.Volume, error) {
		if req.Name == "build-disk-build-1" {
			return nil, errors.New("disk full")
		}
		vol := &volumes.Volume{Id: "vol-" + req.Name, Name: req.Name}
		volumeMgr.volumes[vol.Id] = vol
		return vol, nil
	}

	policy := DefaultBuildPolicy()
	_, err := mgr.executeBuild(ctx, "build-1", req, &policy)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "buildkit root volume")
	assert.Equal(t, 0, instanceMgr.createCallCount)
}
