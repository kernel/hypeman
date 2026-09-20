package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/vmm"
	"gvisor.dev/gvisor/pkg/cleanup"
)

var (
	defaultVersionMu       sync.RWMutex
	defaultVersionOverride *vmm.CHVersion
)

// SetDefaultVersion overrides the default Cloud Hypervisor version for new
// instances. An empty string clears the override (vmm.DefaultVersion is used).
// Returns an error if the version is non-empty and not in SupportedVersions.
func SetDefaultVersion(v string) error {
	defaultVersionMu.Lock()
	defer defaultVersionMu.Unlock()
	if v == "" {
		defaultVersionOverride = nil
		return nil
	}
	chv := vmm.CHVersion(v)
	if !vmm.IsVersionSupported(chv) {
		return fmt.Errorf("unsupported cloud-hypervisor version: %q (supported: %v)", v, vmm.SupportedVersions)
	}
	defaultVersionOverride = &chv
	return nil
}

// GetDefaultVersion returns the configured default, falling back to
// vmm.DefaultVersion if no override is set.
func GetDefaultVersion() vmm.CHVersion {
	defaultVersionMu.RLock()
	defer defaultVersionMu.RUnlock()
	if defaultVersionOverride != nil {
		return *defaultVersionOverride
	}
	return vmm.DefaultVersion
}

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeCloudHypervisor, "ch.sock")
	hypervisor.RegisterClientFactory(hypervisor.TypeCloudHypervisor, func(socketPath string) (hypervisor.Hypervisor, error) {
		return New(socketPath)
	})
}

// Starter implements hypervisor.VMStarter for Cloud Hypervisor.
type Starter struct{}

// NewStarter creates a new Cloud Hypervisor starter.
func NewStarter() *Starter {
	return &Starter{}
}

// Verify Starter implements the interface
var _ hypervisor.VMStarter = (*Starter)(nil)

func (s *Starter) ValidateConfig(config hypervisor.VMConfig) error {
	return hypervisor.ValidateDirectRawConfig("cloud-hypervisor", config)
}

// SocketName returns the socket filename for Cloud Hypervisor.
func (s *Starter) SocketName() string {
	return "ch.sock"
}

// GetBinaryPath returns the path to the Cloud Hypervisor binary.
func (s *Starter) GetBinaryPath(p *paths.Paths, version string) (string, error) {
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return "", fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}
	return vmm.GetBinaryPath(p, chVersion)
}

// GetVersion returns the configured default Cloud Hypervisor version.
// This controls which version is used for new instances; existing instances
// use the version stored in their metadata.
func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	return string(GetDefaultVersion()), nil
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

// StartVM launches Cloud Hypervisor, configures the VM, and boots it.
// Returns the process ID and a Hypervisor client for subsequent operations.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	if err := s.ValidateConfig(config); err != nil {
		return 0, nil, fmt.Errorf("validate cloud-hypervisor config: %w", err)
	}
	log := logger.FromContext(ctx)

	// Validate version
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, nil, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}

	// 0. Start the serial reader before CH so the unix socket is bound by
	// the time CH boots and tries to connect.
	sr, err := startSerialReader(ctx, serialSocketPath(config.SerialLogPath), config.SerialLogPath)
	if err != nil {
		return 0, nil, fmt.Errorf("start serial reader: %w", err)
	}

	// 1. Start the Cloud Hypervisor process
	processCtx, processSpan := hypervisor.StartProcessSpan(ctx, hypervisor.TypeCloudHypervisor)
	pid, err := vmm.StartProcess(processCtx, p, chVersion, socketPath)
	hypervisor.FinishTraceSpan(processSpan, err)
	if err != nil {
		sr.Close()
		return 0, nil, fmt.Errorf("start process: %w", err)
	}

	// Setup cleanup to kill the process if subsequent steps fail
	cu := cleanup.Make(func() {
		syscall.Kill(pid, syscall.SIGKILL)
		sr.Close()
	})
	defer cu.Clean()

	// 2. Create the HTTP client
	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create client: %w", err)
	}

	// 3. Configure the VM via HTTP API
	vmConfig := ToVMConfig(config)
	resp, err := hv.client.CreateVMWithResponse(ctx, vmConfig)
	if err != nil {
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "create_vm", err, 0, "")
		return 0, nil, fmt.Errorf("create vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "create_vm", nil, resp.StatusCode(), string(resp.Body))
		return 0, nil, fmt.Errorf("create vm failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}

	// 4. Boot the VM via HTTP API
	bootResp, err := hv.client.BootVMWithResponse(ctx)
	if err != nil {
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "boot_vm", err, 0, "")
		return 0, nil, fmt.Errorf("boot vm: %w", err)
	}
	if bootResp.StatusCode() != 204 {
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "boot_vm", nil, bootResp.StatusCode(), string(bootResp.Body))
		return 0, nil, fmt.Errorf("boot vm failed with status %d: %s", bootResp.StatusCode(), string(bootResp.Body))
	}

	// Success - release cleanup to prevent killing the process. Hand
	// ownership of the serial reader to the client so Shutdown can
	// stop it.
	hv.serial = sr
	cu.Release()
	return pid, hv, nil
}

