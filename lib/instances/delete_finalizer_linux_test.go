//go:build linux

package instances

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPendingDeleteInstanceHiddenFromLookups(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	now := time.Now().UTC()
	seedInstance(t, mgr, StoredMetadata{Id: "inst-pending", Name: "web", PendingDeleteAt: &now})
	seedInstance(t, mgr, StoredMetadata{Id: "inst-live", Name: "api"})

	insts, err := mgr.listInstances(context.Background())
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, "inst-live", insts[0].Id)

	_, err = mgr.getInstance(context.Background(), "inst-pending")
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = mgr.findInstanceMetadataByExactName(context.Background(), "web")
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = mgr.findInstanceMetadataByNameOrIDPrefix("inst-pending", 1)
	assert.ErrorIs(t, err, ErrNotFound)

	exists, err := mgr.instanceNameExists(context.Background(), "web", "test")
	require.NoError(t, err)
	assert.False(t, exists, "pending-delete instance must not hold its name")

	ids, err := mgr.pendingDeleteInstanceIDs()
	require.NoError(t, err)
	assert.Equal(t, []string{"inst-pending"}, ids)
}

func TestDeferDeleteSetsMarkerOnce(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	seedInstance(t, mgr, StoredMetadata{Id: "inst-defer"})
	meta, err := mgr.loadMetadata("inst-defer")
	require.NoError(t, err)

	err = mgr.deferDelete(context.Background(), meta, errors.New("kill hypervisor: stuck"))
	require.ErrorIs(t, err, errDeleteDeferred)

	saved, err := mgr.loadMetadata("inst-defer")
	require.NoError(t, err)
	require.NotNil(t, saved.PendingDeleteAt)
	first := *saved.PendingDeleteAt

	err = mgr.deferDelete(context.Background(), saved, errors.New("still stuck"))
	require.ErrorIs(t, err, errDeleteDeferred)

	saved, err = mgr.loadMetadata("inst-defer")
	require.NoError(t, err)
	require.NotNil(t, saved.PendingDeleteAt)
	assert.True(t, saved.PendingDeleteAt.Equal(first), "deferral timestamp must not move on retries")
}

func TestDeleteDefersWhenHypervisorOwnershipUnconfirmed(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}

	// A socket path that exists (so state derivation lands on Unknown and the
	// kill path runs) but is not a live listener, plus a process matching it
	// by command line only, reproduces the unresolvable-ownership case where
	// delete used to fail.
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o644))
	match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
	require.NoError(t, match.Start())
	t.Cleanup(func() {
		_ = match.Process.Kill()
		_ = match.Wait()
	})

	seedInstance(t, mgr, StoredMetadata{Id: "inst-stuck", SocketPath: socketPath})

	err := mgr.deleteInstanceWithOptions(context.Background(), "inst-stuck", deleteInstanceOptions{skipGracefulShutdown: true})
	require.ErrorIs(t, err, errDeleteDeferred)

	meta, err := mgr.loadMetadata("inst-stuck")
	require.NoError(t, err)
	require.NotNil(t, meta.PendingDeleteAt)

	// A finalizer pass while the ambiguity persists defers again and keeps
	// the instance.
	mgr.finalizePendingDeletes(context.Background())
	_, err = mgr.loadMetadata("inst-stuck")
	require.NoError(t, err)

	// Once the matching process is gone the hypervisor is provably dead and
	// the next pass completes the teardown.
	require.NoError(t, match.Process.Kill())
	_ = match.Wait()

	mgr.finalizePendingDeletes(context.Background())
	_, err = mgr.loadMetadata("inst-stuck")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFinalizePendingDeleteRemovesStoppedInstance(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	now := time.Now().UTC()
	seedInstance(t, mgr, StoredMetadata{Id: "inst-final", PendingDeleteAt: &now})

	mgr.finalizePendingDeletes(context.Background())

	_, err := mgr.loadMetadata("inst-final")
	assert.ErrorIs(t, err, ErrNotFound)
}
