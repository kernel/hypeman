package firecracker

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/uffdpager"
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

type UFFDClient interface {
	CreateSession(ctx context.Context, req uffdpager.CreateSessionRequest) (*uffdpager.CreateSessionResponse, error)
	CloseSession(ctx context.Context, sessionID string) error
}

type StarterOption func(*Starter)

// Starter implements hypervisor.VMStarter for Firecracker.
type Starter struct {
	uffd UFFDClient
}

func NewStarter(opts ...StarterOption) *Starter {
	s := &Starter{}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func WithUFFDClient(client UFFDClient) StarterOption {
	return func(s *Starter) {
		s.uffd = client
	}
}

var _ hypervisor.VMStarter = (*Starter)(nil)

func (s *Starter) ValidateConfig(config hypervisor.VMConfig) error {
	return hypervisor.ValidateDirectRawConfig("firecracker", config)
}

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

func (s *Starter) ResolveVersion(p *paths.Paths, requested string) (string, error) {
	if requested == "" {
		return s.GetVersion(p)
	}
	if _, err := s.GetBinaryPath(p, requested); err != nil {
		return "", err
	}
	return requested, nil
}

func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	if err := s.ValidateConfig(config); err != nil {
		return 0, nil, fmt.Errorf("validate firecracker config: %w", err)
	}
	processCtx, processSpan := hypervisor.StartProcessSpan(ctx, hypervisor.TypeFirecracker)
	pid, err := s.startProcess(processCtx, p, version, socketPath)
	hypervisor.FinishTraceSpan(processSpan, err)
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

func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string, opts hypervisor.RestoreOptions) (int, hypervisor.Hypervisor, error) {
	processCtx, processSpan := hypervisor.StartProcessSpan(ctx, hypervisor.TypeFirecracker)
	pid, err := s.startProcess(processCtx, p, version, socketPath)
	hypervisor.FinishTraceSpan(processSpan, err)
	if err != nil {
		return 0, nil, fmt.Errorf("start firecracker process: %w", err)
	}

	cu := cleanup.Make(func() {
		cleanupFailedRestore(pid, socketPath)
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
	backend := fileSnapshotMemBackend(snapshotPath)
	createdUFFDSession := ""
	if opts.SnapshotMemoryBackend == hypervisor.SnapshotMemoryBackendUFFD {
		if s.uffd == nil {
			return 0, nil, fmt.Errorf("uffd snapshot restore requested but no uffd pager is configured")
		}
		sessionID := strings.TrimSpace(opts.SnapshotMemorySessionID)
		if sessionID == "" {
			sessionID = filepath.Base(filepath.Dir(socketPath))
		}
		backingMemoryPath := strings.TrimSpace(opts.SnapshotMemoryBackingPath)
		if backingMemoryPath == "" {
			backingMemoryPath = snapshotMemoryPath(snapshotPath)
		}
		resp, err := s.uffd.CreateSession(ctx, uffdpager.CreateSessionRequest{
			SessionID:         sessionID,
			InstanceID:        sessionID,
			BackingMemoryPath: backingMemoryPath,
			CacheKey:          opts.SnapshotMemoryCacheKey,
		})
		if err != nil {
			return 0, nil, fmt.Errorf("create uffd pager session: %w", err)
		}
		createdUFFDSession = resp.SessionID
		backend = snapshotMemBackend{BackendType: "Uffd", BackendPath: resp.UFFDSocketPath}
	}
	targetDataDir := filepath.Dir(socketPath)
	loadSnapshot := func() error {
		return hv.loadSnapshot(ctx, snapshotPath, meta.NetworkOverrides, backend)
	}
	if needsSnapshotSourceDirAlias(meta, targetDataDir) {
		err = func() error {
			unlockAlias := hypervisor.LockSnapshotSourceAliasMutation()
			defer unlockAlias()
			return withSnapshotSourceDirAlias(meta, targetDataDir, loadSnapshot)
		}()
	} else {
		err = loadSnapshot()
	}
	if err != nil {
		if createdUFFDSession != "" {
			_ = s.uffd.CloseSession(context.Background(), createdUFFDSession)
		}
		return 0, nil, fmt.Errorf("load firecracker snapshot: %w", err)
	}
	if meta.SnapshotSourceDataDir != "" && !meta.RetainSnapshotSourceDataDirAlias {
		meta.SnapshotSourceDataDir = ""
		if err := saveRestoreMetadataState(filepath.Dir(socketPath), meta); err != nil {
			return 0, nil, fmt.Errorf("clear firecracker snapshot source alias metadata: %w", err)
		}
	}

	cu.Release()
	return pid, hv, nil
}

func cleanupFailedRestore(pid int, socketPath string) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_, _ = syscall.Wait4(pid, nil, 0, nil)
	_ = os.Remove(socketPath)
}

func withSnapshotSourceDirAlias(meta *restoreMetadata, targetDataDir string, run func() error) error {
	if meta == nil || meta.SnapshotSourceDataDir == "" {
		return run()
	}

	sourceDataDir := filepath.Clean(meta.SnapshotSourceDataDir)
	targetDataDir = filepath.Clean(targetDataDir)
	if sourceDataDir == targetDataDir {
		return run()
	}
	if rel, err := filepath.Rel(sourceDataDir, targetDataDir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("invalid snapshot source alias: target data dir %q must not be nested under source data dir %q", targetDataDir, sourceDataDir)
	}
	if rel, err := filepath.Rel(targetDataDir, sourceDataDir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("invalid snapshot source alias: source data dir %q must not be nested under target data dir %q", sourceDataDir, targetDataDir)
	}

	if err := os.MkdirAll(filepath.Dir(sourceDataDir), 0755); err != nil {
		return fmt.Errorf("ensure snapshot source parent dir: %w", err)
	}

	backupDataDir := ""
	if _, err := os.Stat(sourceDataDir); err == nil {
		backupDataDir = fmt.Sprintf("%s.fork-bak.%d", sourceDataDir, time.Now().UnixNano())
		if err := os.Rename(sourceDataDir, backupDataDir); err != nil {
			return fmt.Errorf("backup snapshot source data dir: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat snapshot source data dir: %w", err)
	}

	if err := os.Symlink(targetDataDir, sourceDataDir); err != nil {
		if backupDataDir != "" {
			_ = os.Rename(backupDataDir, sourceDataDir)
		}
		return fmt.Errorf("symlink snapshot source data dir: %w", err)
	}

	runErr := run()

	removeErr := os.Remove(sourceDataDir)
	restoreErr := error(nil)
	if backupDataDir != "" {
		restoreErr = os.Rename(backupDataDir, sourceDataDir)
	}

	if runErr != nil {
		if removeErr != nil {
			return fmt.Errorf("%v; cleanup snapshot source symlink: %w", runErr, removeErr)
		}
		if restoreErr != nil {
			return fmt.Errorf("%v; restore snapshot source data dir: %w", runErr, restoreErr)
		}
		return runErr
	}
	if removeErr != nil {
		return fmt.Errorf("cleanup snapshot source symlink: %w", removeErr)
	}
	if restoreErr != nil {
		return fmt.Errorf("restore snapshot source data dir: %w", restoreErr)
	}
	return nil
}

func needsSnapshotSourceDirAlias(meta *restoreMetadata, targetDataDir string) bool {
	if meta == nil || meta.SnapshotSourceDataDir == "" {
		return false
	}
	sourceDataDir := filepath.Clean(meta.SnapshotSourceDataDir)
	targetDataDir = filepath.Clean(targetDataDir)
	return sourceDataDir != targetDataDir
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
