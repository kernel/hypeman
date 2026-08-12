//go:build linux

package instances

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackfillHypervisorProcessIdentitiesPersistsConfirmedOwnership(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	selfPID := os.Getpid()
	seedInstance(t, mgr, StoredMetadata{
		Id:            "inst-live",
		HypervisorPID: &selfPID,
		SocketPath:    socketPath,
	})

	mgr.BackfillHypervisorProcessIdentities(context.Background())

	meta, err := mgr.loadMetadata("inst-live")
	require.NoError(t, err)
	require.NotNil(t, meta.HypervisorPID)
	assert.Equal(t, selfPID, *meta.HypervisorPID)
	assert.Equal(t, processStartTime(selfPID), meta.HypervisorStartTime)
	assert.Equal(t, hostBootID(), meta.HypervisorBootID)
}

func TestBackfillHypervisorProcessIdentitiesSkipsDeadPID(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	dead := exec.Command("true")
	require.NoError(t, dead.Run())
	deadPID := dead.Process.Pid

	seedInstance(t, mgr, StoredMetadata{
		Id:            "inst-dead",
		HypervisorPID: &deadPID,
		SocketPath:    filepath.Join(t.TempDir(), "missing.sock"),
	})
	before := instanceMetadataBytes(t, mgr, "inst-dead")

	mgr.BackfillHypervisorProcessIdentities(context.Background())

	assert.Equal(t, before, instanceMetadataBytes(t, mgr, "inst-dead"))
}

func TestBackfillHypervisorProcessIdentitiesSkipsAlreadyStamped(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	selfPID := os.Getpid()
	seedInstance(t, mgr, StoredMetadata{
		Id:                  "inst-stamped",
		HypervisorPID:       &selfPID,
		HypervisorStartTime: processStartTime(selfPID),
		HypervisorBootID:    hostBootID(),
		SocketPath:          filepath.Join(t.TempDir(), "missing.sock"),
	})
	before := instanceMetadataBytes(t, mgr, "inst-stamped")

	mgr.BackfillHypervisorProcessIdentities(context.Background())

	assert.Equal(t, before, instanceMetadataBytes(t, mgr, "inst-stamped"))
}

func TestBackfillHypervisorProcessIdentitiesSkipsMissingSocketOwner(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	stale := startSleep(t)
	stalePID := stale.Process.Pid
	seedInstance(t, mgr, StoredMetadata{
		Id:            "inst-no-owner",
		HypervisorPID: &stalePID,
		SocketPath:    filepath.Join(t.TempDir(), "missing.sock"),
	})
	before := instanceMetadataBytes(t, mgr, "inst-no-owner")

	mgr.BackfillHypervisorProcessIdentities(context.Background())

	assert.Equal(t, before, instanceMetadataBytes(t, mgr, "inst-no-owner"))
	assert.NoError(t, syscall.Kill(stalePID, 0))
}

func TestBackfillHypervisorProcessIdentitiesSkipsUnconfirmedCommandLineMatch(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	match := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
	require.NoError(t, match.Start())
	t.Cleanup(func() {
		_ = match.Process.Kill()
		_ = match.Wait()
	})
	stale := startSleep(t)
	stalePID := stale.Process.Pid
	seedInstance(t, mgr, StoredMetadata{
		Id:            "inst-cmdline",
		HypervisorPID: &stalePID,
		SocketPath:    socketPath,
	})
	before := instanceMetadataBytes(t, mgr, "inst-cmdline")

	mgr.BackfillHypervisorProcessIdentities(context.Background())

	assert.Equal(t, before, instanceMetadataBytes(t, mgr, "inst-cmdline"))
	assert.NoError(t, syscall.Kill(stalePID, 0), "stored process must not be killed")
	assert.NoError(t, syscall.Kill(match.Process.Pid, 0), "command-line match must not be killed")
}

func TestBackfillHypervisorProcessIdentitiesSkipsMissingPIDOrSocket(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	selfPID := os.Getpid()
	seedInstance(t, mgr, StoredMetadata{
		Id:         "inst-no-pid",
		SocketPath: filepath.Join(t.TempDir(), "missing.sock"),
	})
	seedInstance(t, mgr, StoredMetadata{
		Id:            "inst-no-socket",
		HypervisorPID: &selfPID,
	})
	beforeNoPID := instanceMetadataBytes(t, mgr, "inst-no-pid")
	beforeNoSocket := instanceMetadataBytes(t, mgr, "inst-no-socket")

	mgr.BackfillHypervisorProcessIdentities(context.Background())

	assert.Equal(t, beforeNoPID, instanceMetadataBytes(t, mgr, "inst-no-pid"))
	assert.Equal(t, beforeNoSocket, instanceMetadataBytes(t, mgr, "inst-no-socket"))
}

func TestBackfillHypervisorProcessIdentitiesAdoptsConfirmedSocketOwner(t *testing.T) {
	mgr := &manager{paths: paths.New(t.TempDir())}
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	stale := startSleep(t)
	stalePID := stale.Process.Pid
	seedInstance(t, mgr, StoredMetadata{
		Id:            "inst-adopt",
		HypervisorPID: &stalePID,
		SocketPath:    socketPath,
	})

	mgr.BackfillHypervisorProcessIdentities(context.Background())

	meta, err := mgr.loadMetadata("inst-adopt")
	require.NoError(t, err)
	require.NotNil(t, meta.HypervisorPID)
	assert.Equal(t, os.Getpid(), *meta.HypervisorPID)
	assert.Equal(t, processStartTime(os.Getpid()), meta.HypervisorStartTime)
	assert.Equal(t, hostBootID(), meta.HypervisorBootID)
	assert.NoError(t, syscall.Kill(stalePID, 0), "unrelated stored process must not be killed")
}

func seedInstance(t *testing.T, mgr *manager, stored StoredMetadata) {
	t.Helper()
	require.NoError(t, mgr.ensureDirectories(stored.Id))
	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: stored}))
}

func instanceMetadataBytes(t *testing.T, mgr *manager, id string) []byte {
	t.Helper()
	data, err := os.ReadFile(mgr.paths.InstanceMetadata(id))
	require.NoError(t, err)
	return data
}

func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}
