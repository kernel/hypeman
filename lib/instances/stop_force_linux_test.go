package instances

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForceStopSkipsGuestShutdown(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)

	// The guest shutdown RPC is only sent to a live hypervisor, so a real
	// process has to own the instance socket.
	require.NoError(t, os.Remove(meta.SocketPath))
	owner := exec.Command(os.Args[0], "-test.run=^TestSocketListenerHelper$")
	owner.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+meta.SocketPath)
	stdin, err := owner.StdinPipe()
	require.NoError(t, err)
	stdout, err := owner.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, owner.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = owner.Process.Kill()
		_ = owner.Wait()
	})
	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	meta.HypervisorProcessIdentity.Set(owner.Process.Pid)
	require.NoError(t, m.saveMetadata(meta))

	guestShutdownSent := false
	m.shutdownGuestFn = func(context.Context, hypervisor.VsockDialer, int32) error {
		guestShutdownSent = true
		return stdin.Close()
	}

	inst, err := m.StopInstance(context.Background(), id, StopInstanceRequest{Force: true})
	require.NoError(t, err)
	assert.False(t, guestShutdownSent, "forced stop must not ask the guest to shut down")
	assert.Equal(t, StateStopped, inst.State)
}
