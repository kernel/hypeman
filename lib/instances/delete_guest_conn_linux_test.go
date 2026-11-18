package instances

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// Delete closes the pooled guest-agent connection before the graceful shutdown
// RPC, and the RPC opens it again. Delete must close that one too, or it stays
// pooled for the life of the process.
func TestDeleteClosesGuestConn(t *testing.T) {
	m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)

	// The graceful shutdown RPC is only sent to a live hypervisor, so a real
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

	var conn *grpc.ClientConn
	m.shutdownGuestFn = func(ctx context.Context, dialer hypervisor.VsockDialer, _ int32) error {
		c, err := guest.GetOrCreateConn(ctx, dialer)
		if err != nil {
			return err
		}
		conn = c
		// The owner exits when its stdin closes, like a guest powering off.
		return stdin.Close()
	}

	require.NoError(t, m.DeleteInstance(context.Background(), id))

	require.NotNil(t, conn, "graceful shutdown RPC was not sent")
	require.Eventually(t, func() bool {
		return conn.GetState() == connectivity.Shutdown
	}, 5*time.Second, 10*time.Millisecond)
}
