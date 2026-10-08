//go:build linux

package main

import (
	"net"

	"github.com/mdlayher/vsock"
)

const defaultReadyFilePath = "/run/hypeman/guest-agent-ready"

func listenVsock(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}
