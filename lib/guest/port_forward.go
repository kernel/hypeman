package guest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/kernel/hypeman/lib/guest/portforward"
	"github.com/kernel/hypeman/lib/hypervisor"
)

// ErrGuestPortUnavailable means nothing accepted the connection on the guest port.
var ErrGuestPortUnavailable = errors.New("guest port unavailable")

// ErrPortForwarderUnavailable means the guest agent's port forwarder did not answer.
var ErrPortForwarderUnavailable = errors.New("guest port forwarder unavailable")

// portForwardHandshakeTimeout bounds the handshake, including the agent's dial.
const portForwardHandshakeTimeout = 10 * time.Second

// DialPort opens a connection to TCP port on the guest's loopback interface,
// through the guest agent's port forwarder. ctx bounds only the handshake.
func DialPort(ctx context.Context, dialer hypervisor.VsockDialer, port uint16) (net.Conn, error) {
	if port == 0 {
		return nil, fmt.Errorf("invalid guest port 0")
	}
	conn, err := dialer.DialVsock(ctx, portforward.VsockPort)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPortForwarderUnavailable, err)
	}
	// Cancellation unblocks the handshake by closing the connection.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	err = portForwardHandshake(conn, port)
	if !stop() {
		conn.Close()
		return nil, ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func portForwardHandshake(conn net.Conn, port uint16) error {
	if err := conn.SetDeadline(time.Now().Add(portForwardHandshakeTimeout)); err != nil {
		return fmt.Errorf("%w: set handshake deadline: %v", ErrPortForwarderUnavailable, err)
	}
	if err := portforward.WriteRequest(conn, port); err != nil {
		return fmt.Errorf("%w: handshake: %v", ErrPortForwarderUnavailable, err)
	}
	status, err := portforward.ReadStatus(conn)
	if err != nil {
		return fmt.Errorf("%w: handshake: %w", ErrPortForwarderUnavailable, err)
	}
	if status != portforward.StatusOK {
		return fmt.Errorf("%w: %d", ErrGuestPortUnavailable, port)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("%w: clear handshake deadline: %v", ErrPortForwarderUnavailable, err)
	}
	return nil
}
