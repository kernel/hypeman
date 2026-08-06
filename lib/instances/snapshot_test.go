package instances

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForkSnapshotClearsVGPUAssignment(t *testing.T) {
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "snapshot-vgpu-source"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, mgr.defaultHypervisor)

	meta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	meta.GPUProfile = "NVIDIA L40S-2Q"
	meta.GPUFramework = devices.VGPUFramework("future-framework")
	meta.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.4"
	meta.GPUMdevUUID = "retained-uuid"
	require.NoError(t, mgr.saveMetadata(meta))

	snapshot, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Name: "snapshot-vgpu",
	})
	require.NoError(t, err)

	forked, err := mgr.ForkSnapshot(ctx, snapshot.Id, ForkSnapshotRequest{
		Name:        "snapshot-vgpu-fork",
		TargetState: StateStopped,
	})
	require.NoError(t, err)
	assert.Equal(t, "NVIDIA L40S-2Q", forked.GPUProfile)
	assert.Equal(t, devices.VGPUFrameworkNone, forked.GPUFramework)
	assert.Empty(t, forked.GPUDevicePath)
	assert.Empty(t, forked.GPUMdevUUID)

	source, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", source.GPUDevicePath)
}

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
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, forked.Id) })

	require.NoError(t, mgr.DeleteSnapshot(ctx, snap.Id))
	_, err = mgr.GetSnapshot(ctx, snap.Id)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestSnapshotOperationsRejectPathTraversal(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()
	marker := filepath.Join(mgr.paths.DataDir(), "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0644))

	_, err := mgr.GetSnapshot(ctx, "..")
	require.ErrorIs(t, err, ErrSnapshotNotFound)

	err = mgr.DeleteSnapshot(ctx, "..")
	require.ErrorIs(t, err, ErrSnapshotNotFound)
	require.FileExists(t, marker)

	_, err = mgr.ForkSnapshot(ctx, "..", ForkSnapshotRequest{Name: "snapshot-fork"})
	require.ErrorIs(t, err, ErrSnapshotNotFound)

	_, err = mgr.RestoreSnapshot(ctx, "source", "..", RestoreSnapshotRequest{})
	require.ErrorIs(t, err, ErrSnapshotNotFound)
}

func TestStoppedSnapshotRejectsInvalidQEMUMicroVMTargetBeforeMutation(t *testing.T) {
	t.Parallel()
	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "snapshot-invalid-microvm-src"
	createStoppedSnapshotSourceFixture(t, mgr, sourceID, sourceID, mgr.defaultHypervisor)
	source, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	source.HotplugSize = 1
	require.NoError(t, mgr.saveMetadata(source))

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{Kind: SnapshotKindStopped})
	require.NoError(t, err)
	marker := filepath.Join(mgr.paths.InstanceDir(sourceID), "restore-must-not-replace")
	require.NoError(t, os.WriteFile(marker, []byte("source"), 0600))

	_, err = mgr.RestoreSnapshot(ctx, sourceID, snap.Id, RestoreSnapshotRequest{
		TargetState:      StateStopped,
		TargetHypervisor: hypervisor.TypeQEMUMicroVM,
	})
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.FileExists(t, marker, "validation must run before replacing the source payload")

	before, err := mgr.ListInstances(ctx, nil)
	require.NoError(t, err)
	_, err = mgr.ForkSnapshot(ctx, snap.Id, ForkSnapshotRequest{
		Name:             "snapshot-invalid-microvm-fork",
		TargetState:      StateStopped,
		TargetHypervisor: hypervisor.TypeQEMUMicroVM,
	})
	require.ErrorIs(t, err, ErrInvalidRequest)
	after, listErr := mgr.ListInstances(ctx, nil)
	require.NoError(t, listErr)
	require.Len(t, after, len(before), "invalid target must not create a fork")
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

// TestRestoreSnapshotRunningMissingImageReturnsNotFoundFast exercises the
// second FIX-B repro path: restoring a snapshot to a Running target delegates to
// restoreInstance, whose image pre-check must surface images.ErrNotFound (mapped
// to 404 by the handler) instead of hanging in the hypervisor shim when the
// instance's image was deleted. This drives the real restoreSnapshot ->
// restoreInstance delegation, not just the handler boundary.
func TestRestoreSnapshotRunningMissingImageReturnsNotFoundFast(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-restore-missing-image-src"
	snapshotID := "snapshot-restore-missing-image"

	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-restore-missing-image-src", hvType)

	snapshotGuestDir := mgr.paths.SnapshotGuestDir(snapshotID)
	require.NoError(t, os.MkdirAll(mgr.paths.SnapshotDir(snapshotID), 0755))
	require.NoError(t, mgr.copySnapshotPayload(sourceID, snapshotGuestDir))

	sourceMeta, err := mgr.loadMetadata(sourceID)
	require.NoError(t, err)
	require.NoError(t, mgr.saveSnapshotRecord(&snapshotRecord{
		Snapshot: Snapshot{
			Id:               snapshotID,
			Name:             "restore-missing-image",
			Kind:             SnapshotKindStandby,
			SourceInstanceID: sourceID,
			SourceName:       sourceMeta.Name,
			SourceHypervisor: hvType,
			CreatedAt:        time.Now(),
			SizeBytes:        1,
		},
		StoredMetadata: sourceMeta.StoredMetadata,
	}))

	// Simulate the instance's image having been deleted: point the image manager
	// at a different name so GetImage(sourceMeta.Image) returns ErrNotFound.
	mgr.imageManager = readyFixtureImageManager{name: "docker.io/library/some-other-image:latest"}

	assertFailsFastNotFound(t, "restore-from-snapshot should fail fast, not hang in the shim", func() error {
		_, err := mgr.RestoreSnapshot(ctx, sourceID, snapshotID, RestoreSnapshotRequest{
			TargetState: StateRunning,
		})
		return err
	})
}

func TestCreateStandbySnapshotCancelsSourceInstanceCompressionJob(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-create-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-create-src", hvType)

	var instanceCanceled atomic.Bool
	instanceDone := make(chan struct{})

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
	mgr.compressionMu.Unlock()

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-copy",
	})
	require.NoError(t, err)
	require.Equal(t, SnapshotKindStandby, snap.Kind)
	assert.True(t, instanceCanceled.Load(), "instance compression job should be canceled before copying standby snapshot payload")
}

