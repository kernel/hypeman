//go:build linux

package hypervisor

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveProcessPID(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	pid, err := ResolveProcessPID(socketPath)
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), pid)
}
