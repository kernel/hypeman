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

	"github.com/kernel/hypeman/lib/guest/portforward"
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
	require.Equal(t, portforward.StatusOK, status[0])

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
			require.Equal(t, portforward.StatusDialFailed, status[0])
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
		if err != nil || status[0] != portforward.StatusOK {
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

// Handshakes that never send a port hold slots too, until the step timeout frees them.
func TestServePortForwardsCountsStalledHandshakes(t *testing.T) {
	prev := portForwardStepTimeout
	portForwardStepTimeout = 500 * time.Millisecond
	t.Cleanup(func() { portForwardStepTimeout = prev })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { servePortForwards(listener, dialLoopback); close(done) }()
	var stalled []net.Conn
	t.Cleanup(func() {
		for _, conn := range stalled {
			conn.Close()
		}
		listener.Close()
		<-done
	})
	for range maxConcurrentPortForwards {
		conn, err := net.Dial("tcp", listener.Addr().String())
		require.NoError(t, err)
		stalled = append(stalled, conn)
	}
	// Wait until the agent has admitted every stalled handshake.
	time.Sleep(100 * time.Millisecond)

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "a connection beyond the limit is closed while stalled handshakes hold every slot")

	echo := startLoopbackEcho(t)
	require.Eventually(t, func() bool {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			return false
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if portforward.WriteRequest(conn, echo) != nil {
			return false
		}
		status, err := portforward.ReadStatus(conn)
		return err == nil && status == portforward.StatusOK
	}, 5*time.Second, 50*time.Millisecond, "stalled handshakes release their slots after the step timeout")
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

func TestForwardGuestPortReportsSlowDialFailure(t *testing.T) {
	prev := portForwardStepTimeout
	portForwardStepTimeout = 200 * time.Millisecond
	t.Cleanup(func() { portForwardStepTimeout = prev })

	host, agent := net.Pipe()
	defer host.Close()
	// The dial outlasts its timeout, as a slow-to-refuse target can.
	slowDial := func(ctx context.Context, _ uint16) (net.Conn, error) {
		<-ctx.Done()
		time.Sleep(portForwardStepTimeout / 2)
		return nil, ctx.Err()
	}
	go forwardGuestPort(agent, slowDial)
	require.NoError(t, binary.Write(host, binary.BigEndian, uint16(8080)))
	status := make([]byte, 1)
	_, err := io.ReadFull(host, status)
	require.NoError(t, err, "a dial that uses its whole timeout still reports its status")
	require.Equal(t, portforward.StatusDialFailed, status[0])
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
	require.Equal(t, portforward.StatusOK, status[0], "forwarding works after transient accept errors")

	require.NoError(t, inner.Close())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the listener closed")
	}
}

func TestPortForwardListenRetriesThenGivesUp(t *testing.T) {
	var attempts int
	want, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { want.Close() })
	got, err := listenWithRetries(func() (net.Listener, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("vsock not ready")
		}
		return want, nil
	}, 10, time.Microsecond)
	require.NoError(t, err)
	require.Same(t, want, got)
	require.Equal(t, 3, attempts)

	attempts = 0
	_, err = listenWithRetries(func() (net.Listener, error) {
		attempts++
		return nil, errors.New("port in use")
	}, 4, time.Microsecond)
	require.ErrorContains(t, err, "port in use")
	require.Equal(t, 4, attempts, "a port that never binds leaves forwarding off instead of delaying readiness")
}
