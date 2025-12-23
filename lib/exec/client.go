package exec

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/onkernel/hypeman/lib/hypervisor"
)

const (
	// vsockGuestPort is the port the exec-agent listens on inside the guest
	vsockGuestPort = 2222
)

// connPool manages reusable gRPC connections per vsock dialer key
// This avoids the overhead and potential issues of rapidly creating/closing connections
var connPool = struct {
	sync.RWMutex
	conns map[string]*grpc.ClientConn
}{
	conns: make(map[string]*grpc.ClientConn),
}

// getOrCreateConn returns an existing connection or creates a new one
func getOrCreateConn(ctx context.Context, dialer hypervisor.VsockDialer) (*grpc.ClientConn, error) {
	key := dialer.Key()

	// Try read lock first for existing connection
	connPool.RLock()
	if conn, ok := connPool.conns[key]; ok {
		connPool.RUnlock()
		return conn, nil
	}
	connPool.RUnlock()

	// Need to create new connection - acquire write lock
	connPool.Lock()
	defer connPool.Unlock()

	// Double-check after acquiring write lock
	if conn, ok := connPool.conns[key]; ok {
		return conn, nil
	}

	// Create new connection
	conn, err := grpc.Dial("passthrough:///vsock",
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dialer.DialVsock(ctx, vsockGuestPort)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("create grpc connection: %w", err)
	}

	connPool.conns[key] = conn
	slog.Debug("created new gRPC connection", "key", key)
	return conn, nil
}

// CloseConn closes and removes a connection from the pool (call when VM is deleted)
func CloseConn(dialerKey string) {
	connPool.Lock()
	defer connPool.Unlock()

	if conn, ok := connPool.conns[dialerKey]; ok {
		conn.Close()
		delete(connPool.conns, dialerKey)
		slog.Debug("closed gRPC connection", "key", dialerKey)
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

// ExecIntoInstance executes command in instance via vsock using gRPC.
// The dialer is a hypervisor-specific VsockDialer that knows how to connect to the guest.
func ExecIntoInstance(ctx context.Context, dialer hypervisor.VsockDialer, opts ExecOptions) (*ExitStatus, error) {
	start := time.Now()
	var bytesSent int64

	// Get or create a reusable gRPC connection for this vsock dialer
	// Connection pooling avoids issues with rapid connect/disconnect cycles
	grpcConn, err := getOrCreateConn(ctx, dialer)
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
