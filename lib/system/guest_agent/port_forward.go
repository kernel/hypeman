package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/kernel/hypeman/lib/guest/portforward"
)

const maxConcurrentPortForwards = 64

// portForwardSupported is true where the agent forwards guest ports. Linux guests
// reach their ports through ingress instead.
const portForwardSupported = runtime.GOOS == "darwin"

// portForwarding is set once the forwarder is listening. Only then does the agent
// advertise port forwarding.
var portForwarding atomic.Bool

// startPortForwards binds the port forward vsock port before the agent reports
// ready, so an agent that advertises forwarding is already accepting, then connects
// each host connection to the TCP port it names on the guest's loopback interface.
// If the port cannot be bound, the agent still serves its other operations but does
// not advertise forwarding. The host API requires instance-write access, which also
// grants exec, so the forwarder does not restrict which loopback ports it reaches.
func startPortForwards() {
	l, err := listenWithRetries(func() (net.Listener, error) { return listenVsock(portforward.VsockPort) }, 10, time.Second)
	if err != nil {
		log.Printf("[guest-agent] port forwarding disabled: %v", err)
		return
	}
	portForwarding.Store(true)
	log.Printf("[guest-agent] port forwarding on vsock port %d", portforward.VsockPort)
	go servePortForwards(l, dialLoopback)
}

// listenWithRetries retries listen with a fixed backoff, as the agent does for its
// main vsock port.
func listenWithRetries(listen func() (net.Listener, error), attempts int, backoff time.Duration) (net.Listener, error) {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		var l net.Listener
		if l, err = listen(); err == nil {
			return l, nil
		}
		log.Printf("[guest-agent] port forward listen attempt %d/%d failed: %v", attempt, attempts, err)
		if attempt < attempts {
			time.Sleep(backoff)
		}
	}
	return nil, err
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
	port, err := portforward.ReadRequest(host)
	if err != nil {
		return
	}

	if port == 0 {
		_ = portforward.WriteStatus(host, portforward.StatusDialFailed)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), portForwardStepTimeout)
	target, err := dial(ctx, port)
	cancel()
	// A slow dial must not use up the time for reporting its result, or the host
	// sees a dropped handshake (503) instead of a failed dial (502).
	_ = host.SetWriteDeadline(time.Now().Add(portForwardStepTimeout))
	if err != nil {
		_ = portforward.WriteStatus(host, portforward.StatusDialFailed)
		return
	}
	defer target.Close()
	if err := portforward.WriteStatus(host, portforward.StatusOK); err != nil {
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
