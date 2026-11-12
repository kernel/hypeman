//go:build linux

package hypervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

	pid, err := ResolveProcessPID(socketPath)
	require.NoError(t, err)
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

	pid, err := ResolveProcessPID(socketPath)
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), pid)
}

func TestResolveProcessPIDFailsForDuplicateSocketPaths(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte(
		"00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"+
			"00000000: 00000002 00000000 00010000 0001 01 67890 "+socketPath+"\n"), 0o644))

	_, err := ResolveProcessPID(socketPath)
	require.ErrorContains(t, err, "multiple socket inodes found")
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

	pid, err := ResolveProcessPID(socketPath)
	require.NoError(t, err)
	require.Equal(t, 200, pid)
}

func TestResolveProcessPIDForOwnerPrefersExpectedProcess(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))
	for _, pid := range []string{"100", "101"} {
		fdDir := filepath.Join(procDir, pid, "fd")
		require.NoError(t, os.MkdirAll(fdDir, 0o755))
		require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(fdDir, "3")))
	}

	_, err := ResolveProcessPID(socketPath)
	require.ErrorContains(t, err, "multiple owning processes found")

	pid, err := ResolveProcessPIDForOwner(socketPath, 101)
	require.NoError(t, err)
	require.Equal(t, 101, pid)
}

func TestPidBySocketRefPrefersExpectedOwnerAmongMultiple(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	// The full scan itself must prefer the expected owner when a child
	// transiently shares the inherited listener fd: the fast path can miss on
	// a transient fd-dir read failure, and the scan's observation of the
	// owner holding the fd is the same evidence the fast path would have used.
	for _, pid := range []string{"100", "101"} {
		fdDir := filepath.Join(procDir, pid, "fd")
		require.NoError(t, os.MkdirAll(fdDir, 0o755))
		require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(fdDir, "3")))
	}

	match := func(_ int, target string) (bool, error) { return target == "socket:[12345]", nil }
	pid, err := pidBySocket("/tmp/test.sock", match, 101)
	require.NoError(t, err)
	require.Equal(t, 101, pid)

	_, err = pidBySocket("/tmp/test.sock", match, 0)
	require.ErrorContains(t, err, "multiple owning processes found")

	_, err = pidBySocket("/tmp/test.sock", match, 999)
	require.ErrorContains(t, err, "multiple owning processes found")
}

func TestResolveProcessPIDReportsNoOwnerAfterExitedProcesses(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "100"), 0o755))

	_, err := ResolveProcessPID(socketPath)
	require.ErrorIs(t, err, ErrNoOwningProcess)
	require.NotContains(t, err.Error(), "inspect process fds")
}

func TestResolveProcessPIDMatchesSocketInProcessRoot(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	// Two jailed VMMs each bind /api.sock inside their own root, so both
	// listeners have the same path in /proc/net/unix. Each instance's host
	// socket path is a link to its own jail's socket.
	base, err := os.MkdirTemp("/tmp", "spid")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	var netUnix string
	for i, pid := range []string{"100", "101"} {
		root := filepath.Join(base, "jail"+pid)
		require.NoError(t, os.MkdirAll(root, 0o755))
		listener, err := net.Listen("unix", filepath.Join(root, "api.sock"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		require.NoError(t, os.Symlink(filepath.Join(root, "api.sock"), filepath.Join(base, "fc"+pid+".sock")))

		inode := strconv.Itoa(5000 + i)
		netUnix += "00000000: 00000002 00000000 00010000 0001 01 " + inode + " /api.sock\n"
		require.NoError(t, os.MkdirAll(filepath.Join(procDir, pid, "fd"), 0o755))
		require.NoError(t, os.Symlink("socket:["+inode+"]", filepath.Join(procDir, pid, "fd", "3")))
		require.NoError(t, os.Symlink(root, filepath.Join(procDir, pid, "root")))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte(netUnix), 0o644))

	for _, pid := range []int{100, 101} {
		got, err := ResolveProcessPID(filepath.Join(base, "fc"+strconv.Itoa(pid)+".sock"))
		require.NoError(t, err)
		require.Equal(t, pid, got)
	}

	// A process whose root does not hold the file is not the owner.
	require.NoError(t, os.Remove(filepath.Join(procDir, "101", "root")))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(procDir, "101", "root")))
	_, err = ResolveProcessPID(filepath.Join(base, "fc101.sock"))
	require.ErrorIs(t, err, ErrNoOwningProcess)
}

func TestResolveProcessPIDSkipsProcessScanForSocketWithoutListener(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	// A VMM that died without unlinking its socket leaves the file behind.
	socketPath := filepath.Join(t.TempDir(), "fc.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())

	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), nil, 0o644))
	// A process scan would fail on this unreadable fd instead of reporting no owner.
	fdDir := filepath.Join(procDir, "123", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fdDir, "3"), nil, 0o644))

	_, err = ResolveProcessPID(socketPath)
	require.ErrorIs(t, err, ErrNoOwningProcess)
}

