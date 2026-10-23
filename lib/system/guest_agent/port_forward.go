package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"runtime"
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

// portForwardSupported is true where the agent forwards guest ports. Linux guests
// reach their ports through ingress instead.
const portForwardSupported = runtime.GOOS == "darwin"

// startPortForwards listens on the port forward vsock port and connects each
// host connection to the TCP port it names on the guest's loopback interface.
// The host API requires instance-write access, which also grants exec, so the
// forwarder does not restrict which loopback ports it reaches.
func startPortForwards() {
	go func() {
		l := listenWithRetry(func() (net.Listener, error) { return listenVsock(portForwardVsockPort) }, time.Second, 30*time.Second)
		log.Printf("[guest-agent] port forwarding on vsock port %d", portForwardVsockPort)
		servePortForwards(l, dialLoopback)
	}()
}

// listenWithRetry retries listen until it succeeds. The agent reports ready
// without waiting for the forwarder, so giving up would leave forwarding off for
// the rest of the agent's life.
func listenWithRetry(listen func() (net.Listener, error), backoff, maxBackoff time.Duration) net.Listener {
	for attempt := 1; ; attempt++ {
		l, err := listen()
		if err == nil {
			return l
		}
		log.Printf("[guest-agent] port forward listen attempt %d failed: %v", attempt, err)
		time.Sleep(backoff)
		backoff = min(2*backoff, maxBackoff)
	}
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

// portForwardStepTimeout bounds each handshake step: reading the port, dialing it,
// and writing the status. The host allows two steps for the whole handshake. Tests
// shorten it.
var portForwardStepTimeout = 5 * time.Second

func dialLoopback(ctx context.Context, port uint16) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
}

// forwardGuestPort runs the handshake for one host connection, then copies bytes
// between it and the loopback port until either side closes.
func forwardGuestPort(host net.Conn, dial func(context.Context, uint16) (net.Conn, error)) {
	defer host.Close()
	_ = host.SetDeadline(time.Now().Add(portForwardStepTimeout))
	var port uint16
	if err := binary.Read(host, binary.BigEndian, &port); err != nil {
		return
	}

	if port == 0 {
		_, _ = host.Write([]byte{portForwardStatusDialFailed})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), portForwardStepTimeout)
	target, err := dial(ctx, port)
	cancel()
	// A slow dial must not use up the time for reporting its result, or the host
	// sees a dropped handshake (503) instead of a failed dial (502).
	_ = host.SetWriteDeadline(time.Now().Add(portForwardStepTimeout))
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
