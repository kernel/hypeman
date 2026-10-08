//go:build darwin && !cgo

package main

import (
	"fmt"
	"net"
)

const defaultReadyFilePath = "/var/run/hypeman/guest-agent-ready"

func listenVsock(uint32) (net.Listener, error) {
	return nil, fmt.Errorf("Darwin guest vsock requires a cgo-enabled build")
}
