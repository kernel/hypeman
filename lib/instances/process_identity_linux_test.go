//go:build linux

package instances

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHypervisorProcessExistsTreatsUnresolvedSocketAsAlive(t *testing.T) {
	t.Parallel()

	assert.True(t, HypervisorProcessExists(os.Getpid(), filepath.Join(t.TempDir(), "missing.sock")))
}

func TestHypervisorProcessExistsRejectsDifferentLiveSocketOwner(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	process := exec.Command("sleep", "30")
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})

	assert.False(t, HypervisorProcessExists(process.Process.Pid, socketPath))
}
