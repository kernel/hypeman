//go:build darwin && cgo

package main

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
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

func TestVsockReadReturnsBareEOF(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := &vmConn{file: os.NewFile(uintptr(fds[0]), "test-vsock"), local: vmAddr("guest:2222")}
	defer conn.Close()
	unix.Close(fds[1])
	if _, err = conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("an orderly close must read as io.EOF, got %#v", err)
	}
}

func TestVsockAcceptErrorsRemainGRPCTemporary(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ECONNABORTED} {
		err := acceptError(errno)
		if ne, ok := err.(interface{ Temporary() bool }); !ok || !ne.Temporary() {
			t.Fatalf("%v: grpc direct Temporary assertion failed for %T", errno, err)
		}
		if !errors.Is(err, errno) {
			t.Fatalf("%v: lost errno", errno)
		}
	}
}