// RestoreVM starts Cloud Hypervisor and restores VM state from a snapshot.
// The VM is in paused state after restore; caller should call Resume() to continue execution.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string, _ hypervisor.RestoreOptions) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)
	startTime := time.Now()

	// Validate version
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, nil, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}

	// 0. Start the serial reader before CH. The serial log path lives at
	// a fixed offset from the CH API socket within the instance directory.
	logPath := filepath.Join(filepath.Dir(socketPath), "logs", "app.log")
	sr, err := startSerialReader(ctx, serialSocketPath(logPath), logPath)
	if err != nil {
		return 0, nil, fmt.Errorf("start serial reader: %w", err)
	}

	// Migrate legacy serial.mode=File snapshots to Socket so CH binds the
	// reader's socket on restore. New snapshots are already Socket; the
	// rewrite is idempotent.
	if err := rewriteSerialConfigForRestore(filepath.Join(snapshotPath, "config.json"), logPath); err != nil {
		sr.Close()
		return 0, nil, fmt.Errorf("rewrite snapshot serial config: %w", err)
	}

	// 1. Start the Cloud Hypervisor process
	processStartTime := time.Now()
	processCtx, processSpan := hypervisor.StartProcessSpan(ctx, hypervisor.TypeCloudHypervisor)
	pid, err := vmm.StartProcess(processCtx, p, chVersion, socketPath)
	hypervisor.FinishTraceSpan(processSpan, err)
	if err != nil {
		sr.Close()
		return 0, nil, fmt.Errorf("start process: %w", err)
	}
	log.DebugContext(ctx, "CH process started", "pid", pid, "duration_ms", time.Since(processStartTime).Milliseconds())

	// Setup cleanup to kill the process if subsequent steps fail
	cu := cleanup.Make(func() {
		syscall.Kill(pid, syscall.SIGKILL)
		sr.Close()
	})
	defer cu.Clean()

	// 2. Create the HTTP client
	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create client: %w", err)
	}

	// 3. Restore from snapshot via HTTP API
	restoreAPIStart := time.Now()
	sourceURL := "file://" + snapshotPath
	restoreConfig := vmm.RestoreConfig{
		SourceUrl: sourceURL,
		Prefault:  ptr(false),
	}
	resp, err := hv.client.PutVmRestoreWithResponse(ctx, restoreConfig)
	if err != nil {
		return 0, nil, fmt.Errorf("restore: %w", err)
	}
	if resp.StatusCode() != 204 {
		return 0, nil, fmt.Errorf("restore failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	log.DebugContext(ctx, "CH restore API complete", "duration_ms", time.Since(restoreAPIStart).Milliseconds())

	// Success - release cleanup to prevent killing the process. Hand
	// ownership of the serial reader to the client so Shutdown can
	// stop it.
	hv.serial = sr
	cu.Release()
	log.DebugContext(ctx, "CH restore complete", "pid", pid, "total_duration_ms", time.Since(startTime).Milliseconds())
	return pid, hv, nil
}

func ptr[T any](v T) *T {
	return &v
}

func logStartVMFailureDiagnostics(ctx context.Context, log *slog.Logger, socketPath string, pid int, operation string, requestErr error, statusCode int, responseBody string) {
	if log == nil {
		return
	}

	socketExists := false
	if _, err := os.Stat(socketPath); err == nil {
		socketExists = true
	}

	socketDialable, socketDialErr := canDialUnixSocket(socketPath)
	processRunning := false
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		processRunning = true
	}

	ctxErr := ctx.Err()
	ctxCause := context.Cause(ctx)
	deadline, hasDeadline := ctx.Deadline()

	attrs := []any{
		"operation", operation,
		"socket_path", socketPath,
		"socket_exists", socketExists,
		"socket_dialable", socketDialable,
		"pid", pid,
		"process_running", processRunning,
		"ctx_err", errorString(ctxErr),
		"ctx_cause", errorString(ctxCause),
		"vmm_log_tail", tailFile(filepath.Join(filepath.Dir(socketPath), "logs", "vmm.log"), 4096),
	}
	if hasDeadline {
		attrs = append(attrs,
			"ctx_deadline", deadline.UTC().Format(time.RFC3339Nano),
			"ctx_deadline_in_ms", time.Until(deadline).Milliseconds(),
		)
	}
	if socketDialErr != nil {
		attrs = append(attrs, "socket_dial_error", socketDialErr.Error())
	}
	if requestErr != nil {
		attrs = append(attrs,
			"request_error", requestErr.Error(),
			"request_error_is_context_canceled", errors.Is(requestErr, context.Canceled),
			"request_error_is_deadline_exceeded", errors.Is(requestErr, context.DeadlineExceeded),
		)
	}
	if statusCode > 0 {
		attrs = append(attrs, "response_status_code", statusCode)
	}
	if responseBody != "" {
		attrs = append(attrs, "response_body", truncateString(responseBody, 1024))
	}

	log.ErrorContext(ctx, "cloud-hypervisor start_vm diagnostics", attrs...)
}

func canDialUnixSocket(socketPath string) (bool, error) {
	conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	return true, nil
}

func tailFile(path string, maxBytes int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	if len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
	}
	return truncateString(strings.TrimSpace(string(data)), maxBytes)
}

func truncateString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
