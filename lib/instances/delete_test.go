package instances

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForProcessExit_ReapsZombieChild(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())

	start := time.Now()
	exited := WaitForProcessExit(cmd.Process.Pid, 500*time.Millisecond)
	elapsed := time.Since(start)

	require.True(t, exited, "zombie child should be detected/reaped as exited")
	assert.Less(t, elapsed, 250*time.Millisecond, "reaping should be quick")
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
