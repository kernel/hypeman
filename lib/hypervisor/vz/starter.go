//go:build darwin

// Package vz implements the hypervisor.Hypervisor interface for
// Apple's Virtualization.framework on macOS via the vz-shim subprocess.
package vz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
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

var (
	shimOnce sync.Once
	shimPath string
	shimErr  error
)

// extractShim extracts the embedded vz-shim binary to a temp file and codesigns it.
func extractShim() (string, error) {
	shimOnce.Do(func() {
		f, err := os.CreateTemp("", "vz-shim-*")
		if err != nil {
			shimErr = fmt.Errorf("create temp file: %w", err)
			return
		}
		defer f.Close()

		if _, err := f.Write(vzShimBinary); err != nil {
			os.Remove(f.Name())
			shimErr = fmt.Errorf("write vz-shim binary: %w", err)
			return
		}

		if err := f.Chmod(0755); err != nil {
			os.Remove(f.Name())
			shimErr = fmt.Errorf("chmod vz-shim binary: %w", err)
			return
		}

		// Write embedded entitlements to a temp file for codesigning
		entFile, err := os.CreateTemp("", "vz-entitlements-*.plist")
		if err != nil {
			os.Remove(f.Name())
			shimErr = fmt.Errorf("create entitlements temp file: %w", err)
			return
		}
		defer os.Remove(entFile.Name())

		if _, err := entFile.Write(vzEntitlements); err != nil {
			os.Remove(f.Name())
			entFile.Close()
			shimErr = fmt.Errorf("write entitlements file: %w", err)
			return
		}
		entFile.Close()

		// Codesign with entitlements for Virtualization.framework
		cmd := exec.Command("codesign", "--sign", "-", "--entitlements", entFile.Name(), "--force", f.Name())
		if out, err := cmd.CombinedOutput(); err != nil {
			os.Remove(f.Name())
			shimErr = fmt.Errorf("codesign vz-shim: %s: %w", string(out), err)
			return
		}

		shimPath = f.Name()
	})
	return shimPath, shimErr
}

// Starter implements hypervisor.VMStarter for Virtualization.framework.
type Starter struct{}

// NewStarter creates a new vz starter.
func NewStarter() *Starter {
	return &Starter{}
}

var _ hypervisor.VMStarter = (*Starter)(nil)

func (s *Starter) SocketName() string {
	return "vz.sock"
}

// GetBinaryPath extracts the embedded vz-shim and returns its path.
func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	return extractShim()
}

// GetVersion returns "vz-shim".
func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	return "vz-shim", nil
}

// StartVM spawns a vz-shim subprocess to host the VM.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	instanceDir := filepath.Dir(socketPath)
	controlSocket := socketPath
	vsockSocket := filepath.Join(instanceDir, "vz.vsock")
	logPath := filepath.Join(instanceDir, "logs", "vz-shim.log")

	shimConfig := shimconfig.ShimConfig{
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

	for _, disk := range config.Disks {
		shimConfig.Disks = append(shimConfig.Disks, shimconfig.DiskConfig{
			Path:     disk.Path,
			Readonly: disk.Readonly,
		})
	}

	for _, net := range config.Networks {
		shimConfig.Networks = append(shimConfig.Networks, shimconfig.NetworkConfig{
			MAC: net.MAC,
		})
	}

	configJSON, err := json.Marshal(shimConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal shim config: %w", err)
	}

	log.DebugContext(ctx, "spawning vz-shim", "config", string(configJSON))

	shimBinary, err := s.GetBinaryPath(p, version)
	if err != nil {
		return 0, nil, fmt.Errorf("get vz-shim binary: %w", err)
	}

	var shimStderr bytes.Buffer
	cmd := exec.Command(shimBinary, "-config", string(configJSON))
	cmd.Stdout = nil
	cmd.Stderr = &shimStderr
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("start vz-shim: %w", err)
	}

	pid := cmd.Process.Pid
	log.InfoContext(ctx, "vz-shim started", "pid", pid, "control_socket", controlSocket)

	// Wait for shim in a goroutine so we can detect early exit
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	client, err := s.waitForShim(ctx, controlSocket, 30*time.Second)
	if err != nil {
		// Read shim log file for diagnostics (before instance dir cleanup deletes it)
		shimLog := ""
		if logData, readErr := os.ReadFile(logPath); readErr == nil && len(logData) > 0 {
			shimLog = string(logData)
		}

		// Check if shim already exited (crashed during startup)
		select {
		case waitErr := <-waitDone:
			stderr := shimStderr.String()
			details := ""
			if stderr != "" {
				details += fmt.Sprintf(" (stderr: %s)", stderr)
			}
			if shimLog != "" {
				details += fmt.Sprintf(" (shim log: %s)", shimLog)
			}
			return 0, nil, fmt.Errorf("vz-shim exited early: %v%s", waitErr, details)
		default:
			// Shim still running but socket not available
			cmd.Process.Kill()
			<-waitDone
		}
		if shimLog != "" {
			return 0, nil, fmt.Errorf("connect to vz-shim: %w (shim log: %s)", err, shimLog)
		}
		return 0, nil, fmt.Errorf("connect to vz-shim: %w", err)
	}

	return pid, client, nil
}

// RestoreVM is not supported by vz (Virtualization.framework cannot restore Linux guests).
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	return 0, nil, hypervisor.ErrNotSupported
}

// PrepareFork is not supported for vz.
func (s *Starter) PrepareFork(ctx context.Context, req hypervisor.ForkPrepareRequest) (hypervisor.ForkPrepareResult, error) {
	_ = ctx
	_ = req
	return hypervisor.ForkPrepareResult{}, hypervisor.ErrNotSupported
}

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
