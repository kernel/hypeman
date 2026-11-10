//go:build linux

package main

import (
	"errors"
	"testing"

	"github.com/mdlayher/vsock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestVsockListenerDoesNotPinTheBootCID(t *testing.T) {
	listener, err := listenVsock(0)
	if errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("vsock is unavailable: %v", err)
	}
	require.NoError(t, err)
	defer listener.Close()
	addr, ok := listener.Addr().(*vsock.Addr)
	require.True(t, ok)
	require.Equal(t, uint32(unix.VMADDR_CID_ANY), addr.ContextID)
}
