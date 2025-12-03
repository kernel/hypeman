package exec

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// vsockDialTimeout is the timeout for connecting to the vsock Unix socket
	vsockDialTimeout = 5 * time.Second
	// vsockHandshakeTimeout is the timeout for the Cloud Hypervisor vsock handshake
	vsockHandshakeTimeout = 5 * time.Second
	// vsockGuestPort is the port the exec-agent listens on inside the guest
	vsockGuestPort = 2222
)

// connPool manages reusable gRPC connections per vsock socket path
// This avoids the overhead and potential issues of rapidly creating/closing connections
var connPool = struct {
	sync.RWMutex
	conns map[string]*grpc.ClientConn
}{
	conns: make(map[string]*grpc.ClientConn),
}

// getOrCreateConn returns an existing connection or creates a new one
func getOrCreateConn(ctx context.Context, vsockSocketPath string) (*grpc.ClientConn, error) {
	// Try read lock first for existing connection
	connPool.RLock()
	if conn, ok := connPool.conns[vsockSocketPath]; ok {
		connPool.RUnlock()
		return conn, nil
	}
	connPool.RUnlock()

	// Need to create new connection - acquire write lock
	connPool.Lock()
	defer connPool.Unlock()

	// Double-check after acquiring write lock
	if conn, ok := connPool.conns[vsockSocketPath]; ok {
		return conn, nil
	}

	// Create new connection
	conn, err := grpc.Dial("passthrough:///vsock",
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dialVsock(ctx, vsockSocketPath)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("create grpc connection: %w", err)
	}

	connPool.conns[vsockSocketPath] = conn
	slog.Debug("created new gRPC connection", "socket", vsockSocketPath)
	return conn, nil
}

// CloseConn closes and removes a connection from the pool (call when VM is deleted)
func CloseConn(vsockSocketPath string) {
	connPool.Lock()
	defer connPool.Unlock()

	if conn, ok := connPool.conns[vsockSocketPath]; ok {
		conn.Close()
		delete(connPool.conns, vsockSocketPath)
		slog.Debug("closed gRPC connection", "socket", vsockSocketPath)
	}
}

// ExitStatus represents command exit information
type ExitStatus struct {
	Code int
}

// ExecOptions configures command execution
type ExecOptions struct {
	Command []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	TTY     bool
	Env     map[string]string // Environment variables
	Cwd     string            // Working directory (optional)
	Timeout int32             // Execution timeout in seconds (0 = no timeout)
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

// ExecIntoInstance executes command in instance via vsock using gRPC
// vsockSocketPath is the Unix socket created by Cloud Hypervisor (e.g., /var/lib/hypeman/guests/{id}/vsock.sock)
func ExecIntoInstance(ctx context.Context, vsockSocketPath string, opts ExecOptions) (*ExitStatus, error) {
	start := time.Now()
	var bytesSent int64

	// Get or create a reusable gRPC connection for this vsock socket
	// Connection pooling avoids issues with rapid connect/disconnect cycles
	grpcConn, err := getOrCreateConn(ctx, vsockSocketPath)
	if err != nil {
		return nil, fmt.Errorf("get grpc connection: %w", err)
	}
	// Note: Don't close the connection - it's pooled and reused

	// Create exec client
	client := NewExecServiceClient(grpcConn)
	stream, err := client.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("start exec stream: %w", err)
	}
	// Ensure stream is properly closed when we're done
	defer stream.CloseSend()

	// Send start request
	if err := stream.Send(&ExecRequest{
		Request: &ExecRequest_Start{
			Start: &ExecStart{
				Command:        opts.Command,
				Tty:            opts.TTY,
				Env:            opts.Env,
				Cwd:            opts.Cwd,
				TimeoutSeconds: opts.Timeout,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send start request: %w", err)
	}

	// Handle stdin in background
	if opts.Stdin != nil {
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := opts.Stdin.Read(buf)
				if n > 0 {
					stream.Send(&ExecRequest{
						Request: &ExecRequest_Stdin{Stdin: buf[:n]},
					})
					atomic.AddInt64(&bytesSent, int64(n))
				}
				if err != nil {
					stream.CloseSend()
					return
				}
			}
		}()
	}

	// Receive responses
	var totalStdout, totalStderr int
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil, fmt.Errorf("stream closed without exit code (stdout=%d, stderr=%d)", totalStdout, totalStderr)
		}
		if err != nil {
			return nil, fmt.Errorf("receive response (stdout=%d, stderr=%d): %w", totalStdout, totalStderr, err)
		}

		switch r := resp.Response.(type) {
		case *ExecResponse_Stdout:
			totalStdout += len(r.Stdout)
			if opts.Stdout != nil {
				opts.Stdout.Write(r.Stdout)
			}
		case *ExecResponse_Stderr:
			totalStderr += len(r.Stderr)
			if opts.Stderr != nil {
				opts.Stderr.Write(r.Stderr)
			}
		case *ExecResponse_ExitCode:
			exitCode := int(r.ExitCode)
			// Record metrics
			if ExecMetrics != nil {
				bytesReceived := int64(totalStdout + totalStderr)
				ExecMetrics.RecordSession(ctx, start, exitCode, atomic.LoadInt64(&bytesSent), bytesReceived)
			}
			return &ExitStatus{Code: exitCode}, nil
		}
	}
}

// dialVsock connects to Cloud Hypervisor's vsock Unix socket and performs the handshake
func dialVsock(ctx context.Context, vsockSocketPath string) (net.Conn, error) {
	slog.DebugContext(ctx, "connecting to vsock", "socket", vsockSocketPath)

	// Use dial timeout, respecting context deadline if shorter
	dialTimeout := vsockDialTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < dialTimeout {
			dialTimeout = remaining
		}
	}

	// Connect to CH's Unix socket with timeout
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", vsockSocketPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock socket %s: %w", vsockSocketPath, err)
	}

	slog.DebugContext(ctx, "connected to vsock socket, performing handshake", "port", vsockGuestPort)

	// Set deadline for handshake
	if err := conn.SetDeadline(time.Now().Add(vsockHandshakeTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	// Perform Cloud Hypervisor vsock handshake
	handshakeCmd := fmt.Sprintf("CONNECT %d\n", vsockGuestPort)
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
