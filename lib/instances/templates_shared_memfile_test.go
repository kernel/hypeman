package instances

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	require.NoError(t, err)
	bi, err := os.Stat(b)
	require.NoError(t, err)
	as := ai.Sys().(*syscall.Stat_t)
	bs := bi.Sys().(*syscall.Stat_t)
	return as.Ino == bs.Ino && as.Dev == bs.Dev
}

// TestInstallForkSharedMemFile_HardlinksSourceMemFile verifies that the helper
// creates a hardlink at the fork's snapshot mem-file path that shares the
// source instance's mem-file inode.
func TestInstallForkSharedMemFile_HardlinksSourceMemFile(t *testing.T) {
	t.Parallel()

	mgr, _ := newStorageOnlyManager(t)
	sourceID := "shared-memfile-source"

	srcSnapshotDir := mgr.paths.InstanceSnapshotLatest(sourceID)
	require.NoError(t, os.MkdirAll(srcSnapshotDir, 0o755))
	srcMem := filepath.Join(srcSnapshotDir, templateSharedMemFileName)
	require.NoError(t, os.WriteFile(srcMem, []byte("guest memory bytes"), 0o644))

	forkDir := filepath.Join(t.TempDir(), "fork-data")

	require.NoError(t, mgr.installForkSharedMemFile(forkDir, sourceID))

	forkMem := filepath.Join(forkDir, "snapshots", "snapshot-latest", templateSharedMemFileName)
	info, err := os.Lstat(forkMem)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "fork mem-file must be a regular file (hardlink), not a symlink")
	assert.True(t, sameInode(t, srcMem, forkMem), "fork mem-file must share the source's inode")
}

// TestInstallForkSharedMemFile_ErrorsWhenSourceMissing makes sure the helper
// refuses to silently create a dangling link when the source mem-file does not
// exist.
func TestInstallForkSharedMemFile_ErrorsWhenSourceMissing(t *testing.T) {
	t.Parallel()

	mgr, _ := newStorageOnlyManager(t)
	forkDir := filepath.Join(t.TempDir(), "fork-data")

	err := mgr.installForkSharedMemFile(forkDir, "no-such-source")
	require.Error(t, err)
}

// TestForkFirecrackerSharesMemFile_FromTemplate verifies the end-to-end fork
// path: when the source is a Firecracker Template instance, the fork's
// mem-file is a hardlink to the source's mem-file instead of a copy. This
// preserves the firecracker MAP_PRIVATE COW semantics that let multiple forks
// share the heavy backing file.
func TestForkFirecrackerSharesMemFile_FromTemplate(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "shared-memfile-fc-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "shared-memfile-fc-src", hypervisor.TypeFirecracker)
	promoteFixtureToTemplate(t, mgr, sourceID)

	srcSnapshotDir := mgr.paths.InstanceSnapshotLatest(sourceID)
	srcMem := filepath.Join(srcSnapshotDir, templateSharedMemFileName)
	require.NoError(t, os.WriteFile(srcMem, []byte("firecracker mem-file contents"), 0o644))
	snapshotConfigPath := mgr.paths.InstanceSnapshotConfig(sourceID)
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0o755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0o644))

	forked, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{
		Name:        "shared-memfile-fc-fork",
		TargetState: StateStopped,
	}, true)
	require.NoError(t, err)
	require.NotNil(t, forked)

	forkMem := filepath.Join(mgr.paths.InstanceSnapshotLatest(forked.Id), templateSharedMemFileName)
	info, err := os.Lstat(forkMem)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "fork mem-file must be a regular file (hardlink) for firecracker fan-out")
	assert.True(t, sameInode(t, srcMem, forkMem), "fork mem-file must share the source's inode")
}

// TestForkFirecrackerStandbySourceDoesNotShareMemFile guards the
// non-Template carve-out: forking a plain Standby source must copy the
// mem-file outright. Sharing would let a later RestoreInstance on the source
// mutate the file out from under live forks.
func TestForkFirecrackerStandbySourceDoesNotShareMemFile(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()

	sourceID := "standby-fork-fc-src"
	createStandbySnapshotSourceFixture(t, mgr, sourceID, "standby-fork-fc-src", hypervisor.TypeFirecracker)

	srcSnapshotDir := mgr.paths.InstanceSnapshotLatest(sourceID)
	srcMem := filepath.Join(srcSnapshotDir, templateSharedMemFileName)
	require.NoError(t, os.WriteFile(srcMem, []byte("firecracker mem-file contents"), 0o644))
	snapshotConfigPath := mgr.paths.InstanceSnapshotConfig(sourceID)
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshotConfigPath), 0o755))
	require.NoError(t, os.WriteFile(snapshotConfigPath, []byte(`{}`), 0o644))

	forked, err := mgr.forkInstanceFromStoppedOrStandby(ctx, sourceID, ForkInstanceRequest{
		Name:        "standby-fork-fc-fork",
		TargetState: StateStopped,
	}, true)
	require.NoError(t, err)
	require.NotNil(t, forked)

	forkMem := filepath.Join(mgr.paths.InstanceSnapshotLatest(forked.Id), templateSharedMemFileName)
	info, err := os.Lstat(forkMem)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular(), "standby-source fork mem-file must be a regular file copy")
	assert.False(t, sameInode(t, srcMem, forkMem), "standby-source fork mem-file must be a copy, not a hardlink to source")
}

// promoteFixtureToTemplate marks the source's stored metadata as a Template
// without invoking the full PromoteToTemplate lifecycle (which would require
// a live VM). Test-only shortcut.
func promoteFixtureToTemplate(t *testing.T, mgr *manager, id string) {
	t.Helper()
	meta, err := mgr.loadMetadata(id)
	require.NoError(t, err)
	meta.IsTemplate = true
	require.NoError(t, mgr.saveMetadata(meta))
}
