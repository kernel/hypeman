package instances

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecoverNetworklessForkBeforeShimListens(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStandby, time.Now())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.ForkPending = &ForkOwner{}
	require.NoError(t, m.saveMetadata(meta))

	shim := exec.Command("sleep", "60")
	require.NoError(t, shim.Start())
	t.Cleanup(func() { _ = shim.Process.Kill(); _ = shim.Wait() })
	prev := findVZShimPIDs
	t.Cleanup(func() { findVZShimPIDs = prev })
	lookupErr := errors.New("cannot inspect processes")
	findVZShimPIDs = func(path string) ([]int, error) {
		if lookupErr != nil || path != meta.SocketPath {
			return nil, lookupErr
		}
		return []int{shim.Process.Pid}, nil
	}

	m.RecoverInterruptedForks(t.Context())
	require.DirExists(t, meta.DataDir)
	require.True(t, ProcessExists(shim.Process.Pid))
	lookupErr = nil
	m.RecoverInterruptedForks(t.Context())
	require.False(t, ProcessExists(shim.Process.Pid))
	require.NoDirExists(t, meta.DataDir)
}

func TestDarwinTeardownRequiresMatchingShim(t *testing.T) {
	const socket = "/data/guests/fork/vz.sock"
	// The stored PID is live but belongs to this test, not to the guest.
	stored := os.Getpid()
	identity := HypervisorProcessIdentity{HypervisorPID: &stored}
	var matches []int
	var lookupErr error
	prev := findVZShimPIDs
	t.Cleanup(func() { findVZShimPIDs = prev })
	findVZShimPIDs = func(path string) ([]int, error) {
		require.Equal(t, socket, path)
		return matches, lookupErr
	}

	pid, err := resolveLiveHypervisorPID(identity, socket)
	require.Error(t, err)
	require.Zero(t, pid)

	matches = []int{123, 456}
	pid, err = resolveLiveHypervisorPID(identity, socket)
	require.Error(t, err)
	require.Zero(t, pid)

	matches = []int{123}
	pid, err = resolveLiveHypervisorPID(identity, socket)
	require.NoError(t, err)
	require.Equal(t, matches[0], pid)

	lookupErr = errors.New("cannot inspect processes")
	pid, err = resolveLiveHypervisorPID(identity, socket)
	require.ErrorIs(t, err, lookupErr)
	require.Zero(t, pid)
}
