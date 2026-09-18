//go:build linux

package qemu

import (
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func vsockPair(t *testing.T) (*vsockConn, *os.File, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	require.NoError(t, err)
	peer := os.NewFile(uintptr(fds[1]), "peer")
	t.Cleanup(func() { _ = peer.Close() })
	conn, err := newVsockConn(fds[0], 42, 1024)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn, peer, fds[0]
}

func TestVsockConnDoesNotCloseReusedDescriptor(t *testing.T) {
	conn, _, fd := vsockPair(t)
	directory, err := os.Open("/proc/self/fd")
	require.NoError(t, err)
	defer directory.Close()
	require.NoError(t, conn.Close())
	reused, err := unix.FcntlInt(directory.Fd(), unix.F_DUPFD_CLOEXEC, fd)
	require.NoError(t, err)
	victim := os.NewFile(uintptr(reused), "proc-fds")
	defer victim.Close()
	if reused != fd {
		t.Skip("another goroutine reused the descriptor first")
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _ = conn.Close() })
	}
	wg.Wait()
	_, err = victim.ReadDir(-1)
	require.NoError(t, err, "repeated close must not close an unrelated directory")

	n, err := conn.Read(make([]byte, 1))
	require.Zero(t, n)
	require.ErrorIs(t, err, os.ErrClosed)
	n, err = conn.Write([]byte("x"))
	require.Zero(t, n)
	require.ErrorIs(t, err, os.ErrClosed)
	require.Error(t, conn.SetDeadline(time.Now()))
}

func TestVsockConnReadWrite(t *testing.T) {
	conn, peer, _ := vsockPair(t)
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Second)))
	_, err := peer.Write([]byte("in"))
	require.NoError(t, err)
	buf := make([]byte, 2)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "in", string(buf))
	_, err = conn.Write([]byte("out"))
	require.NoError(t, err)
	buf = make([]byte, 3)
	_, err = io.ReadFull(peer, buf)
	require.NoError(t, err)
	require.Equal(t, "out", string(buf))
	require.NoError(t, peer.Close())
	n, err := conn.Read(buf)
	require.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)
}

func TestVsockConnCloseInterruptsRead(t *testing.T) {
	conn, _, _ := vsockPair(t)
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := conn.Read(make([]byte, 1))
		result <- err
	}()
	<-started
	// Give the reader time to block before closing the connection.
	select {
	case err := <-result:
		t.Fatalf("read returned before close: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	require.NoError(t, conn.Close())
	select {
	case err := <-result:
		require.ErrorIs(t, err, os.ErrClosed)
	case <-time.After(time.Second):
		t.Fatal("close did not interrupt read")
	}
}

func TestVsockConnDeadlines(t *testing.T) {
	conn, peer, _ := vsockPair(t)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(-time.Second)))
	_, err := conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	_, err = peer.Write([]byte("x"))
	require.NoError(t, err)
	_, err = conn.Read(make([]byte, 1))
	require.NoError(t, err)
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(-time.Second)))
	_, err = conn.Write([]byte("x"))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	require.NoError(t, conn.SetDeadline(time.Time{}))
	_, err = conn.Write([]byte("x"))
	require.NoError(t, err)
}
