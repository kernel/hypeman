package instances

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

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
