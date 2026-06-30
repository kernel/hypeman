package instances

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShutdownHypervisorRemovesControlSocket(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "hypeman-standby-socket-")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.RemoveAll(tmpDir)
	})
	socketPath := filepath.Join(tmpDir, "noop.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	require.NoError(t, listener.Close())

	lifecycleNoopHypervisorStates.Store(socketPath, hypervisor.StateRunning)
	t.Cleanup(func() {
		lifecycleNoopHypervisorStates.Delete(socketPath)
	})

	m := &manager{}
	inst := &Instance{
		StoredMetadata: StoredMetadata{
			Id:             "standby-socket-cleanup",
			SocketPath:     socketPath,
			HypervisorType: lifecycleNoopHypervisorType,
		},
	}

	require.NoError(t, m.shutdownHypervisor(t.Context(), inst))

	_, err = os.Stat(socketPath)
	require.True(t, os.IsNotExist(err), "shutdown should remove the hypervisor control socket")
}

func TestDiscardPromotedRetainedSnapshotTargetAfterSnapshotError(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot-latest")
	retainedBaseDir := filepath.Join(root, "snapshot-base")

	require.NoError(t, os.MkdirAll(retainedBaseDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(retainedBaseDir, "base-marker"), []byte("base"), 0644))

	promotedExistingBase, err := prepareRetainedSnapshotTarget(snapshotDir, retainedBaseDir)
	require.NoError(t, err)
	require.True(t, promotedExistingBase, "test setup should promote the retained base into the snapshot target")

	_, err = os.Stat(retainedBaseDir)
	assert.True(t, os.IsNotExist(err), "promotion should move the retained base out of its hidden location")

	// Simulate a partially written snapshot target before the snapshot API returns an error.
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "partial-marker"), []byte("partial"), 0644))

	require.NoError(t, discardPromotedRetainedSnapshotTarget(snapshotDir))

	_, err = os.Stat(snapshotDir)
	assert.True(t, os.IsNotExist(err), "snapshot failures should discard the promoted snapshot target")
	_, err = os.Stat(retainedBaseDir)
	assert.True(t, os.IsNotExist(err), "snapshot failures should not restore the promoted base for reuse")
}

func TestPrepareRetainedSnapshotTargetDiscardsStaleSnapshotDirBeforeRetry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	snapshotDir := filepath.Join(root, "snapshot-latest")
	retainedBaseDir := filepath.Join(root, "snapshot-base")

	require.NoError(t, os.MkdirAll(snapshotDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "memory"), []byte("partial"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "state"), []byte("partial"), 0644))

	promotedExistingBase, err := prepareRetainedSnapshotTarget(snapshotDir, retainedBaseDir)
	require.NoError(t, err)
	assert.False(t, promotedExistingBase, "stale snapshot cleanup should not report a promoted retained base")

	_, err = os.Stat(snapshotDir)
	assert.True(t, os.IsNotExist(err), "stale snapshot targets should be discarded before retrying standby")

	_, err = os.Stat(retainedBaseDir)
	assert.True(t, os.IsNotExist(err), "cleanup without a retained base should leave the retained base location empty")
}
