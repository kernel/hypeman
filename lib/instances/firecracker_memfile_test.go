package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertDifferentInode(t *testing.T, pathA, pathB string) {
	t.Helper()
	infoA, err := os.Stat(pathA)
	require.NoError(t, err)
	infoB, err := os.Stat(pathB)
	require.NoError(t, err)
	assert.False(t, os.SameFile(infoA, infoB), "%s and %s must not share an inode", pathA, pathB)
}

func TestEnsureExclusiveSnapshotMemoryOwnershipUnsharesHardlinkedMemory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshots", "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	memPath := filepath.Join(snapshotDir, "memory")
	require.NoError(t, os.WriteFile(memPath, []byte("shared memory"), 0644))
	forkLinkPath := filepath.Join(root, "fork-memory")
	require.NoError(t, os.Link(memPath, forkLinkPath))

	require.NoError(t, ensureExclusiveSnapshotMemoryOwnership(context.Background(), snapshotDir))

	assertDifferentInode(t, memPath, forkLinkPath)
	unshared, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("shared memory"), unshared)
	forkContent, err := os.ReadFile(forkLinkPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("shared memory"), forkContent)
}

func TestEnsureExclusiveSnapshotMemoryOwnershipSkipsPrivateMemory(t *testing.T) {
	t.Parallel()

	snapshotDir := filepath.Join(t.TempDir(), "snapshot-latest")
	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	memPath := filepath.Join(snapshotDir, "memory")
	require.NoError(t, os.WriteFile(memPath, []byte("private memory"), 0644))
	stalePath := memPath + ".unshare.tmp"
	require.NoError(t, os.WriteFile(stalePath, []byte("stale"), 0644))
	before, err := os.Stat(memPath)
	require.NoError(t, err)

	require.NoError(t, ensureExclusiveSnapshotMemoryOwnership(context.Background(), snapshotDir))

	after, err := os.Stat(memPath)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "private mem-file must not be rewritten")
	assert.NoFileExists(t, stalePath, "stale unshare tmp must be swept on standby entry")

	require.NoError(t, ensureExclusiveSnapshotMemoryOwnership(context.Background(), filepath.Join(t.TempDir(), "missing")))
}
