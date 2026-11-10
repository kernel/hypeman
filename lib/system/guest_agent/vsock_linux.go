//go:build linux

package main

import (
	"net"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

func listenVsock(port uint32) (net.Listener, error) {
	// A restored guest may receive a new CID; keep its listening socket valid.
	return vsock.ListenContextID(unix.VMADDR_CID_ANY, port, nil)
}
