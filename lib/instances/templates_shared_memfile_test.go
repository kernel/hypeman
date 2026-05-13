package instances

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

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
