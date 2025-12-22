package guest

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	// vsockDialTimeout is the timeout for connecting to the vsock Unix socket
	vsockDialTimeout = 5 * time.Second
	// vsockHandshakeTimeout is the timeout for the Cloud Hypervisor vsock handshake
	vsockHandshakeTimeout = 5 * time.Second
	// vsockGuestPort is the port the guest-agent listens on inside the guest
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

// GetOrCreateConnPublic is a public wrapper for getOrCreateConn for use by the API layer
func GetOrCreateConnPublic(ctx context.Context, vsockSocketPath string) (*grpc.ClientConn, error) {
	return getOrCreateConn(ctx, vsockSocketPath)
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

	// Create guest client
	client := NewGuestServiceClient(grpcConn)
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
			if GuestMetrics != nil {
				bytesReceived := int64(totalStdout + totalStderr)
				GuestMetrics.RecordExecSession(ctx, start, exitCode, atomic.LoadInt64(&bytesSent), bytesReceived)
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
		return nil, fmt.Errorf("read vsock handshake response (is guest-agent running in guest?): %w", err)
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

// CopyToInstanceOptions configures a copy-to-instance operation
type CopyToInstanceOptions struct {
	SrcPath  string      // Local source path
	DstPath  string      // Destination path in guest
	Mode     fs.FileMode // Optional: override file mode (0 = preserve source)
}

// CopyToInstance copies a file or directory to an instance via vsock
func CopyToInstance(ctx context.Context, vsockSocketPath string, opts CopyToInstanceOptions) error {
	grpcConn, err := getOrCreateConn(ctx, vsockSocketPath)
	if err != nil {
		return fmt.Errorf("get grpc connection: %w", err)
	}

	client := NewGuestServiceClient(grpcConn)

	// Stat the source
	srcInfo, err := os.Stat(opts.SrcPath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	if srcInfo.IsDir() {
		return copyDirToInstance(ctx, client, opts.SrcPath, opts.DstPath)
	}
	return copyFileToInstance(ctx, client, opts.SrcPath, opts.DstPath, opts.Mode)
}

// copyFileToInstance copies a single file to the instance
func copyFileToInstance(ctx context.Context, client GuestServiceClient, srcPath, dstPath string, mode fs.FileMode) error {
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	if mode == 0 {
		mode = srcInfo.Mode().Perm()
	}

	stream, err := client.CopyToGuest(ctx)
	if err != nil {
		return fmt.Errorf("start copy stream: %w", err)
	}

	// Send start message
	if err := stream.Send(&CopyToGuestRequest{
		Request: &CopyToGuestRequest_Start{
			Start: &CopyToGuestStart{
				Path:  dstPath,
				Mode:  uint32(mode),
				IsDir: false,
				Size:  srcInfo.Size(),
				Mtime: srcInfo.ModTime().Unix(),
			},
		},
	}); err != nil {
		return fmt.Errorf("send start: %w", err)
	}

	// Open and stream file content
	file, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer file.Close()

	buf := make([]byte, 32*1024)
	for {
		n, err := file.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&CopyToGuestRequest{
				Request: &CopyToGuestRequest_Data{Data: buf[:n]},
			}); sendErr != nil {
				return fmt.Errorf("send data: %w", sendErr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read source: %w", err)
		}
	}

	// Send end message
	if err := stream.Send(&CopyToGuestRequest{
		Request: &CopyToGuestRequest_End{End: &CopyToGuestEnd{}},
	}); err != nil {
		return fmt.Errorf("send end: %w", err)
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("close stream: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("copy failed: %s", resp.Error)
	}

	return nil
}

// copyDirToInstance copies a directory recursively to the instance
func copyDirToInstance(ctx context.Context, client GuestServiceClient, srcPath, dstPath string) error {
	return filepath.WalkDir(srcPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcPath, path)
		if err != nil {
			return fmt.Errorf("relative path: %w", err)
		}

		targetPath := filepath.Join(dstPath, relPath)
		if targetPath == dstPath && relPath == "." {
			targetPath = dstPath
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("info: %w", err)
		}

		if d.IsDir() {
			// Create directory
			stream, err := client.CopyToGuest(ctx)
			if err != nil {
				return fmt.Errorf("start copy stream: %w", err)
			}

			if err := stream.Send(&CopyToGuestRequest{
				Request: &CopyToGuestRequest_Start{
					Start: &CopyToGuestStart{
						Path:  targetPath,
						Mode:  uint32(info.Mode().Perm()),
						IsDir: true,
						Mtime: info.ModTime().Unix(),
					},
				},
			}); err != nil {
				return fmt.Errorf("send start: %w", err)
			}

			if err := stream.Send(&CopyToGuestRequest{
				Request: &CopyToGuestRequest_End{End: &CopyToGuestEnd{}},
			}); err != nil {
				return fmt.Errorf("send end: %w", err)
			}

			resp, err := stream.CloseAndRecv()
			if err != nil {
				return fmt.Errorf("close stream: %w", err)
			}

			if !resp.Success {
				return fmt.Errorf("create directory failed: %s", resp.Error)
			}
			return nil
		}

		// Copy file
		return copyFileToInstance(ctx, client, path, targetPath, 0)
	})
}

// CopyFromInstanceOptions configures a copy-from-instance operation
type CopyFromInstanceOptions struct {
	SrcPath     string // Source path in guest
	DstPath     string // Local destination path
	FollowLinks bool   // Follow symbolic links
}

// FileHandler is called for each file received from the instance
type FileHandler func(header *CopyFromGuestHeader, data io.Reader) error

// CopyFromInstance copies a file or directory from an instance via vsock
func CopyFromInstance(ctx context.Context, vsockSocketPath string, opts CopyFromInstanceOptions) error {
	grpcConn, err := getOrCreateConn(ctx, vsockSocketPath)
	if err != nil {
		return fmt.Errorf("get grpc connection: %w", err)
	}

	client := NewGuestServiceClient(grpcConn)

	stream, err := client.CopyFromGuest(ctx, &CopyFromGuestRequest{
		Path:        opts.SrcPath,
		FollowLinks: opts.FollowLinks,
	})
	if err != nil {
		return fmt.Errorf("start copy stream: %w", err)
	}

	var currentFile *os.File
	var currentHeader *CopyFromGuestHeader
	var receivedFinal bool

	// Ensure file is closed on error paths
	defer func() {
		if currentFile != nil {
			currentFile.Close()
		}
	}()

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("receive: %w", err)
		}

		switch r := resp.Response.(type) {
		case *CopyFromGuestResponse_Header:
			// Close previous file if any
			if currentFile != nil {
				currentFile.Close()
				currentFile = nil
			}

			currentHeader = r.Header
			// Use securejoin to prevent path traversal attacks
			targetPath, err := securejoin.SecureJoin(opts.DstPath, r.Header.Path)
			if err != nil {
				return fmt.Errorf("invalid path %s: %w", r.Header.Path, err)
			}

			if r.Header.IsDir {
				if err := os.MkdirAll(targetPath, fs.FileMode(r.Header.Mode)); err != nil {
					return fmt.Errorf("create directory %s: %w", targetPath, err)
				}
		} else if r.Header.IsSymlink {
			// Validate symlink target to prevent path traversal attacks
			// Reject absolute paths
			if filepath.IsAbs(r.Header.LinkTarget) {
				return fmt.Errorf("invalid symlink target (absolute path not allowed): %s", r.Header.LinkTarget)
			}
			// Reject targets that escape the destination directory
			// Resolve the link target relative to the symlink's parent directory
			linkDir := filepath.Dir(targetPath)
			resolvedTarget := filepath.Clean(filepath.Join(linkDir, r.Header.LinkTarget))
			cleanDst := filepath.Clean(opts.DstPath)
			// Check path containment - handle root destination specially
			var contained bool
			if cleanDst == "/" {
				// For root destination, any absolute path that doesn't contain ".." after cleaning is valid
				contained = !strings.Contains(resolvedTarget, "..")
			} else {
				contained = strings.HasPrefix(resolvedTarget, cleanDst+string(filepath.Separator)) || resolvedTarget == cleanDst
			}
			if !contained {
				return fmt.Errorf("invalid symlink target (escapes destination): %s", r.Header.LinkTarget)
			}

			// Create parent directory if needed
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return fmt.Errorf("create parent dir for symlink: %w", err)
			}
			// Create symlink
			os.Remove(targetPath) // Remove existing if any
			if err := os.Symlink(r.Header.LinkTarget, targetPath); err != nil {
				return fmt.Errorf("create symlink %s: %w", targetPath, err)
			}
			} else {
				// Create parent directory
				if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
					return fmt.Errorf("create parent dir: %w", err)
				}
				// Create file
				f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(r.Header.Mode))
				if err != nil {
					return fmt.Errorf("create file %s: %w", targetPath, err)
				}
				currentFile = f
			}

		case *CopyFromGuestResponse_Data:
			if currentFile != nil {
				if _, err := currentFile.Write(r.Data); err != nil {
					return fmt.Errorf("write: %w", err)
				}
			}

		case *CopyFromGuestResponse_End:
			if currentFile != nil {
				currentFile.Close()
				// Set modification time
				if currentHeader != nil && currentHeader.Mtime > 0 {
					targetPath, err := securejoin.SecureJoin(opts.DstPath, currentHeader.Path)
					if err == nil {
						mtime := time.Unix(currentHeader.Mtime, 0)
						os.Chtimes(targetPath, mtime, mtime)
					}
				}
				currentFile = nil
				currentHeader = nil
			}
			if r.End.Final {
				receivedFinal = true
				return nil
			}

		case *CopyFromGuestResponse_Error:
			return fmt.Errorf("copy error at %s: %s", r.Error.Path, r.Error.Message)
		}
	}

	if !receivedFinal {
		return fmt.Errorf("copy stream ended without completion marker")
	}
	return nil
}

