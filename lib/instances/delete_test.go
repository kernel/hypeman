package instances

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForProcessExit_ReapsZombieChild(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())

	exited := WaitForProcessExit(cmd.Process.Pid, 500*time.Millisecond)
	require.True(t, exited, "zombie child should be detected/reaped as exited")
}

func TestWaitForProcessExit_EPERMProcessIsAlive(t *testing.T) {
	t.Parallel()
	if syscall.Kill(1, 0) == nil {
		t.Skip("running as root")
	}

	assert.False(t, WaitForProcessExit(1, 100*time.Millisecond))
}

func TestWaitForProcessExit_TimesOutForRunningProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "2")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	exited := WaitForProcessExit(cmd.Process.Pid, 100*time.Millisecond)
	assert.False(t, exited, "running process should time out")
}
