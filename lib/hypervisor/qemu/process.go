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

// ProcessManager implements hypervisor.ProcessManager for QEMU.
type ProcessManager struct{}

// NewProcessManager creates a new QEMU process manager.
func NewProcessManager() *ProcessManager {
	return &ProcessManager{}
}

// Verify ProcessManager implements the interface
var _ hypervisor.ProcessManager = (*ProcessManager)(nil)

// SocketName returns the socket filename for QEMU.
func (p *ProcessManager) SocketName() string {
	return "qemu.sock"
}

// StartProcess launches a QEMU VMM process.
func (p *ProcessManager) StartProcess(ctx context.Context, paths *paths.Paths, version string, socketPath string) (int, error) {
	return p.StartProcessWithArgs(ctx, paths, version, socketPath, nil)
}

// StartProcessWithArgs launches a QEMU VMM process with extra arguments.
func (p *ProcessManager) StartProcessWithArgs(ctx context.Context, paths *paths.Paths, version string, socketPath string, extraArgs []string) (int, error) {
	// Get binary path
	binaryPath, err := p.GetBinaryPath(paths, version)
	if err != nil {
		return 0, fmt.Errorf("get binary: %w", err)
	}

	// Check if socket is already in use
	if isSocketInUse(socketPath) {
		return 0, fmt.Errorf("socket already in use, QEMU may be running at %s", socketPath)
	}

	// Remove stale socket if exists
	os.Remove(socketPath)

	// Build base command arguments for QMP socket
	args := []string{
		"-chardev", fmt.Sprintf("socket,id=qmp,path=%s,server=on,wait=off", socketPath),
		"-mon", "chardev=qmp,mode=control",
	}
	args = append(args, extraArgs...)

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
		return 0, fmt.Errorf("create logs directory: %w", err)
	}

	vmmLogFile, err := os.OpenFile(
		filepath.Join(logsDir, "vmm.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return 0, fmt.Errorf("create vmm log: %w", err)
	}
	defer vmmLogFile.Close()

	cmd.Stdout = vmmLogFile
	cmd.Stderr = vmmLogFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start qemu: %w", err)
	}

	pid := cmd.Process.Pid

	// Wait for socket to be ready
	if err := waitForSocket(socketPath, 10*time.Second); err != nil {
		// Read vmm.log to understand why socket wasn't created
		vmmLogPath := filepath.Join(logsDir, "vmm.log")
		if logData, readErr := os.ReadFile(vmmLogPath); readErr == nil && len(logData) > 0 {
			return 0, fmt.Errorf("%w; vmm.log: %s", err, string(logData))
		}
		return 0, err
	}

	return pid, nil
}

// GetBinaryPath returns the path to the QEMU binary.
// QEMU is expected to be installed on the system.
func (p *ProcessManager) GetBinaryPath(paths *paths.Paths, version string) (string, error) {
	// Determine binary name based on host architecture
	binaryName, err := qemuBinaryName()
	if err != nil {
		return "", err
	}

	// Look for system-installed QEMU
	candidates := []string{
		"/usr/bin/" + binaryName,
		"/usr/local/bin/" + binaryName,
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// Try PATH lookup
	if path, err := exec.LookPath(binaryName); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("%s not found; install with: %s", binaryName, qemuInstallHint())
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
