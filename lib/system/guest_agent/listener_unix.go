//go:build !windows

package main

import (
	"net"

	"github.com/mdlayher/vsock"
)

func listenVSock(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}

func defaultReadyFilePath() string {
	return "/run/hypeman/guest-agent-ready"
}
