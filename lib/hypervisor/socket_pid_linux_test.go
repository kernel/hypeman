//go:build linux

package hypervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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

func TestResolveProcessPIDIgnoresConnectedSocketEntries(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	// Accepted server-side sockets share the listener's path in
	// /proc/net/unix; they must not make the listener's inode ambiguous.
	conn, err := net.Dial("unix", socketPath)
	require.NoError(t, err)
	defer conn.Close()
	accepted, err := listener.Accept()
	require.NoError(t, err)
	defer accepted.Close()

	pid, confirmed, err := ResolveProcessPID(socketPath)
	require.NoError(t, err)
	require.True(t, confirmed)
	require.Equal(t, os.Getpid(), pid)
}

func TestResolveProcessPIDIsUnconfirmedForDuplicateSocketPaths(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte(
		"00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"+
			"00000000: 00000002 00000000 00010000 0001 01 67890 "+socketPath+"\n"), 0o644))

	_, confirmed, err := ResolveProcessPID(socketPath)
	require.ErrorContains(t, err, "multiple socket inodes found")
	require.False(t, confirmed)
}

func TestResolveProcessPIDToleratesExitedProcess(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "100"), 0o755))
	fdDir := filepath.Join(procDir, "200", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(fdDir, "3")))

	pid, confirmed, err := ResolveProcessPID(socketPath)
	require.NoError(t, err)
	require.True(t, confirmed)
	require.Equal(t, 200, pid)
}

func TestResolveProcessPIDReportsNoOwnerAfterExitedProcesses(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "100"), 0o755))

	_, confirmed, err := ResolveProcessPID(socketPath)
	require.ErrorIs(t, err, ErrNoOwningProcess)
	require.False(t, confirmed)
	require.NotContains(t, err.Error(), "inspect process fds")
}

func TestResolveProcessPIDReportsMissingSocket(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), nil, 0o644))

	_, confirmed, err := ResolveProcessPID("/tmp/missing.sock")
	require.ErrorIs(t, err, ErrNoOwningProcess)
	require.False(t, confirmed)
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
	require.False(t, errors.Is(err, ErrNoOwningProcess))
}

func TestResolveProcessPIDDuringProcessChurn(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			_ = exec.CommandContext(ctx, "/bin/true").Run()
		}
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pid, confirmed, err := ResolveProcessPID(socketPath)
		require.NoError(t, err)
		require.True(t, confirmed)
		require.Equal(t, os.Getpid(), pid)
	}
}
