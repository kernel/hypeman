//go:build darwin

package ingress

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCaddyPIDFromPS(t *testing.T) {
	out := []byte(`  101 /usr/sbin/sshd
  202 /opt/caddy run --config /other/caddy.json
  303 /Users/me/bin/caddy run --config /data/caddy/caddy.json --resume
`)
	require.Equal(t, 303, caddyPIDFromPS(out, "/data/caddy/caddy.json"))
	require.Zero(t, caddyPIDFromPS(out, "/missing/caddy.json"))
}

func TestIsProcessRunningOnDarwin(t *testing.T) {
	d := &CaddyDaemon{}
	require.True(t, d.isProcessRunning(os.Getpid()))

	cmd := exec.Command("/bin/sleep", "60")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.True(t, d.isProcessRunning(pid))
	require.NoError(t, cmd.Process.Kill())
	// Not yet reaped, so the process is a zombie.
	require.Eventually(t, func() bool { return !d.isProcessRunning(pid) }, 5*time.Second, 10*time.Millisecond, "a zombie is not running")
	_ = cmd.Wait()
	require.False(t, d.isProcessRunning(pid))
}
