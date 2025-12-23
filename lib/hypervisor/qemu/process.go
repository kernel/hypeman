// Package qemu implements the hypervisor.Hypervisor interface for QEMU.
package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/paths"
)

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeQEMU, "qemu.sock")
}

// Starter implements hypervisor.VMStarter for QEMU.
type Starter struct{}

// NewStarter creates a new QEMU starter.
func NewStarter() *Starter {
	return &Starter{}
}

// Verify Starter implements the interface
var _ hypervisor.VMStarter = (*Starter)(nil)

// SocketName returns the socket filename for QEMU.
func (s *Starter) SocketName() string {
	return "qemu.sock"
}

// GetBinaryPath returns the path to the QEMU binary.
// QEMU is expected to be installed on the system.
func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	binaryName, err := qemuBinaryName()
	if err != nil {
		return "", err
	}

	candidates := []string{
		"/usr/bin/" + binaryName,
		"/usr/local/bin/" + binaryName,
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	if path, err := exec.LookPath(binaryName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("%s not found; install with: %s", binaryName, qemuInstallHint())
}

// StartVM launches QEMU with the VM configuration and returns a Hypervisor client.
// QEMU receives all configuration via command-line arguments at process start.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	// Get binary path
	binaryPath, err := s.GetBinaryPath(p, version)
	if err != nil {
		return 0, nil, fmt.Errorf("get binary: %w", err)
	}

	// Check if socket is already in use
	if isSocketInUse(socketPath) {
		return 0, nil, fmt.Errorf("socket already in use, QEMU may be running at %s", socketPath)
	}

	// Remove stale socket if exists
	os.Remove(socketPath)

	// Build command arguments: QMP socket + VM configuration
	args := []string{
		"-chardev", fmt.Sprintf("socket,id=qmp,path=%s,server=on,wait=off", socketPath),
		"-mon", "chardev=qmp,mode=control",
	}
	// Append VM configuration as command-line arguments
	args = append(args, BuildArgs(config)...)

	// Create command
	cmd := exec.Command(binaryPath, args...)

	// Daemonize: detach from parent process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Redirect stdout/stderr to VMM log file
	instanceDir := filepath.Dir(socketPath)
	logsDir := filepath.Join(instanceDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return 0, nil, fmt.Errorf("create logs directory: %w", err)
	}

	vmmLogFile, err := os.OpenFile(
		filepath.Join(logsDir, "vmm.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return 0, nil, fmt.Errorf("create vmm log: %w", err)
	}
	defer vmmLogFile.Close()

	cmd.Stdout = vmmLogFile
	cmd.Stderr = vmmLogFile

	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start qemu: %w", err)
	}

	pid := cmd.Process.Pid

	// Wait for socket to be ready
	if err := waitForSocket(socketPath, 10*time.Second); err != nil {
		vmmLogPath := filepath.Join(logsDir, "vmm.log")
		if logData, readErr := os.ReadFile(vmmLogPath); readErr == nil && len(logData) > 0 {
			return 0, nil, fmt.Errorf("%w; vmm.log: %s", err, string(logData))
		}
		return 0, nil, err
	}

	// Create QMP client
	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create client: %w", err)
	}

	return pid, hv, nil
}

// RestoreVM starts QEMU and restores VM state from a snapshot.
// Not yet implemented for QEMU.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	return 0, nil, fmt.Errorf("restore not supported by QEMU implementation")
}

// qemuBinaryName returns the QEMU binary name for the host architecture.
func qemuBinaryName() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "qemu-system-x86_64", nil
	case "arm64":
		return "qemu-system-aarch64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
}

// qemuInstallHint returns package installation hints for the current architecture.
func qemuInstallHint() string {
	switch runtime.GOARCH {
	case "amd64":
		return "apt install qemu-system-x86 (Debian/Ubuntu) or dnf install qemu-system-x86-core (Fedora)"
	case "arm64":
		return "apt install qemu-system-arm (Debian/Ubuntu) or dnf install qemu-system-aarch64-core (Fedora)"
	default:
		return "install QEMU for your platform"
	}
}

// isSocketInUse checks if a Unix socket is actively being used
func isSocketInUse(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// waitForSocket waits for the QMP socket to become available
func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for socket")
}
