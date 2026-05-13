//go:build linux

package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
