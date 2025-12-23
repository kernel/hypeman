// Package qemu implements the hypervisor.Hypervisor interface for QEMU.
package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"

	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/paths"
	"gvisor.dev/gvisor/pkg/cleanup"
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

// GetVersion returns the version of the installed QEMU binary.
// Parses the output of "qemu-system-* --version" to extract the version string.
func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	binaryPath, err := s.GetBinaryPath(p, "")
	if err != nil {
		return "", err
	}

	cmd := exec.Command(binaryPath, "--version")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get qemu version: %w", err)
	}

	// Parse "QEMU emulator version 8.2.0 (Debian ...)" -> "8.2.0"
	re := regexp.MustCompile(`version (\d+\.\d+(?:\.\d+)?)`)
	matches := re.FindStringSubmatch(string(output))
	if len(matches) >= 2 {
		return matches[1], nil
	}

	return "", fmt.Errorf("could not parse QEMU version from: %s", string(output))
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

	// Setup cleanup to kill the process if subsequent steps fail
	cu := cleanup.Make(func() {
		syscall.Kill(pid, syscall.SIGKILL)
	})
	defer cu.Clean()

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

	// Save config for potential restore later
	// QEMU migration files only contain memory state, not device config
	if err := saveVMConfig(instanceDir, config); err != nil {
		// Non-fatal - restore just won't work
		// Log would be nice but we don't have logger here
	}

	// Success - release cleanup to prevent killing the process
	cu.Release()
	return pid, hv, nil
}

// RestoreVM starts QEMU and restores VM state from a snapshot.
// The VM is in paused state after restore; caller should call Resume() to continue execution.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
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

	// Load saved VM config from snapshot directory
	// QEMU requires exact same command-line args as when snapshot was taken
	config, err := loadVMConfig(snapshotPath)
	if err != nil {
		return 0, nil, fmt.Errorf("load vm config from snapshot: %w", err)
	}

	instanceDir := filepath.Dir(socketPath)

	// Build command arguments: QMP socket + VM configuration + incoming migration
	args := []string{
		"-chardev", fmt.Sprintf("socket,id=qmp,path=%s,server=on,wait=off", socketPath),
		"-mon", "chardev=qmp,mode=control",
	}
	// Append VM configuration as command-line arguments
	args = append(args, BuildArgs(config)...)

	// Add incoming migration flag to restore from snapshot
	// The snapshot file is named "memory" in the snapshot directory
	incomingURI := "file://" + filepath.Join(snapshotPath, "memory")
	args = append(args, "-incoming", incomingURI)

	// Create command
	cmd := exec.Command(binaryPath, args...)

	// Daemonize: detach from parent process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Redirect stdout/stderr to VMM log file
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

	// Setup cleanup to kill the process if subsequent steps fail
	cu := cleanup.Make(func() {
		syscall.Kill(pid, syscall.SIGKILL)
	})
	defer cu.Clean()

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

	// Success - release cleanup to prevent killing the process
	cu.Release()
	return pid, hv, nil
}

// vmConfigFile is the name of the file where VM config is saved for restore.
const vmConfigFile = "qemu-config.json"

// saveVMConfig saves the VM configuration to a file in the instance directory.
// This is needed for QEMU restore since migration files only contain memory state.
func saveVMConfig(instanceDir string, config hypervisor.VMConfig) error {
	configPath := filepath.Join(instanceDir, vmConfigFile)
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// loadVMConfig loads the VM configuration from the instance directory.
func loadVMConfig(instanceDir string) (hypervisor.VMConfig, error) {
	configPath := filepath.Join(instanceDir, vmConfigFile)
	data, err := os.ReadFile(configPath)
	if err != nil {
		return hypervisor.VMConfig{}, fmt.Errorf("read config: %w", err)
	}
	var config hypervisor.VMConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return hypervisor.VMConfig{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return config, nil
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
