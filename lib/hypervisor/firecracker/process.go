package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"gvisor.dev/gvisor/pkg/cleanup"
)

const (
	socketWaitTimeout = 10 * time.Second
	socketPollEvery   = 50 * time.Millisecond
	socketDialTimeout = 100 * time.Millisecond
)

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeFirecracker, "fc.sock")
	hypervisor.RegisterClientFactory(hypervisor.TypeFirecracker, func(socketPath string) (hypervisor.Hypervisor, error) {
		return New(socketPath)
	})
}

// Starter implements hypervisor.VMStarter for Firecracker.
type Starter struct{}

func NewStarter() *Starter {
	return &Starter{}
}

var _ hypervisor.VMStarter = (*Starter)(nil)

func (s *Starter) SocketName() string {
	return "fc.sock"
}

func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	return resolveBinaryPath(p, version)
}

func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	if path := getCustomBinaryPath(); path != "" {
		version, err := detectVersion(path)
		if err != nil {
			return "custom", nil
		}
		return version, nil
	}
	return string(defaultVersion), nil
}

func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	pid, err := s.startProcess(ctx, p, version, socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("start firecracker process: %w", err)
	}

	cu := cleanup.Make(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	defer cu.Clean()

	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create firecracker client: %w", err)
	}

	if err := hv.configureForBoot(ctx, config); err != nil {
		return 0, nil, fmt.Errorf("configure firecracker vm: %w", err)
	}
	if err := saveRestoreMetadata(filepath.Dir(socketPath), toNetworkInterfaces(config)); err != nil {
		return 0, nil, fmt.Errorf("persist firecracker restore metadata: %w", err)
	}
	if err := hv.instanceStart(ctx); err != nil {
		return 0, nil, fmt.Errorf("start firecracker vm: %w", err)
	}

	cu.Release()
	return pid, hv, nil
}

func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	pid, err := s.startProcess(ctx, p, version, socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("start firecracker process: %w", err)
	}

	cu := cleanup.Make(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	defer cu.Clean()

	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create firecracker client: %w", err)
	}

	meta, err := loadRestoreMetadata(filepath.Dir(socketPath))
	if err != nil {
		return 0, nil, fmt.Errorf("load firecracker restore metadata: %w", err)
	}
	if err := hv.loadSnapshot(ctx, snapshotPath, meta.NetworkOverrides); err != nil {
		return 0, nil, fmt.Errorf("load firecracker snapshot: %w", err)
	}

	cu.Release()
	return pid, hv, nil
}

func (s *Starter) startProcess(_ context.Context, p *paths.Paths, version string, socketPath string) (int, error) {
	binaryPath, err := s.GetBinaryPath(p, version)
	if err != nil {
		return 0, fmt.Errorf("resolve firecracker binary: %w", err)
	}

	if isSocketInUse(socketPath) {
		return 0, fmt.Errorf("socket already in use, firecracker may already be running at %s", socketPath)
	}
	_ = os.Remove(socketPath)

	instanceDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(filepath.Join(instanceDir, "logs"), 0755); err != nil {
		return 0, fmt.Errorf("create logs directory: %w", err)
	}

	vmmLogPath := filepath.Join(instanceDir, "logs", "vmm.log")
	vmmLogFile, err := os.OpenFile(vmmLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, fmt.Errorf("create vmm log file: %w", err)
	}
	defer vmmLogFile.Close()

	// Use Command (not CommandContext) so the VMM survives request-scoped context cancellation.
	cmd := exec.Command(binaryPath, "--api-sock", socketPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = vmmLogFile
	cmd.Stderr = vmmLogFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start firecracker: %w", err)
	}

	if err := waitForSocket(socketPath, socketWaitTimeout); err != nil {
		if data, readErr := os.ReadFile(vmmLogPath); readErr == nil && len(data) > 0 {
			return 0, fmt.Errorf("%w; vmm.log: %s", err, string(data))
		}
		return 0, err
	}

	return cmd.Process.Pid, nil
}

func isSocketInUse(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, socketDialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", path, socketDialTimeout)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(socketPollEvery)
	}
	return fmt.Errorf("timeout waiting for socket")
}
