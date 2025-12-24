package guest

import (
	"context"
	"errors"
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
	"github.com/onkernel/hypeman/lib/hypervisor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const (
	// vsockGuestPort is the port the guest-agent listens on inside the guest
	vsockGuestPort = 2222
)

// AgentConnectionError indicates the guest agent is not responding.
// This can happen if:
// - The VM is still booting
// - The guest agent was stopped or deleted
type AgentConnectionError struct {
	Err error
}

func (e *AgentConnectionError) Error() string {
	return fmt.Sprintf("guest agent not responding (it may have been stopped, deleted, or the VM is still booting): %v", e.Err)
}

func (e *AgentConnectionError) Unwrap() error {
	return e.Err
}

// connPool manages reusable gRPC connections per vsock dialer key
// This avoids the overhead and potential issues of rapidly creating/closing connections
var connPool = struct {
	sync.RWMutex
	conns map[string]*grpc.ClientConn
}{
	conns: make(map[string]*grpc.ClientConn),
}

// GetOrCreateConn returns an existing connection or creates a new one using a VsockDialer.
// This supports multiple hypervisor types (Cloud Hypervisor, QEMU, etc.).
func GetOrCreateConn(ctx context.Context, dialer hypervisor.VsockDialer) (*grpc.ClientConn, error) {
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

	// Create new connection using the VsockDialer
	conn, err := grpc.Dial("passthrough:///vsock",
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			netConn, err := dialer.DialVsock(ctx, vsockGuestPort)
			if err != nil {
				return nil, &AgentConnectionError{Err: err}
			}
			return netConn, nil
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

// CloseConn removes a connection from the pool by key (call when VM is deleted).
// We only remove from pool, not explicitly close - the connection will fail
// naturally when the VM dies, and grpc will clean up.
func CloseConn(dialerKey string) {
	connPool.Lock()
	defer connPool.Unlock()

	if _, ok := connPool.conns[dialerKey]; ok {
		delete(connPool.conns, dialerKey)
		slog.Debug("removed gRPC connection from pool", "key", dialerKey)
	}
}

// ExitStatus represents command exit information
type ExitStatus struct {
	Code int
}

// ExecOptions configures command execution
type ExecOptions struct {
	Command      []string
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
	TTY          bool
	Env          map[string]string // Environment variables
	Cwd          string            // Working directory (optional)
	Timeout      int32             // Execution timeout in seconds (0 = no timeout)
	WaitForAgent time.Duration     // Max time to wait for agent to be ready (0 = no wait, fail immediately)
}

// ExecIntoInstance executes command in instance via vsock using gRPC.
// The dialer is a hypervisor-specific VsockDialer that knows how to connect to the guest.
// If WaitForAgent is set, it will retry on connection errors until the timeout.
func ExecIntoInstance(ctx context.Context, dialer hypervisor.VsockDialer, opts ExecOptions) (*ExitStatus, error) {
	// If no wait requested, execute immediately
	if opts.WaitForAgent == 0 {
		return execIntoInstanceOnce(ctx, dialer, opts)
	}

	deadline := time.Now().Add(opts.WaitForAgent)

	for {
		exit, err := execIntoInstanceOnce(ctx, dialer, opts)

		// Success - return immediately
		if err == nil {
			return exit, err
		}

		// Check if this is a retryable connection error
		if !isRetryableConnectionError(err) {
			return exit, err
		}

		// Connection error - check if we should retry
		if time.Now().After(deadline) {
			return nil, err
		}

		// Wait before retrying, but respect context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			// Continue to retry
		}
	}
}

// isRetryableConnectionError returns true if the error indicates the guest agent
// is not yet ready and we should retry connecting.
func isRetryableConnectionError(err error) bool {
	// Check for our custom AgentConnectionError
	var connErr *AgentConnectionError
	if errors.As(err, &connErr) {
		return true
	}

	// Check for gRPC Unavailable errors (agent not yet listening)
	if s, ok := status.FromError(err); ok {
		if s.Code() == codes.Unavailable {
			return true
		}
	}

	return false
}

// execIntoInstanceOnce executes command in instance via vsock using gRPC (single attempt).
func execIntoInstanceOnce(ctx context.Context, dialer hypervisor.VsockDialer, opts ExecOptions) (*ExitStatus, error) {
	start := time.Now()
	var bytesSent int64

	// Get or create a reusable gRPC connection for this vsock dialer
	grpcConn, err := GetOrCreateConn(ctx, dialer)
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

// CopyToInstanceOptions configures a copy-to-instance operation
type CopyToInstanceOptions struct {
	SrcPath string      // Local source path
	DstPath string      // Destination path in guest
	Mode    fs.FileMode // Optional: override file mode (0 = preserve source)
}

// CopyToInstance copies a file or directory to an instance via vsock.
// The dialer is a hypervisor-specific VsockDialer that knows how to connect to the guest.
func CopyToInstance(ctx context.Context, dialer hypervisor.VsockDialer, opts CopyToInstanceOptions) error {
	grpcConn, err := GetOrCreateConn(ctx, dialer)
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

	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer f.Close()

	stream, err := client.CopyToGuest(ctx)
	if err != nil {
		return fmt.Errorf("start copy stream: %w", err)
	}

	// Send start request
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

	// Stream file content
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
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

	// Send end marker
	if err := stream.Send(&CopyToGuestRequest{
		Request: &CopyToGuestRequest_End{End: &CopyToGuestEnd{}},
	}); err != nil {
		return fmt.Errorf("send end: %w", err)
	}

	// Receive response
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("receive response: %w", err)
	}

	if !resp.Success {
		return fmt.Errorf("copy failed: %s", resp.Error)
	}

	return nil
}

// copyDirToInstance copies a directory recursively to the instance
func copyDirToInstance(ctx context.Context, client GuestServiceClient, srcPath, dstPath string) error {
	srcPath = filepath.Clean(srcPath)

	// First create the destination directory
	stream, err := client.CopyToGuest(ctx)
	if err != nil {
		return fmt.Errorf("start copy stream for dir: %w", err)
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("stat source dir: %w", err)
	}

	if err := stream.Send(&CopyToGuestRequest{
		Request: &CopyToGuestRequest_Start{
			Start: &CopyToGuestStart{
				Path:  dstPath,
				Mode:  uint32(srcInfo.Mode().Perm()),
				IsDir: true,
				Mtime: srcInfo.ModTime().Unix(),
			},
		},
	}); err != nil {
		return fmt.Errorf("send dir start: %w", err)
	}

	if err := stream.Send(&CopyToGuestRequest{
		Request: &CopyToGuestRequest_End{End: &CopyToGuestEnd{}},
	}); err != nil {
		return fmt.Errorf("send dir end: %w", err)
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("receive dir response: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("create dir failed: %s", resp.Error)
	}

	// Walk and copy contents
	return filepath.WalkDir(srcPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == srcPath {
			return nil // Skip root, already created
		}

		relPath, err := filepath.Rel(srcPath, path)
		if err != nil {
			return fmt.Errorf("get relative path: %w", err)
		}
		targetPath := filepath.Join(dstPath, relPath)

		if d.IsDir() {
			// Create subdirectory
			stream, err := client.CopyToGuest(ctx)
			if err != nil {
				return fmt.Errorf("start copy stream for subdir: %w", err)
			}

			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("get dir info: %w", err)
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
				return fmt.Errorf("send subdir start: %w", err)
			}

			if err := stream.Send(&CopyToGuestRequest{
				Request: &CopyToGuestRequest_End{End: &CopyToGuestEnd{}},
			}); err != nil {
				return fmt.Errorf("send subdir end: %w", err)
			}

			resp, err := stream.CloseAndRecv()
			if err != nil {
				return fmt.Errorf("receive subdir response: %w", err)
			}
			if !resp.Success {
				return fmt.Errorf("create subdir failed: %s", resp.Error)
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

// CopyFromInstance copies a file or directory from an instance via vsock.
// The dialer is a hypervisor-specific VsockDialer that knows how to connect to the guest.
func CopyFromInstance(ctx context.Context, dialer hypervisor.VsockDialer, opts CopyFromInstanceOptions) error {
	grpcConn, err := GetOrCreateConn(ctx, dialer)
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
				currentFile = nil
			}
			// Set modification time for files and directories
			if currentHeader != nil && currentHeader.Mtime > 0 {
				targetPath, err := securejoin.SecureJoin(opts.DstPath, currentHeader.Path)
				if err == nil {
					mtime := time.Unix(currentHeader.Mtime, 0)
					os.Chtimes(targetPath, mtime, mtime)
				}
			}
			currentHeader = nil
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
