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

	pid, confirmed, err := ResolveProcessPID(socketPath)
	require.NoError(t, err)
	require.True(t, confirmed)
	require.Equal(t, os.Getpid(), pid)
}

func TestResolveProcessPIDFailsWhenFDIsUnreadable(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))
	fdDir := filepath.Join(procDir, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fdDir, "3"), nil, 0o644))

	_, confirmed, err := ResolveProcessPID(socketPath)
	require.Error(t, err)
	require.False(t, confirmed)
	require.ErrorContains(t, err, "inspect process fds")
}
