//go:build darwin && !cgo

package main

import (
	"fmt"
	"net"
)

func listenVsock(uint32) (net.Listener, error) {
	return nil, fmt.Errorf("Darwin guest vsock requires a cgo-enabled build")
}
