package instances

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
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

func TestForkSharesSnapshotMemory(t *testing.T) {
	t.Parallel()

	gpu := &StoredMetadata{HypervisorType: hypervisor.TypeQEMU, HypervisorVersion: qemu.VGPUSnapshotVersion, GPUProfile: "NVIDIA L40S-3Q"}
	assert.True(t, forkSharesSnapshotMemory(&StoredMetadata{HypervisorType: hypervisor.TypeFirecracker}))
	assert.True(t, forkSharesSnapshotMemory(gpu))
	// System QEMU streams its snapshot through a pipe and keeps private copies.
	assert.False(t, forkSharesSnapshotMemory(&StoredMetadata{HypervisorType: hypervisor.TypeQEMU}))
	assert.False(t, forkSharesSnapshotMemory(&StoredMetadata{HypervisorType: hypervisor.TypeCloudHypervisor}))
}

func TestCloneGuestDirectoryForForkHardlinksSharedMemory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	src := filepath.Join(root, "template")
	dst := filepath.Join(root, "fork")
	srcMem := firecrackerSnapshotMemoryPathInGuestDir(src)
	require.NoError(t, os.MkdirAll(filepath.Dir(srcMem), 0755))
	require.NoError(t, os.WriteFile(srcMem, []byte("guest memory"), 0644))
	srcConfig := filepath.Join(filepath.Dir(srcMem), "qemu-config.json")
	require.NoError(t, os.WriteFile(srcConfig, []byte("{}"), 0644))

	m := &manager{}
	require.NoError(t, m.cloneGuestDirectoryForFork(context.Background(), src, dst, true))

	dstMem := firecrackerSnapshotMemoryPathInGuestDir(dst)
	srcInfo, err := os.Stat(srcMem)
	require.NoError(t, err)
	dstInfo, err := os.Stat(dstMem)
	require.NoError(t, err)
	assert.True(t, os.SameFile(srcInfo, dstInfo), "fork must share the template's memory inode")
	assertDifferentInode(t, srcConfig, filepath.Join(filepath.Dir(dstMem), "qemu-config.json"))
}
