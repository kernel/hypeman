package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

const maxConcurrentPortForwards = 64

// Port forward protocol, matching lib/guest/port_forward.go. The builder image
// compiles this agent with only the generated files from lib/guest.
const (
	portForwardVsockPort        = 2223
	portForwardStatusOK         = byte(0)
	portForwardStatusDialFailed = byte(1)
)

// startPortForwards listens on the port forward vsock port and connects each
// host connection to the TCP port it names on the guest's loopback interface.
// The host API requires instance-write access, which also grants exec, so the
// forwarder does not restrict which loopback ports it reaches.
func startPortForwards() {
	go func() {
		var l net.Listener
		var err error
		for attempt := 1; attempt <= 10; attempt++ {
			if l, err = listenVsock(portForwardVsockPort); err == nil {
				break
			}
			log.Printf("[guest-agent] port forward listen attempt %d/10 failed: %v", attempt, err)
			time.Sleep(time.Second)
		}
		if err != nil {
			log.Printf("[guest-agent] port forwarding unavailable: %v", err)
			return
		}
		log.Printf("[guest-agent] port forwarding on vsock port %d", portForwardVsockPort)
		servePortForwards(l, dialLoopback)
	}()
}

// servePortForwards accepts until the listener closes. Other accept errors, such as
// running out of file descriptors, are retried so forwarding recovers.
func servePortForwards(l net.Listener, dial func(context.Context, uint16) (net.Conn, error)) {
	defer l.Close()
	slots := make(chan struct{}, maxConcurrentPortForwards)
	backoff := 5 * time.Millisecond
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			log.Printf("[guest-agent] port forward accept: %v", err)
			time.Sleep(backoff)
			backoff = min(2*backoff, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				forwardGuestPort(conn, dial)
			}()
		default:
			conn.Close()
		}
	}
}

func dialLoopback(ctx context.Context, port uint16) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
}

// forwardGuestPort runs the handshake for one host connection, then copies bytes
// between it and the loopback port until either side closes.
func forwardGuestPort(host net.Conn, dial func(context.Context, uint16) (net.Conn, error)) {
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(5 * time.Second))
	var port uint16
	if err := binary.Read(host, binary.BigEndian, &port); err != nil {
		return
	}

	if port == 0 {
		_, _ = host.Write([]byte{portForwardStatusDialFailed})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	target, err := dial(ctx, port)
	cancel()
	if err != nil {
		_, _ = host.Write([]byte{portForwardStatusDialFailed})
		return
	}
	defer target.Close()
	if _, err := host.Write([]byte{portForwardStatusOK}); err != nil {
		return
	}
	_ = host.SetDeadline(time.Time{})

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, host)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(host, target)
		done <- struct{}{}
	}()
	// Either side closing ends the forward; closing both unblocks the other copy.
	<-done
	host.Close()
	target.Close()
	<-done
}
