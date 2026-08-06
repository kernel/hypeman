//go:build linux

package instances

import (
	"bufio"
	"fmt"
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

func TestHypervisorProcessExistsWithReboundSocketPathHelper(t *testing.T) {
	if os.Getenv("HYPERVISOR_SOCKET_HELPER") != "1" {
		return
	}

	listener, err := net.Listen("unix", os.Getenv("HYPERVISOR_SOCKET_PATH"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer listener.Close()
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = os.Stdin.Read(make([]byte, 1))
}

func TestHypervisorProcessExistsTreatsReboundSocketPathAsAlive(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	process := exec.Command(os.Args[0], "-test.run=^TestHypervisorProcessExistsWithReboundSocketPathHelper$")
	process.Env = append(os.Environ(), "HYPERVISOR_SOCKET_HELPER=1", "HYPERVISOR_SOCKET_PATH="+socketPath)
	stdin, err := process.StdinPipe()
	require.NoError(t, err)
	stdout, err := process.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, process.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = process.Wait()
	})

	_, err = bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.NoError(t, os.Remove(socketPath))
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	assert.True(t, HypervisorProcessExists(os.Getpid(), socketPath))
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
