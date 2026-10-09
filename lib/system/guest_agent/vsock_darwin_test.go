//go:build darwin && cgo

package main

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestVsockReadTimeoutImplementsNetError(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	if err = unix.SetNonblock(fds[0], true); err != nil {
		unix.Close(fds[0])
		t.Fatal(err)
	}
	conn := &vmConn{file: os.NewFile(uintptr(fds[0]), "test-vsock"), local: vmAddr("guest:2222")}
	defer conn.Close()
	if err = conn.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = conn.Read(make([]byte, 1))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("transport timeout must implement net.Error: %v", err)
	}
}
