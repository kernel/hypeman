package vmm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/onkernel/hypeman/lib/paths"
)

// VMM wraps the generated Cloud Hypervisor client (API v0.3.0)
type VMM struct {
	*ClientWithResponses
	socketPath string
}

// NewVMM creates a Cloud Hypervisor client for an existing VMM socket
func NewVMM(socketPath string) (*VMM, error) {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
		Timeout: 30 * time.Second,
	}

	client, err := NewClientWithResponses("http://localhost/api/v1",
		WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create client: %w", err)
	}

	return &VMM{
		ClientWithResponses: client,
		socketPath:          socketPath,
	}, nil
}

// StartProcess starts a Cloud Hypervisor VMM process with the given version
// It extracts the embedded binary if needed and starts the VMM as a daemon.
// Returns the process ID of the started Cloud Hypervisor process.
func StartProcess(ctx context.Context, p *paths.Paths, version CHVersion, socketPath string) (int, error) {
	return StartProcessWithArgs(ctx, p, version, socketPath, nil)
}

// StartProcessWithArgs starts a Cloud Hypervisor VMM process with additional command-line arguments.
// This is useful for testing or when you need to pass specific flags like verbosity.
func StartProcessWithArgs(ctx context.Context, p *paths.Paths, version CHVersion, socketPath string, extraArgs []string) (int, error) {
	// Get binary path (extracts if needed)
	binaryPath, err := GetBinaryPath(p, version)
	if err != nil {
		return 0, fmt.Errorf("get binary: %w", err)
	}

	// Check if socket is already in use
	if isSocketInUse(socketPath) {
		return 0, fmt.Errorf("socket already in use, VMM may be running at %s", socketPath)
	}

	// Remove stale socket if exists
	// Ignore error - if we can't remove it, CH will fail with clearer error
	os.Remove(socketPath)

	// Build command arguments
	args := []string{"--api-socket", socketPath}
	args = append(args, extraArgs...)
	
	// Use Command (not CommandContext) so process survives parent context cancellation
	cmd := exec.Command(binaryPath, args...)
	
	// Daemonize: detach from parent process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // Create new process group
	}
	
	// Redirect stdout/stderr to log files (process won't block on I/O)
	instanceDir := filepath.Dir(socketPath)
	stdoutFile, err := os.OpenFile(
		filepath.Join(instanceDir, "ch-stdout.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return 0, fmt.Errorf("create stdout log: %w", err)
	}
	// Note: These defers close the parent's file descriptors after cmd.Start().
	// The child process receives duplicated file descriptors during fork/exec,
	// so it can continue writing to the log files even after we close them here.
	defer stdoutFile.Close()
	
	stderrFile, err := os.OpenFile(
		filepath.Join(instanceDir, "ch-stderr.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return 0, fmt.Errorf("create stderr log: %w", err)
	}
	defer stderrFile.Close()
	
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start cloud-hypervisor: %w", err)
	}
	
	pid := cmd.Process.Pid

	// Wait for socket to be ready (use fresh context with timeout, not parent context)
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := waitForSocket(waitCtx, socketPath, 5*time.Second); err != nil {
		return 0, err
	}
	
	return pid, nil
}

// isSocketInUse checks if a Unix socket is actively being used
func isSocketInUse(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false // Socket doesn't exist or not listening
	}
	conn.Close()
	return true
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for socket")
		case <-ticker.C:
			if conn, err := net.Dial("unix", path); err == nil {
				conn.Close()
				return nil
			}
		}
	}
}
