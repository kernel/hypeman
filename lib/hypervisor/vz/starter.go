//go:build darwin

// Package vz implements the hypervisor.Hypervisor interface for
// Apple's Virtualization.framework on macOS via the vz-shim subprocess.
package vz

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
)

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeVZ, "vz.sock")
	hypervisor.RegisterVsockDialerFactory(hypervisor.TypeVZ, NewVsockDialer)
	hypervisor.RegisterClientFactory(hypervisor.TypeVZ, func(socketPath string) (hypervisor.Hypervisor, error) {
		return NewClient(socketPath)
	})
}

// ShimConfig is the configuration passed to the vz-shim process.
type ShimConfig struct {
	VCPUs         int             `json:"vcpus"`
	MemoryBytes   int64           `json:"memory_bytes"`
	Disks         []DiskConfig    `json:"disks"`
	Networks      []NetworkConfig `json:"networks"`
	SerialLogPath string          `json:"serial_log_path"`
	KernelPath    string          `json:"kernel_path"`
	InitrdPath    string          `json:"initrd_path"`
	KernelArgs    string          `json:"kernel_args"`
	ControlSocket string          `json:"control_socket"`
	VsockSocket   string          `json:"vsock_socket"`
	LogPath       string          `json:"log_path"`
}

// DiskConfig for shim.
type DiskConfig struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

// NetworkConfig for shim.
type NetworkConfig struct {
	MAC string `json:"mac"`
}

// Starter implements hypervisor.VMStarter for Virtualization.framework.
type Starter struct{}

// NewStarter creates a new vz starter.
func NewStarter() *Starter {
	return &Starter{}
}

// Verify Starter implements the interface
var _ hypervisor.VMStarter = (*Starter)(nil)

// SocketName returns the socket filename for vz.
func (s *Starter) SocketName() string {
	return "vz.sock"
}

// GetBinaryPath returns empty - vz uses system Virtualization.framework.
func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	return "", nil
}

// GetVersion returns the macOS version as the "hypervisor version".
func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	// Return a version indicating vz availability
	return "vz-macos", nil
}

// StartVM spawns a vz-shim subprocess to host the VM.
// Returns the shim PID and a client to control the VM.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	// Derive socket paths from the control socket path
	instanceDir := filepath.Dir(socketPath)
	controlSocket := socketPath
	vsockSocket := filepath.Join(instanceDir, "vz.vsock")
	logPath := filepath.Join(instanceDir, "logs", "vz-shim.log")

	// Build shim config
	shimConfig := ShimConfig{
		VCPUs:         config.VCPUs,
		MemoryBytes:   config.MemoryBytes,
		SerialLogPath: config.SerialLogPath,
		KernelPath:    config.KernelPath,
		InitrdPath:    config.InitrdPath,
		KernelArgs:    config.KernelArgs,
		ControlSocket: controlSocket,
		VsockSocket:   vsockSocket,
		LogPath:       logPath,
	}

	// Convert disks
	for _, disk := range config.Disks {
		shimConfig.Disks = append(shimConfig.Disks, DiskConfig{
			Path:     disk.Path,
			Readonly: disk.Readonly,
		})
	}

	// Convert networks
	for _, net := range config.Networks {
		shimConfig.Networks = append(shimConfig.Networks, NetworkConfig{
			MAC: net.MAC,
		})
	}

	configJSON, err := json.Marshal(shimConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal shim config: %w", err)
	}

	log.DebugContext(ctx, "spawning vz-shim", "config", string(configJSON))

	// Find the vz-shim binary (same directory as hypeman or in PATH)
	shimPath, err := s.findShimBinary()
	if err != nil {
		return 0, nil, fmt.Errorf("find vz-shim binary: %w", err)
	}

	// Spawn the shim process
	cmd := exec.CommandContext(ctx, shimPath, "-config", string(configJSON))
	cmd.Stdout = nil // Shim logs to file
	cmd.Stderr = nil
	cmd.Stdin = nil

	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start vz-shim: %w", err)
	}

	pid := cmd.Process.Pid
	log.InfoContext(ctx, "vz-shim started", "pid", pid, "control_socket", controlSocket)

	// Wait for the control socket to be ready
	client, err := s.waitForShim(ctx, controlSocket, 30*time.Second)
	if err != nil {
		// Kill the shim if we can't connect
		cmd.Process.Kill()
		return 0, nil, fmt.Errorf("connect to vz-shim: %w", err)
	}

	// Release the process so it's not killed when cmd goes out of scope
	cmd.Process.Release()

	return pid, client, nil
}

// findShimBinary locates the vz-shim binary.
func (s *Starter) findShimBinary() (string, error) {
	// First, check next to the current executable
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		shimPath := filepath.Join(exeDir, "vz-shim")
		if _, err := os.Stat(shimPath); err == nil {
			return shimPath, nil
		}
		// Also check parent's tmp dir (for air hot-reload development)
		// When running ./tmp/main, check ./tmp/vz-shim
		if filepath.Base(exeDir) == "tmp" {
			shimPath = filepath.Join(exeDir, "vz-shim")
			if _, err := os.Stat(shimPath); err == nil {
				return shimPath, nil
			}
		}
	}

	// Check in PATH
	shimPath, err := exec.LookPath("vz-shim")
	if err == nil {
		return shimPath, nil
	}

	// Check common locations
	commonPaths := []string{
		"/usr/local/bin/vz-shim",
		filepath.Join(os.Getenv("HOME"), "bin", "vz-shim"),
	}
	for _, p := range commonPaths {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("vz-shim binary not found")
}

// waitForShim waits for the shim's control socket to be ready.
func (s *Starter) waitForShim(ctx context.Context, socketPath string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		client, err := NewClient(socketPath)
		if err == nil {
			return client, nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return nil, fmt.Errorf("timeout waiting for shim socket: %s", socketPath)
}

// RestoreVM restores a VM from a snapshot.
// Note: Snapshot restore via shim is not yet implemented.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	return 0, nil, fmt.Errorf("vz snapshot restore not implemented via shim")
}

// RestoreVMWithConfig restores a VM from a snapshot.
// Note: Snapshot restore via shim is not yet implemented.
func (s *Starter) RestoreVMWithConfig(ctx context.Context, p *paths.Paths, config hypervisor.VMConfig, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	return 0, nil, fmt.Errorf("vz snapshot restore not implemented via shim")
}