func TestCreateStandbySnapshotFromCompressedSourceCopiesRawMemory(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-create-compressed-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-create-compressed-src", hvType)

	rawPath := filepath.Join(mgr.paths.InstanceSnapshotLatest(sourceID), "memory-ranges")
	require.NoError(t, os.WriteFile(rawPath, []byte("some guest memory"), 0644))
	_, _, err := compressSnapshotMemoryFile(ctx, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-from-compressed",
	})
	require.NoError(t, err)

	snapshotDir := mgr.paths.SnapshotGuestDir(snap.Id)
	_, ok := findRawSnapshotMemoryFile(snapshotDir)
	assert.True(t, ok, "snapshot copy should contain raw memory after preparing a compressed standby source")
	_, _, ok = findCompressedSnapshotMemoryFile(snapshotDir)
	assert.False(t, ok, "snapshot copy should not inherit compressed memory artifacts from the source standby instance")
}

func TestForkSnapshotFromCompressedSourceCopiesRawMemory(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	hvType := mgr.defaultHypervisor
	sourceID := "snapshot-fork-compressed-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "snapshot-fork-compressed-src", hvType)

	snap, err := mgr.CreateSnapshot(ctx, sourceID, CreateSnapshotRequest{
		Kind: SnapshotKindStandby,
		Name: "standby-for-fork-compressed",
	})
	require.NoError(t, err)

	snapshotDir := mgr.paths.SnapshotGuestDir(snap.Id)
	rawPath := filepath.Join(snapshotDir, "memory-ranges")
	snapshotConfigPath := filepath.Join(snapshotDir, "snapshots", "snapshot-latest", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0o755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(rawPath, []byte("some guest memory"), 0o644))
	_, _, err = compressSnapshotMemoryFile(ctx, rawPath, snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(1),
	})
	require.NoError(t, err)

	forked, err := mgr.ForkSnapshot(ctx, snap.Id, ForkSnapshotRequest{
		Name:        "snapshot-fork-compressed",
		TargetState: StateStopped,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = deleteTestInstanceNow(context.Background(), mgr, forked.Id) })

	forkSnapshotDir := mgr.paths.InstanceDir(forked.Id)
	_, ok := findRawSnapshotMemoryFile(forkSnapshotDir)
	assert.True(t, ok, "forked snapshot payload should contain raw memory after preparing a compressed snapshot source")
	_, _, ok = findCompressedSnapshotMemoryFile(forkSnapshotDir)
	assert.False(t, ok, "forked snapshot payload should not retain compressed memory artifacts from the source snapshot")
}

func createStoppedSnapshotSourceFixture(t *testing.T, mgr *manager, id, name string, hvType hypervisor.Type) {
	t.Helper()
	require.NoError(t, mgr.ensureDirectories(id))

	starter, err := mgr.getVMStarter(hvType)
	require.NoError(t, err)

	imageRef := integrationTestImageRef(t, "docker.io/library/alpine:latest")
	// Restore/start resolve the instance's image up front; the real image
	// manager has nothing seeded for this synthetic fixture, so stand in a fake
	// that reports the fixture image as a ready host-native image (mirroring a
	// real instance whose rootfs exists on disk).
	mgr.imageManager = readyFixtureImageManager{name: imageRef}

	now := time.Now()
	meta := &metadata{StoredMetadata: StoredMetadata{
		Id:                id,
		Name:              name,
		Image:             imageRef,
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

// readyFixtureImageManager stands in for the image manager in snapshot/restore
// fixtures: it reports a single host-native ready image so the lifecycle paths'
// image pre-checks pass without seeding a real rootfs. All other methods embed
// the (nil) interface and are unused by these fixtures.
type readyFixtureImageManager struct {
	images.Manager
	name string
}

func (f readyFixtureImageManager) GetImage(_ context.Context, name string) (*images.Image, error) {
	if name != f.name {
		return nil, images.ErrNotFound
	}
	return &images.Image{
		Name:     f.name,
		Digest:   "sha256:fixture",
		Platform: images.HostPlatformString(),
		Status:   images.StatusReady,
	}, nil
}

func createStandbySnapshotSourceFixture(t *testing.T, mgr *manager, id, name string, hvType hypervisor.Type) {
	t.Helper()
	createStoppedSnapshotSourceFixture(t, mgr, id, name, hvType)
	snapshotDir := mgr.paths.InstanceSnapshotLatest(id)
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "state"), []byte("snapshot"), 0644))
}
