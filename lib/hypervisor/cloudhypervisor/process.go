package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/vmm"
	"gvisor.dev/gvisor/pkg/cleanup"
)

func init() {
	hypervisor.RegisterSocketName(hypervisor.TypeCloudHypervisor, "ch.sock")
	hypervisor.RegisterCapabilities(hypervisor.TypeCloudHypervisor, capabilities())
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

// GetVersion returns the latest supported Cloud Hypervisor version.
// Cloud Hypervisor binaries are embedded, so we return the latest known version.
func (s *Starter) GetVersion(p *paths.Paths) (string, error) {
	return string(vmm.V49_0), nil
}

// StartVM launches Cloud Hypervisor, configures the VM, and boots it.
// Returns the process ID and a Hypervisor client for subsequent operations.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)

	// Validate version
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, nil, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}

	// 1. Start the Cloud Hypervisor process
	processCtx, processSpan := hypervisor.StartProcessSpan(ctx, hypervisor.TypeCloudHypervisor)
	pid, err := vmm.StartProcess(processCtx, p, chVersion, socketPath)
	hypervisor.FinishTraceSpan(processSpan, err)
	if err != nil {
		return 0, nil, fmt.Errorf("start process: %w", err)
	}

	// Setup cleanup to kill the process if subsequent steps fail
	cu := cleanup.Make(func() {
		syscall.Kill(pid, syscall.SIGKILL)
	})
	defer cu.Clean()

	// 2. Create the HTTP client
	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create client: %w", err)
	}

	// 3. Configure the VM via HTTP API
	vmConfig := ToVMConfig(config)
	serialSocketPath, serialLogPath := serialLogPathsFromVMConfig(vmConfig)
	if serialSocketPath != "" {
		if err := removeStaleSerialSocket(serialSocketPath); err != nil {
			return 0, nil, err
		}
	}
	resp, err := hv.client.CreateVMWithResponse(ctx, vmConfig)
	if err != nil {
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "create_vm", err, 0, "")
		return 0, nil, fmt.Errorf("create vm: %w", err)
	}
	if resp.StatusCode() != 204 {
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "create_vm", nil, resp.StatusCode(), string(resp.Body))
		return 0, nil, fmt.Errorf("create vm failed with status %d: %s", resp.StatusCode(), string(resp.Body))
	}
	if serialSocketPath != "" {
		serialLog, err := startSerialSocketLogger(ctx, serialSocketPath, serialLogPath)
		if err != nil {
			return 0, nil, fmt.Errorf("start serial logger: %w", err)
		}
		hv.serialLog = serialLog
	}

	// 4. Boot the VM via HTTP API
	bootResp, err := hv.client.BootVMWithResponse(ctx)
	if err != nil {
		hv.serialLog.Close()
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "boot_vm", err, 0, "")
		return 0, nil, fmt.Errorf("boot vm: %w", err)
	}
	if bootResp.StatusCode() != 204 {
		hv.serialLog.Close()
		logStartVMFailureDiagnostics(ctx, log, socketPath, pid, "boot_vm", nil, bootResp.StatusCode(), string(bootResp.Body))
		return 0, nil, fmt.Errorf("boot vm failed with status %d: %s", bootResp.StatusCode(), string(bootResp.Body))
	}

	// Success - release cleanup to prevent killing the process
	cu.Release()
	return pid, hv, nil
}

// RestoreVM starts Cloud Hypervisor and restores VM state from a snapshot.
// The VM is in paused state after restore; caller should call Resume() to continue execution.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)
	startTime := time.Now()

	// Validate version
	chVersion := vmm.CHVersion(version)
	if !vmm.IsVersionSupported(chVersion) {
		return 0, nil, fmt.Errorf("unsupported cloud-hypervisor version: %s", version)
	}

	// 1. Start the Cloud Hypervisor process
	processStartTime := time.Now()
	processCtx, processSpan := hypervisor.StartProcessSpan(ctx, hypervisor.TypeCloudHypervisor)
	pid, err := vmm.StartProcess(processCtx, p, chVersion, socketPath)
	hypervisor.FinishTraceSpan(processSpan, err)
	if err != nil {
		return 0, nil, fmt.Errorf("start process: %w", err)
	}
	log.DebugContext(ctx, "CH process started", "pid", pid, "duration_ms", time.Since(processStartTime).Milliseconds())

	// Setup cleanup to kill the process if subsequent steps fail
	cu := cleanup.Make(func() {
		syscall.Kill(pid, syscall.SIGKILL)
	})
	defer cu.Clean()

	// 2. Create the HTTP client
	hv, err := New(socketPath)
	if err != nil {
		return 0, nil, fmt.Errorf("create client: %w", err)
	}

	serialSocketPath, serialLogPath := serialLogPathsFromSnapshot(snapshotPath)
	if serialSocketPath != "" {
		if err := removeStaleSerialSocket(serialSocketPath); err != nil {
			return 0, nil, err
		}
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
	if serialSocketPath != "" {
		serialLog, err := startSerialSocketLogger(ctx, serialSocketPath, serialLogPath)
		if err != nil {
			return 0, nil, fmt.Errorf("start serial logger: %w", err)
		}
		hv.serialLog = serialLog
	}

	// Success - release cleanup to prevent killing the process
	cu.Release()
	log.DebugContext(ctx, "CH restore complete", "pid", pid, "total_duration_ms", time.Since(startTime).Milliseconds())
	return pid, hv, nil
}

func ptr[T any](v T) *T {
	return &v
}

func serialLogPathsFromVMConfig(config vmm.VmConfig) (socketPath, logPath string) {
	if config.Serial == nil || config.Serial.Socket == nil || config.Serial.Mode != vmm.ConsoleConfigModeSocket {
		return "", ""
	}
	return *config.Serial.Socket, appLogPathForSerialSocket(*config.Serial.Socket)
}

func serialLogPathsFromSnapshot(snapshotPath string) (socketPath, logPath string) {
	data, err := os.ReadFile(filepath.Join(snapshotPath, "config.json"))
	if err != nil {
		return "", ""
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return "", ""
	}

	serial, ok := config["serial"].(map[string]any)
	if !ok || serial == nil || serial["mode"] != string(vmm.ConsoleConfigModeSocket) {
		return "", ""
	}
	socketPath, _ = serial["socket"].(string)
	if socketPath == "" {
		return "", ""
	}
	return socketPath, appLogPathForSerialSocket(socketPath)
}

func removeStaleSerialSocket(socketPath string) error {
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale serial socket: %w", err)
	}
	return nil
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
