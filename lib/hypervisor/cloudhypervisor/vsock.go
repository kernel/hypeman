package cloudhypervisor

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/onkernel/hypeman/lib/hypervisor"
)

const (
	// vsockDialTimeout is the timeout for connecting to the vsock Unix socket
	vsockDialTimeout = 5 * time.Second
	// vsockHandshakeTimeout is the timeout for the Cloud Hypervisor vsock handshake
	vsockHandshakeTimeout = 5 * time.Second
)

func init() {
	hypervisor.RegisterVsockDialerFactory(hypervisor.TypeCloudHypervisor, NewVsockDialer)
}

// VsockDialer implements hypervisor.VsockDialer for Cloud Hypervisor.
// Cloud Hypervisor exposes vsock through a Unix socket file with a text-based
// handshake protocol (CONNECT {port}\n / OK ...).
type VsockDialer struct {
	socketPath string
}

// NewVsockDialer creates a new VsockDialer for Cloud Hypervisor.
// The vsockSocket parameter is the path to the Unix socket file.
// The vsockCID parameter is unused for Cloud Hypervisor (it uses socket path instead).
func NewVsockDialer(vsockSocket string, vsockCID int64) hypervisor.VsockDialer {
	return &VsockDialer{
		socketPath: vsockSocket,
	}
}

// Key returns a unique identifier for this dialer, used for connection pooling.
func (d *VsockDialer) Key() string {
	return "ch:" + d.socketPath
}

// DialVsock connects to the guest on the specified port.
// It connects to the Cloud Hypervisor Unix socket and performs the handshake protocol.
func (d *VsockDialer) DialVsock(ctx context.Context, port int) (net.Conn, error) {
	slog.DebugContext(ctx, "connecting to vsock", "socket", d.socketPath, "port", port)

	// Use dial timeout, respecting context deadline if shorter
	dialTimeout := vsockDialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < dialTimeout {
			dialTimeout = remaining
		}
	}

	// Connect to CH's Unix socket with timeout
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", d.socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock socket %s: %w", d.socketPath, err)
	}

	slog.DebugContext(ctx, "connected to vsock socket, performing handshake", "port", port)

	// Set deadline for handshake
	if err := conn.SetDeadline(time.Now().Add(vsockHandshakeTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	// Perform Cloud Hypervisor vsock handshake
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
		return nil, fmt.Errorf("read vsock handshake response (is exec-agent running in guest?): %w", err)
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
	// This ensures any bytes buffered during handshake are not lost
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

// bufferedConn wraps a net.Conn with a bufio.Reader to ensure any buffered
// data from the handshake is properly drained before reading from the connection
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
