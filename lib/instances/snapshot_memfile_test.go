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

func TestCloneCloudHypervisorSnapshotSharesMemoryInode(t *testing.T) {
	t.Parallel()

	srcDir := filepath.Join(t.TempDir(), "source")
	dstDir := filepath.Join(t.TempDir(), "fork")
	srcMemory, ok := sharedSnapshotMemoryPathInGuestDir(srcDir, hypervisor.TypeCloudHypervisor)
	require.True(t, ok)
	require.NoError(t, os.MkdirAll(filepath.Dir(srcMemory), 0755))
	require.NoError(t, os.WriteFile(srcMemory, []byte("snapshot memory"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "metadata.json"), []byte("{}"), 0600))

	mgr := &manager{}
	require.NoError(t, mgr.cloneGuestDirectoryForFork(
		context.Background(), hypervisor.TypeCloudHypervisor, srcDir, dstDir, true,
	))

	dstMemory, ok := sharedSnapshotMemoryPathInGuestDir(dstDir, hypervisor.TypeCloudHypervisor)
	require.True(t, ok)
	assertSameSnapshotMemoryInode(t, srcMemory, dstMemory)
	assert.FileExists(t, filepath.Join(dstDir, "metadata.json"))
}

func TestEnsureExclusiveCloudHypervisorSnapshotMemory(t *testing.T) {
	t.Parallel()

	snapshotDir := t.TempDir()
	memoryPath := filepath.Join(snapshotDir, "memory-ranges")
	require.NoError(t, os.WriteFile(memoryPath, []byte("shared memory"), 0600))
	siblingPath := filepath.Join(t.TempDir(), "memory-ranges")
	require.NoError(t, os.Link(memoryPath, siblingPath))

	require.NoError(t, ensureExclusiveSnapshotMemoryOwnership(
		context.Background(), snapshotDir, hypervisor.TypeCloudHypervisor,
	))

	memoryInfo, err := os.Stat(memoryPath)
	require.NoError(t, err)
	siblingInfo, err := os.Stat(siblingPath)
	require.NoError(t, err)
	assert.False(t, os.SameFile(memoryInfo, siblingInfo))
	contents, err := os.ReadFile(memoryPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("shared memory"), contents)
}

func TestEnsureExclusiveCloudHypervisorSnapshotMemoryReplacesPrivateInode(t *testing.T) {
	t.Parallel()

	snapshotDir := t.TempDir()
	memoryPath := filepath.Join(snapshotDir, "memory-ranges")
	require.NoError(t, os.WriteFile(memoryPath, []byte("private memory"), 0600))
	before, err := os.Stat(memoryPath)
	require.NoError(t, err)

	require.NoError(t, ensureExclusiveSnapshotMemoryOwnership(
		context.Background(), snapshotDir, hypervisor.TypeCloudHypervisor,
	))

	after, err := os.Stat(memoryPath)
	require.NoError(t, err)
	assert.False(t, os.SameFile(before, after))
	contents, err := os.ReadFile(memoryPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("private memory"), contents)
}

func assertSameSnapshotMemoryInode(t *testing.T, first, second string) {
	t.Helper()
	firstInfo, err := os.Stat(first)
	require.NoError(t, err)
	secondInfo, err := os.Stat(second)
	require.NoError(t, err)
	firstStat, ok := firstInfo.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	secondStat, ok := secondInfo.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, firstStat.Dev, secondStat.Dev)
	assert.Equal(t, firstStat.Ino, secondStat.Ino)
	assert.GreaterOrEqual(t, uint64(firstStat.Nlink), uint64(2))
}
