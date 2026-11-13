package instances

import (
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A hypervisor that cannot be confirmed dead may still be running, so delete
// and stop must fail without releasing what is cached for it.
func TestFailedKillKeepsVMMState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action func(context.Context, *manager, string) error
	}{
		{name: "delete", action: func(ctx context.Context, m *manager, id string) error { return m.DeleteInstance(ctx, id) }},
		{name: "stop", action: func(ctx context.Context, m *manager, id string) error {
			_, err := m.StopInstance(ctx, id)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, id := newLifecycleNoopManagerWithInstance(t, StateRunning, time.Now().UTC())
			meta, err := m.loadMetadata(id)
			require.NoError(t, err)
			meta.HypervisorType = releaseVMMStateTestHypervisorType
			require.NoError(t, m.saveMetadata(meta))

			// Two processes hold the control socket, so its owner cannot be
			// confirmed and the kill fails without signalling either.
			require.NoError(t, os.Remove(meta.SocketPath))
			listener, err := net.Listen("unix", meta.SocketPath)
			require.NoError(t, err)
			defer listener.Close()
			inherited, err := listener.(*net.UnixListener).File()
			require.NoError(t, err)
			defer inherited.Close()
			second := exec.Command("sleep", "30")
			second.ExtraFiles = []*os.File{inherited}
			require.NoError(t, second.Start())
			t.Cleanup(func() {
				_ = second.Process.Kill()
				_ = second.Wait()
			})

			err = tc.action(context.Background(), m, id)
			require.ErrorContains(t, err, "cannot confirm ownership")

			_, released := releasedSockets.Load(meta.SocketPath)
			require.False(t, released)
		})
	}
}
