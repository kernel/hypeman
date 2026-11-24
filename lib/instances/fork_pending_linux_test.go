//go:build linux

package instances

import (
	"context"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A PID alone does not identify the process that was making a fork: it is
// handed out again after the process exits, and after a reboot.
func TestForkOwnerIsGoneWhenItsPIDBelongsToAnotherProcess(t *testing.T) {
	current := *currentForkOwner()
	require.NotZero(t, current.StartTime)
	require.NotEmpty(t, current.BootID)

	recycled := current
	recycled.StartTime++
	assert.True(t, recycled.gone(), "same PID, started at another time")

	beforeReboot := current
	beforeReboot.BootID = "00000000-0000-0000-0000-000000000000"
	assert.True(t, beforeReboot.gone(), "same PID and start time, recorded in an earlier boot")
}

func TestPCILivenessChecksOwnerOfUnresponsiveSocket(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	staleSocket(t, meta.SocketPath)
	require.Equal(t, StateUnknown, m.deriveStateWithoutHydration(t.Context(), &meta.StoredMetadata).State)
	checker := NewLivenessChecker(m)
	require.False(t, checker.IsInstanceRunning(t.Context(), id), "a stale socket has no owner")

	listen := func() {
		t.Helper()
		require.NoError(t, os.Remove(meta.SocketPath))
		l, err := net.Listen("unix", meta.SocketPath)
		require.NoError(t, err)
		t.Cleanup(func() { l.Close() })
	}
	listen()
	require.True(t, checker.IsInstanceRunning(t.Context(), id), "a live listener still protects the device")
	listen() // two listening inodes now have the same path in /proc/net/unix
	_, err = resolveLiveHypervisorPID(meta.HypervisorProcessIdentity, meta.SocketPath)
	require.ErrorContains(t, err, "multiple socket inodes")
	require.True(t, checker.IsInstanceRunning(t.Context(), id), "ambiguous ownership must also protect the device")
}

func TestRecoverNetworklessForkWithLivePIDAndNoSocket(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateStopped, time.Now())
	child := exec.Command("sleep", "60")
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.HypervisorProcessIdentity.Set(child.Process.Pid)
	meta.ForkPending = &ForkOwner{}
	require.NoError(t, m.saveMetadata(meta))

	require.True(t, NewLivenessChecker(m).IsInstanceRunning(t.Context(), id), "a missing socket is not proof the device is free")
	m.RecoverInterruptedForks(t.Context())
	require.False(t, ProcessExists(child.Process.Pid))
	require.NoDirExists(t, meta.DataDir)
}

// Nobody was given an interrupted fork, and the API does not listen until
// recovery returns, so recovery waits for a guest to shut down only when a
// passthrough device needs it to.
func TestRecoverInterruptedForksShutsDownOnlyGuestsWithPassthroughDevices(t *testing.T) {
	for _, tc := range []struct {
		name      string
		devices   []string
		shutdowns int
	}{
		{"no device", nil, 0},
		{"passthrough device", []string{"dev-1"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
			m.deviceManager = &recordingDeviceManager{}
			pid, identity := startTrapProcess(t, "exit 0")
			meta, err := m.loadMetadata(id)
			require.NoError(t, err)
			meta.HypervisorProcessIdentity = identity
			meta.Devices = tc.devices
			meta.ForkPending = exitedProcessOwner(t)
			require.NoError(t, m.saveMetadata(meta))
			shutdowns := 0
			m.shutdownGuestFn = func(context.Context, hypervisor.VsockDialer, int32) error {
				shutdowns++
				return syscall.Kill(pid, syscall.SIGKILL)
			}

			m.RecoverInterruptedForks(t.Context())
			assert.Equal(t, tc.shutdowns, shutdowns)
			assert.False(t, ProcessExists(pid))
			assert.NoDirExists(t, m.paths.InstanceDir(id))
		})
	}
}
