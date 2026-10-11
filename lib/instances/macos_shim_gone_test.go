package instances

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances/phasetracking"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/stretchr/testify/require"
)

const shimGoneHypervisorType hypervisor.Type = "shim-gone-test"

func init() {
	hypervisor.RegisterClientFactory(shimGoneHypervisorType, func(socketPath string) (hypervisor.Hypervisor, error) {
		conn, err := net.Dial("unix", socketPath)
		if err != nil {
			return nil, err
		}
		conn.Close()
		return &lifecycleNoopHypervisor{state: hypervisor.StateRunning}, nil
	})
}

// staleSocket leaves a socket file that nothing listens on, as a SIGKILLed shim does.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, l.Close())
}

func TestMacOSKilledShimIsStopped(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p, admissionAllocationsLoaded: true, admissionAllocations: map[string]resources.InstanceAllocation{}}
	id := "killed-shim"
	require.NoError(t, m.ensureDirectories(id))
	// macOS limits socket paths to 104 bytes, more than its TMPDIR leaves room for.
	socketDir, err := os.MkdirTemp("/tmp", "hm-shim-")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "vz.sock")
	staleSocket(t, socket)
	deadPID := 1 << 22
	stored := &StoredMetadata{
		Id: id, DataDir: p.InstanceDir(id), SocketPath: socket, HypervisorType: shimGoneHypervisorType, Size: 8 << 30,
		MacOS: &images.MacOSImage{}, HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: &deadPID},
	}
	m.setAdmissionAllocationActive(stored, true)
	allocations, err := m.ListInstanceAllocations(context.Background())
	require.NoError(t, err)
	require.Equal(t, string(StateCreated), allocations[0].State)
	require.False(t, admissionSocketActive(stored), "the reconciler does not count a refusing socket")

	require.Equal(t, StateStopped, m.deriveStateWithoutHydration(context.Background(), stored).State)
	allocations, err = m.ListInstanceAllocations(context.Background())
	require.NoError(t, err)
	require.Equal(t, string(StateStopped), allocations[0].State, "a killed shim's memory is released for admission")

	require.NoError(t, os.MkdirAll(filepath.Join(p.InstanceDir(id), "snapshots", "snapshot-latest"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(p.InstanceDir(id), "snapshots", "snapshot-latest", "state"), []byte("x"), 0644))
	require.Equal(t, StateStandby, m.deriveStateWithoutHydration(context.Background(), stored).State)

	livePID := os.Getpid()
	stored.HypervisorPID = &livePID
	require.Equal(t, StateUnknown, m.deriveStateWithoutHydration(context.Background(), stored).State, "a live shim is not presumed stopped")

	stored.HypervisorPID = &deadPID
	stored.MacOS = nil
	require.Equal(t, StateUnknown, m.deriveStateWithoutHydration(context.Background(), stored).State, "Linux guests are unchanged")
}

func TestMacOSShimGoneRequiresRefusedConnection(t *testing.T) {
	stored := &StoredMetadata{MacOS: &images.MacOSImage{}}
	require.True(t, macOSShimGone(stored, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}))
	require.False(t, macOSShimGone(stored, context.DeadlineExceeded))
	require.False(t, macOSShimGone(stored, nil))
}

func TestFinishMacOSResumeRemovesConsumedSnapshot(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	id := "resumed"
	require.NoError(t, m.ensureDirectories(id))
	image := &images.MacOSImage{GuestAgent: true}
	ready := time.Now().UTC().Add(-time.Minute)
	meta := &metadata{StoredMetadata: StoredMetadata{Id: id, DataDir: p.InstanceDir(id), MacOS: image, GuestAgentReadyAt: &ready}}
	meta.Phases.Record(phasetracking.PhaseStandby, ready)
	require.NoError(t, m.saveMetadata(meta))
	snapshot := p.InstanceSnapshotLatest(id)
	require.NoError(t, os.MkdirAll(snapshot, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(snapshot, "state"), []byte("x"), 0644))

	require.NoError(t, m.finishMacOSResume(id))

	require.NoDirExists(t, snapshot, "a consumed memory snapshot must not survive the resume")
	saved, err := m.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, phasetracking.PhaseRunning, saved.Phases.Current)
}
