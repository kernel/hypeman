package vmm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
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
// It extracts the embedded binary if needed and starts the VMM
func StartProcess(ctx context.Context, dataDir string, version CHVersion, socketPath string) error {
	// Get binary path (extracts if needed)
	binaryPath, err := GetBinaryPath(dataDir, version)
	if err != nil {
		return fmt.Errorf("get binary: %w", err)
	}

	// Check if socket is already in use
	if isSocketInUse(socketPath) {
		return fmt.Errorf("socket already in use, VMM may be running at %s", socketPath)
	}

	// Remove stale socket if exists
	// Ignore error - if we can't remove it, CH will fail with clearer error
	os.Remove(socketPath)

	cmd := exec.CommandContext(ctx, binaryPath, "--api-socket", socketPath)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloud-hypervisor: %w", err)
	}

	// Wait for socket to be ready
	return waitForSocket(ctx, socketPath, 5*time.Second)
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
