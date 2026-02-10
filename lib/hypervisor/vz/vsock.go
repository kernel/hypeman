//go:build darwin

package vz

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

const (
	vsockDialTimeout      = 5 * time.Second
	vsockHandshakeTimeout = 5 * time.Second
)

// VsockDialer implements hypervisor.VsockDialer for vz via the shim's Unix socket proxy.
// Uses the same protocol as Cloud Hypervisor: CONNECT {port}\n -> OK {port}\n
type VsockDialer struct {
	socketPath string // path to vz.vsock Unix socket
}

// NewVsockDialer creates a new VsockDialer for vz.
// vsockSocket is the path to the vz.vsock Unix socket proxy.
// vsockCID is unused because the vz proxy is per-VM (unlike QEMU which uses kernel AF_VSOCK with CID routing).
func NewVsockDialer(vsockSocket string, vsockCID int64) hypervisor.VsockDialer {
	return &VsockDialer{
		socketPath: vsockSocket,
	}
}

// Key returns a unique identifier for this dialer, used for connection pooling.
func (d *VsockDialer) Key() string {
	return "vz:" + d.socketPath
}

// DialVsock connects to the guest on the specified port via the shim's vsock proxy.
func (d *VsockDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	slog.DebugContext(ctx, "connecting to vsock via shim proxy", "socket", d.socketPath, "port", port)

	// Use dial timeout, respecting context deadline if shorter
	dialTimeout := vsockDialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < dialTimeout {
			dialTimeout = remaining
		}
	}

	// Connect to the shim's vsock proxy Unix socket
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", d.socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock proxy socket %s: %w", d.socketPath, err)
	}

	slog.DebugContext(ctx, "connected to vsock proxy, performing handshake", "port", port)

	// Set deadline for handshake
	if err := conn.SetDeadline(time.Now().Add(vsockHandshakeTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	// Perform handshake (same protocol as Cloud Hypervisor)
	handshakeCmd := fmt.Sprintf("CONNECT %d\n", port)
	if _, err := conn.Write([]byte(handshakeCmd)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send vsock handshake: %w", err)
	}

	// Read handshake response
	reader := bufio.NewReader(conn)
	response, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read vsock handshake response (is guest-agent running?): %w", err)
	}

	// Clear deadline after successful handshake
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clear deadline: %w", err)
	}

	response = strings.TrimSpace(response)
	if !strings.HasPrefix(response, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("vsock handshake failed: %s", response)
	}

	slog.DebugContext(ctx, "vsock handshake successful", "response", response)

	// Return wrapped connection that uses the bufio.Reader
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// bufferedConn wraps a net.Conn with a bufio.Reader to ensure any buffered
// data from the handshake is properly drained before reading from the connection.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