func TestResolveProcessPIDReportsMissingSocket(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), nil, 0o644))

	_, err := ResolveProcessPID("/tmp/missing.sock")
	require.ErrorIs(t, err, ErrNoOwningProcess)
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

	_, err := ResolveProcessPID(socketPath)
	require.Error(t, err)
	require.ErrorContains(t, err, "inspect process fds")
	require.False(t, errors.Is(err, ErrNoOwningProcess))
}

func TestResolveProcessPIDForOwnerConfirmsCandidateWithoutFullScan(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))

	fdDir := filepath.Join(procDir, "100", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(fdDir, "3")))

	// An unreadable sibling fd must not block confirming the candidate.
	siblingFDDir := filepath.Join(procDir, "123", "fd")
	require.NoError(t, os.MkdirAll(siblingFDDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(siblingFDDir, "3"), nil, 0o644))

	pid, err := ResolveProcessPIDForOwner(socketPath, 100)
	require.NoError(t, err)
	require.Equal(t, 100, pid)
}

func TestResolveProcessPIDForOwnerSkipsUnreadableCandidateFD(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))

	// An unreadable fd before the listener fd must not abort the candidate
	// check; the scan skips it and still finds the match.
	fdDir := filepath.Join(procDir, "100", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fdDir, "1"), nil, 0o644))
	require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(fdDir, "3")))

	pid, err := ResolveProcessPIDForOwner(socketPath, 100)
	require.NoError(t, err)
	require.Equal(t, 100, pid)
}

func TestResolveProcessPIDForOwnerFallsThroughWhenCandidateLacksSocket(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))

	candidateFDDir := filepath.Join(procDir, "999", "fd")
	require.NoError(t, os.MkdirAll(candidateFDDir, 0o755))
	require.NoError(t, os.Symlink("socket:[99999]", filepath.Join(candidateFDDir, "3")))

	ownerFDDir := filepath.Join(procDir, "200", "fd")
	require.NoError(t, os.MkdirAll(ownerFDDir, 0o755))
	require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(ownerFDDir, "3")))

	pid, err := ResolveProcessPIDForOwner(socketPath, 999)
	require.NoError(t, err)
	require.Equal(t, 200, pid)
}

func TestResolveProcessPIDForOwnerFallsThroughWhenCandidateIsGone(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644))

	ownerFDDir := filepath.Join(procDir, "200", "fd")
	require.NoError(t, os.MkdirAll(ownerFDDir, 0o755))
	require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(ownerFDDir, "3")))

	pid, err := ResolveProcessPIDForOwner(socketPath, 999)
	require.NoError(t, err)
	require.Equal(t, 200, pid)
}

func TestResolveProcessPIDForOwnerReportsMissingSocket(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("00000000: 00000002 00000000 00010000 0001 01 12345 /tmp/other.sock\n"), 0o644))

	_, err := ResolveProcessPIDForOwner("/tmp/missing.sock", 100)
	require.ErrorIs(t, err, ErrNoOwningProcess)
}

func TestResolveProcessPIDForOwnerReportsMissingSocketWithHeaderOnlyUnixTable(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte("Num       RefCount Protocol Flags    Type St Inode Path\n"), 0o644))

	_, err := ResolveProcessPIDForOwner("/tmp/missing.sock", 100)
	require.ErrorIs(t, err, ErrNoOwningProcess)
}

func TestResolveProcessPIDForOwnerReportsDuplicateSocketInodes(t *testing.T) {
	oldProcDir := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = oldProcDir })

	socketPath := "/tmp/test.sock"
	require.NoError(t, os.MkdirAll(filepath.Join(procDir, "net"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "net", "unix"), []byte(
		"00000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"+
			"00000000: 00000002 00000000 00010000 0001 01 67890 "+socketPath+"\n"), 0o644))

	fdDir := filepath.Join(procDir, "100", "fd")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.Symlink("socket:[12345]", filepath.Join(fdDir, "3")))

	_, err := ResolveProcessPIDForOwner(socketPath, 100)
	require.ErrorContains(t, err, "multiple socket inodes found")
}

func TestResolveProcessPIDForOwnerConfirmsLiveListener(t *testing.T) {
	tmpDir := t.TempDir()
	socketPath := filepath.Join(tmpDir, "test.sock")

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	pid, err := ResolveProcessPIDForOwner(socketPath, os.Getpid())
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), pid)
}

func TestResolveProcessPIDIgnoresCommandLineBystander(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o600))

	// A process carrying the socket path in its command line (e.g. a debug
	// client like ch-remote) without holding the listener must not resolve
	// as the owner; a missing listener is proof the hypervisor is gone.
	bystander := exec.Command("sh", "-c", "sleep 30", "sh", socketPath)
	require.NoError(t, bystander.Start())
	t.Cleanup(func() {
		_ = bystander.Process.Kill()
		_ = bystander.Wait()
	})

	_, err := ResolveProcessPID(socketPath)
	require.ErrorIs(t, err, ErrNoOwningProcess)
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

	// Resolve without an owner hint so every iteration runs the full /proc
	// scan; the owner fast path never exercises the churn tolerance.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pid, err := ResolveProcessPID(socketPath)
		require.NoError(t, err)
		require.Equal(t, os.Getpid(), pid)
	}
}
