package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/kernel/hypeman/lib/guest"
	"github.com/stretchr/testify/require"
)

// startLoopbackEcho serves one echo connection on loopback and returns its port.
func startLoopbackEcho(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

func TestForwardGuestPortCopiesBothWays(t *testing.T) {
	port := startLoopbackEcho(t)

	host, agent := net.Pipe()
	done := make(chan struct{})
	go func() {
		forwardGuestPort(agent, dialLoopback)
		close(done)
	}()
	require.NoError(t, binary.Write(host, binary.BigEndian, port))
	status := make([]byte, 1)
	_, err := io.ReadFull(host, status)
	require.NoError(t, err)
	require.Equal(t, portForwardStatusOK, status[0])

	_, err = host.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(host, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))

	require.NoError(t, host.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not end after the host closed")
	}
}

func TestForwardGuestPortReportsDialFailure(t *testing.T) {
	for _, port := range []uint16{0, 9222} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			host, agent := net.Pipe()
			defer host.Close()
			refuse := func(context.Context, uint16) (net.Conn, error) { return nil, fmt.Errorf("connection refused") }
			go forwardGuestPort(agent, refuse)
			require.NoError(t, binary.Write(host, binary.BigEndian, port))
			status := make([]byte, 1)
			_, err := io.ReadFull(host, status)
			require.NoError(t, err)
			require.Equal(t, portForwardStatusDialFailed, status[0])
		})
	}
}

func TestServePortForwardsBoundsConcurrencyAndReusesSlots(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	peers := make(chan net.Conn, maxConcurrentPortForwards+1)
	var dialed atomic.Int32
	dial := func(context.Context, uint16) (net.Conn, error) {
		a, b := net.Pipe()
		peers <- b
		dialed.Add(1)
		return a, nil
	}
	done := make(chan struct{})
	go func() { servePortForwards(listener, dial); close(done) }()
	openForward := func() (net.Conn, error) {
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		var status [1]byte
		err = binary.Write(conn, binary.BigEndian, uint16(9222))
		if err == nil {
			_, err = io.ReadFull(conn, status[:])
		}
		if err != nil || status[0] != portForwardStatusOK {
			conn.Close()
			return nil, fmt.Errorf("handshake failed: %v", err)
		}
		return conn, nil
	}
	var hosts []net.Conn
	t.Cleanup(func() {
		for _, conn := range hosts {
			conn.Close()
		}
		listener.Close()
		<-done
		for len(peers) > 0 {
			(<-peers).Close()
		}
	})
	for range maxConcurrentPortForwards {
		conn, err := openForward()
		require.NoError(t, err)
		hosts = append(hosts, conn)
	}
	_, err = openForward()
	require.Error(t, err, "excess forwards must be rejected, not queued")
	require.Equal(t, int32(maxConcurrentPortForwards), dialed.Load(), "overload must not dial the target")

	require.NoError(t, hosts[0].Close())
	require.Eventually(t, func() bool {
		conn, err := openForward()
		if err != nil {
			return false
		}
		hosts = append(hosts, conn)
		return true
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int32(maxConcurrentPortForwards+1), dialed.Load())
}

func TestForwardGuestPortBoundsStatusWrite(t *testing.T) {
	host, agent := net.Pipe()
	defer host.Close()
	done := make(chan struct{})
	go func() { forwardGuestPort(agent, dialLoopback); close(done) }()
	require.NoError(t, binary.Write(host, binary.BigEndian, uint16(0)))
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("an unread handshake status must not hold a forwarding slot indefinitely")
	}
}

func TestPortForwardProtocolMatchesHost(t *testing.T) {
	require.Equal(t, pb.PortForwardVsockPort, portForwardVsockPort)
	require.Equal(t, pb.PortForwardStatusOK, portForwardStatusOK)
	require.Equal(t, pb.PortForwardStatusDialFailed, portForwardStatusDialFailed)
}

type flakyListener struct {
	net.Listener
	failures int
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures > 0 {
		l.failures--
		return nil, errors.New("too many open files")
	}
	return l.Listener.Accept()
}

func TestServePortForwardsSurvivesTransientAcceptErrors(t *testing.T) {
	echoPort := startLoopbackEcho(t)
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		servePortForwards(&flakyListener{Listener: inner, failures: 3}, dialLoopback)
		close(done)
	}()

	host, err := net.Dial("tcp", inner.Addr().String())
	require.NoError(t, err)
	defer host.Close()
	require.NoError(t, binary.Write(host, binary.BigEndian, echoPort))
	status := make([]byte, 1)
	_, err = io.ReadFull(host, status)
	require.NoError(t, err)
	require.Equal(t, portForwardStatusOK, status[0], "forwarding works after transient accept errors")

	require.NoError(t, inner.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the listener closed")
	}
}

func TestPortForwardListenRetriesUntilBound(t *testing.T) {
	var attempts int
	want, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { want.Close() })
	got := listenWithRetry(func() (net.Listener, error) {
		attempts++
		if attempts <= 12 {
			return nil, errors.New("vsock not ready")
		}
		return want, nil
	}, time.Microsecond, time.Millisecond)
	require.Same(t, want, got)
	require.Equal(t, 13, attempts, "listening continues past the old ten-attempt limit")
}
