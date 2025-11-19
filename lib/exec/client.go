package exec

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

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
}

// ExecIntoInstance executes command in instance via vsock using gRPC
// vsockSocketPath is the Unix socket created by Cloud Hypervisor (e.g., /var/lib/hypeman/guests/{id}/vsock.sock)
func ExecIntoInstance(ctx context.Context, vsockSocketPath string, opts ExecOptions) (*ExitStatus, error) {
	// Connect to Cloud Hypervisor's vsock Unix socket with custom dialer
	grpcConn, err := grpc.NewClient("passthrough:///vsock",
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			// Connect to CH's Unix socket
			conn, err := net.Dial("unix", vsockSocketPath)
			if err != nil {
				return nil, fmt.Errorf("dial unix socket: %w", err)
			}

			// Perform Cloud Hypervisor vsock handshake
			if _, err := fmt.Fprintf(conn, "CONNECT 2222\n"); err != nil {
				conn.Close()
				return nil, fmt.Errorf("send handshake: %w", err)
			}

			// Read handshake response
			reader := bufio.NewReader(conn)
			response, err := reader.ReadString('\n')
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("read handshake response: %w", err)
			}

			if !strings.HasPrefix(response, "OK ") {
				conn.Close()
				return nil, fmt.Errorf("handshake failed: %s", strings.TrimSpace(response))
			}

			// Return the connection for gRPC to use
			// Note: bufio.Reader may have buffered data, but since we only read one line
			// and gRPC will start fresh, this should be safe
			return conn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("create grpc client: %w", err)
	}
	defer grpcConn.Close()

	// Create exec client
	client := NewExecServiceClient(grpcConn)
	stream, err := client.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("start exec stream: %w", err)
	}

	// Send start request
	if err := stream.Send(&ExecRequest{
		Request: &ExecRequest_Start{
			Start: &ExecStart{
				Command: opts.Command,
				Tty:     opts.TTY,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send start request: %w", err)
	}

	// Handle stdin in background
	if opts.Stdin != nil {
		go func() {
			buf := make([]byte, 32 * 1024)
			for {
				n, err := opts.Stdin.Read(buf)
				if n > 0 {
					stream.Send(&ExecRequest{
						Request: &ExecRequest_Stdin{Stdin: buf[:n]},
					})
				}
				if err != nil {
					stream.CloseSend()
					return
				}
			}
		}()
	}

	// Receive responses
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			// Stream closed without exit code
			return nil, fmt.Errorf("stream closed without exit code")
		}
		if err != nil {
			return nil, fmt.Errorf("receive response: %w", err)
		}

		switch r := resp.Response.(type) {
		case *ExecResponse_Stdout:
			if opts.Stdout != nil {
				opts.Stdout.Write(r.Stdout)
			}
		case *ExecResponse_Stderr:
			if opts.Stderr != nil {
				opts.Stderr.Write(r.Stderr)
			}
		case *ExecResponse_ExitCode:
			return &ExitStatus{Code: int(r.ExitCode)}, nil
		}
	}
}

