package qemu

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kernel/hypeman/lib/hypervisor"
)

const (
	// vsockDialTimeout is the timeout for connecting via AF_VSOCK
	vsockDialTimeout = 5 * time.Second
)

func init() {
	hypervisor.RegisterVsockDialerFactory(hypervisor.TypeQEMU, NewVsockDialer)
}

// VsockDialer implements hypervisor.VsockDialer for QEMU.
// QEMU with vhost-vsock-pci uses the kernel's native AF_VSOCK socket family.
// Connections are made using the guest's CID (Context ID) and port number.
type VsockDialer struct {
	cid uint32
}

// NewVsockDialer creates a new VsockDialer for QEMU.
// The vsockSocket parameter is unused for QEMU (it uses CID instead).
// The vsockCID is the guest's Context ID assigned via vhost-vsock-pci.
func NewVsockDialer(vsockSocket string, vsockCID int64) hypervisor.VsockDialer {
	return &VsockDialer{
		cid: uint32(vsockCID),
	}
}

// Key returns a unique identifier for this dialer, used for connection pooling.
func (d *VsockDialer) Key() string {
	return fmt.Sprintf("qemu:%d", d.cid)
}

// DialVsock connects to the guest on the specified port using AF_VSOCK.
// This uses the kernel's vsock infrastructure with the guest's CID.
func (d *VsockDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	slog.DebugContext(ctx, "connecting to vsock via AF_VSOCK", "cid", d.cid, "port", port)

	// Create AF_VSOCK socket
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create vsock socket: %w", err)
	}

	// Set up the sockaddr for the guest
	sockaddr := &unix.SockaddrVM{
		CID:  d.cid,
		Port: uint32(port),
	}

	// Use context deadline or default timeout
	dialTimeout := vsockDialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < dialTimeout {
			dialTimeout = remaining
		}
	}

	// Set socket to non-blocking for timeout support
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("set non-blocking: %w", err)
	}

	// Attempt to connect
	err = unix.Connect(fd, sockaddr)
	if err != nil {
		if err != unix.EINPROGRESS {
			unix.Close(fd)
			return nil, fmt.Errorf("connect to vsock cid=%d port=%d: %w", d.cid, port, err)
		}

		// Wait for connection to complete using poll
		deadline := time.Now().Add(dialTimeout)
		for {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				unix.Close(fd)
				return nil, fmt.Errorf("connect to vsock cid=%d port=%d: timeout after %v", d.cid, port, dialTimeout)
			}

			// Poll for write readiness (indicates connection complete)
			pollFds := []unix.PollFd{{
				Fd:     int32(fd),
				Events: unix.POLLOUT,
			}}

			timeoutMs := int(remaining.Milliseconds())
			if timeoutMs < 1 {
				timeoutMs = 1
			}

			n, err := unix.Poll(pollFds, timeoutMs)
			if err != nil {
				if err == unix.EINTR {
					continue // Interrupted, retry
				}
				unix.Close(fd)
				return nil, fmt.Errorf("poll vsock: %w", err)
			}

			if n > 0 {
				// Check for connection errors
				errno, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
				if err != nil {
					unix.Close(fd)
					return nil, fmt.Errorf("getsockopt: %w", err)
				}
				if errno != 0 {
					unix.Close(fd)
					return nil, fmt.Errorf("connect to vsock cid=%d port=%d: %w", d.cid, port, unix.Errno(errno))
				}
				break // Connection successful
			}
		}
	}

	// Set back to blocking mode for normal I/O
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("set blocking: %w", err)
	}

	slog.DebugContext(ctx, "vsock connection established", "cid", d.cid, "port", port)

	// Wrap the file descriptor in a net.Conn
	return newVsockConn(fd, d.cid, uint32(port))
}

// vsockConn wraps a vsock file descriptor as a net.Conn
type vsockConn struct {
	fd         int
	localCID   uint32
	localPort  uint32
	remoteCID  uint32
	remotePort uint32
}

func newVsockConn(fd int, remoteCID, remotePort uint32) (*vsockConn, error) {
	return &vsockConn{
		fd:         fd,
		localCID:   unix.VMADDR_CID_HOST,
		localPort:  0, // ephemeral
		remoteCID:  remoteCID,
		remotePort: remotePort,
	}, nil
}

func (c *vsockConn) Read(b []byte) (int, error) {
	n, err := unix.Read(c.fd, b)
	// Ensure we never return negative n (violates io.Reader contract)
	// This can happen when the vsock fd becomes invalid (VM died)
	if n < 0 {
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	return n, err
}

func (c *vsockConn) Write(b []byte) (int, error) {
	n, err := unix.Write(c.fd, b)
	// Ensure we never return negative n (violates io.Writer contract)
	// This can happen when the vsock fd becomes invalid (VM died)
	if n < 0 {
		if err == nil {
			err = io.ErrClosedPipe
		}
		return 0, err
	}
	return n, err
}

func (c *vsockConn) Close() error {
	return unix.Close(c.fd)
}

func (c *vsockConn) LocalAddr() net.Addr {
	return &vsockAddr{cid: c.localCID, port: c.localPort}
}

func (c *vsockConn) RemoteAddr() net.Addr {
	return &vsockAddr{cid: c.remoteCID, port: c.remotePort}
}

func (c *vsockConn) SetDeadline(t time.Time) error {
	if t.IsZero() {
		// Clear deadlines
		if err := c.SetReadDeadline(t); err != nil {
			return err
		}
		return c.SetWriteDeadline(t)
	}
	timeout := time.Until(t)
	if timeout < 0 {
		timeout = 0
	}
	tv := unix.NsecToTimeval(timeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	return unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
}

func (c *vsockConn) SetReadDeadline(t time.Time) error {
	var tv unix.Timeval
	if !t.IsZero() {
		timeout := time.Until(t)
		if timeout < 0 {
			timeout = 0
		}
		tv = unix.NsecToTimeval(timeout.Nanoseconds())
	}
	return unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

func (c *vsockConn) SetWriteDeadline(t time.Time) error {
	var tv unix.Timeval
	if !t.IsZero() {
		timeout := time.Until(t)
		if timeout < 0 {
			timeout = 0
		}
		tv = unix.NsecToTimeval(timeout.Nanoseconds())
	}
	return unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
}

// vsockAddr implements net.Addr for vsock addresses
type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a *vsockAddr) Network() string {
	return "vsock"
}

func (a *vsockAddr) String() string {
	return fmt.Sprintf("%d:%d", a.cid, a.port)
}
