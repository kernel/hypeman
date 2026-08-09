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
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"gvisor.dev/gvisor/pkg/cleanup"
)

// Timeout constants for QEMU operations
const (
	// socketWaitTimeout is how long to wait for QMP socket to become available after process start
	socketWaitTimeout = 10 * time.Second

	// migrationTimeout is how long to wait for migration to complete
	migrationTimeout = 30 * time.Second

	// socketPollInterval is how often to check if socket is ready
	socketPollInterval = 50 * time.Millisecond

	// socketDialTimeout is timeout for individual socket connection attempts
	socketDialTimeout = 100 * time.Millisecond

	// clientCreateTimeout is how long to retry QMP client creation after the
	// socket appears. Under high parallel load the socket can accept connections
	// slightly later than file creation/availability.
	clientCreateTimeout = 10 * time.Second
)

func init() {
	for _, hypervisorType := range []hypervisor.Type{hypervisor.TypeQEMU, hypervisor.TypeQEMUMicroVM} {
		hypervisor.RegisterSocketName(hypervisorType, "qemu.sock")
		hypervisor.RegisterCapabilities(hypervisorType, capabilities(hypervisorType))
		t := hypervisorType
		hypervisor.RegisterClientFactory(t, func(socketPath string) (hypervisor.Hypervisor, error) {
			return NewForType(socketPath, t)
		})
	}
}

// Starter implements hypervisor.VMStarter for a QEMU backend profile.
type Starter struct {
	hypervisorType hypervisor.Type
	machineType    hypervisor.MachineType
}

// NewStarter creates a QEMU starter using the architecture-native standard board.
func NewStarter() *Starter {
	return &Starter{hypervisorType: hypervisor.TypeQEMU}
}

// NewMicroVMStarter creates a QEMU starter using the minimal x86 microvm board.
func NewMicroVMStarter() *Starter {
	return &Starter{
		hypervisorType: hypervisor.TypeQEMUMicroVM,
		machineType:    MachineTypeMicroVM,
	}
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

// buildQMPArgs returns the base QMP socket arguments for QEMU.
func buildQMPArgs(socketPath string) []string {
	return []string{
		"-chardev", fmt.Sprintf("socket,id=qmp,path=%s,server=on,wait=off", socketPath),
		"-mon", "chardev=qmp,mode=control",
	}
}

type startedProcess struct {
	pid          int
	socketPath   string
	waitDone     chan error
	waitConsumed bool
	waitErr      error
}

func startManagedProcess(cmd *exec.Cmd, socketPath string) (*startedProcess, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &startedProcess{
		pid:        cmd.Process.Pid,
		socketPath: socketPath,
		waitDone:   make(chan error, 1),
	}
	go func() {
		err := cmd.Wait()
		_ = os.Remove(socketPath)
		proc.waitDone <- err
	}()

	return proc, nil
}

func (p *startedProcess) checkExited() (error, bool) {
	if p.waitConsumed {
		return p.waitErr, true
	}

	select {
	case err := <-p.waitDone:
		p.waitConsumed = true
		p.waitErr = err
		return err, true
	default:
		return nil, false
	}
}

func (p *startedProcess) wait() error {
	if err, exited := p.checkExited(); exited {
		return err
	}

	err := <-p.waitDone
	p.waitConsumed = true
	p.waitErr = err
	return err
}

func (p *startedProcess) cleanup() {
	if _, exited := p.checkExited(); !exited {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
		_ = p.wait()
	}
	_ = os.Remove(p.socketPath)
}

// startQEMUProcess handles the common QEMU process startup logic.
// Returns the PID, hypervisor client, and a cleanup function.
// The cleanup function must be called on error; call cleanup.Release() on success.
func (s *Starter) startQEMUProcess(ctx context.Context, p *paths.Paths, version string, socketPath string, args []string) (int, *QEMU, *cleanup.Cleanup, error) {
	log := logger.FromContext(ctx)
	processAttrs := hypervisor.TraceAttributesFromContext(ctx)
	processAttrs = append(processAttrs,
		attribute.String("operation", "start_process"),
		attribute.String("hypervisor", string(s.hypervisorType)),
	)
	processCtx, processSpan := otel.Tracer("hypeman/hypervisor/qemu").Start(ctx, "hypervisor.start_process", trace.WithAttributes(processAttrs...))
	defer processSpan.End()

	// Get binary path
	binaryPath, err := s.GetBinaryPath(p, version)
	if err != nil {
		processSpan.RecordError(err)
		processSpan.SetStatus(codes.Error, err.Error())
		return 0, nil, nil, fmt.Errorf("get binary: %w", err)
	}

	// Check if socket is already in use
	if isSocketInUse(socketPath) {
		processSpan.SetStatus(codes.Error, "socket already in use")
		return 0, nil, nil, fmt.Errorf("socket already in use, QEMU may be running at %s", socketPath)
	}

	// Remove stale socket if exists
	os.Remove(socketPath)

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
		processSpan.RecordError(err)
		processSpan.SetStatus(codes.Error, err.Error())
		return 0, nil, nil, fmt.Errorf("create logs directory: %w", err)
	}

	vmmLogFile, err := os.OpenFile(
		filepath.Join(logsDir, "vmm.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		processSpan.RecordError(err)
		processSpan.SetStatus(codes.Error, err.Error())
		return 0, nil, nil, fmt.Errorf("create vmm log: %w", err)
	}
	defer vmmLogFile.Close()

	cmd.Stdout = vmmLogFile
	cmd.Stderr = vmmLogFile

	processStartTime := time.Now()
	proc, err := startManagedProcess(cmd, socketPath)
	if err != nil {
		processSpan.RecordError(err)
		processSpan.SetStatus(codes.Error, err.Error())
		return 0, nil, nil, fmt.Errorf("start qemu: %w", err)
	}

	pid := proc.pid
	log.DebugContext(processCtx, "QEMU process started", "pid", pid, "duration_ms", time.Since(processStartTime).Milliseconds())

	// Setup cleanup to kill, reap, and remove the socket if subsequent steps fail.
	cu := cleanup.Make(proc.cleanup)

	// Wait for socket to be ready
	socketWaitStart := time.Now()
	if err := waitForSocketOrExit(socketPath, socketWaitTimeout, proc); err != nil {
		processSpan.RecordError(err)
		processSpan.SetStatus(codes.Error, err.Error())
		cu.Clean()
		wrapped := appendVMMLog(err, logsDir)
		// Diagnostic (log-only): if this was a vsock guest-CID collision, capture
		// the colliding CID and which host process is holding it.
		logVsockCIDConflict(processCtx, guestCIDFromArgs(args), wrapped.Error())
		return 0, nil, nil, wrapped
	}
	log.DebugContext(processCtx, "QMP socket ready", "duration_ms", time.Since(socketWaitStart).Milliseconds())

	// Create QMP client. The socket file may exist before QEMU can actually
	// accept monitor connections, so retry briefly on transient dial failures.
	var hv *QEMU
	clientDeadline := time.Now().Add(clientCreateTimeout)
	for {
		if waitErr, exited := proc.checkExited(); exited {
			err = fmt.Errorf("qemu exited early: %w", waitErr)
			processSpan.RecordError(err)
			processSpan.SetStatus(codes.Error, err.Error())
			cu.Clean()
			wrapped := appendVMMLog(err, logsDir)
			// Diagnostic (log-only): if this was a vsock guest-CID collision,
			// capture the colliding CID and which host process is holding it.
			logVsockCIDConflict(processCtx, guestCIDFromArgs(args), wrapped.Error())
			return 0, nil, nil, wrapped
		}

		hv, err = NewForType(socketPath, s.hypervisorType)
		if err == nil {
			break
		}
		if time.Now().After(clientDeadline) {
			processSpan.RecordError(err)
			processSpan.SetStatus(codes.Error, err.Error())
			cu.Clean()
			return 0, nil, nil, appendVMMLog(fmt.Errorf("create client: %w", err), logsDir)
		}
		time.Sleep(socketPollInterval)
	}

	processSpan.SetStatus(codes.Ok, "")
	return pid, hv, &cu, nil
}

func (s *Starter) applyMachineType(config hypervisor.VMConfig, restoring bool) (hypervisor.VMConfig, error) {
	if s.machineType != "" {
		if restoring && config.MachineType == "" {
			return hypervisor.VMConfig{}, fmt.Errorf("qemu-microvm snapshot is missing its machine type")
		}
		if config.MachineType != "" && config.MachineType != s.machineType {
			return hypervisor.VMConfig{}, fmt.Errorf("hypervisor %s cannot use QEMU machine type %q", s.hypervisorType, config.MachineType)
		}
		config.MachineType = s.machineType
	} else if config.MachineType == MachineTypeMicroVM {
		return hypervisor.VMConfig{}, fmt.Errorf("QEMU machine type microvm requires hypervisor %s", hypervisor.TypeQEMUMicroVM)
	}

	machine, err := ResolveMachineType(config.MachineType)
	if err != nil {
		return hypervisor.VMConfig{}, err
	}
	config.MachineType = machine
	return config, nil
}

// StartVM launches QEMU with the VM configuration and returns a Hypervisor client.
// QEMU receives all configuration via command-line arguments at process start.
func (s *Starter) StartVM(ctx context.Context, p *paths.Paths, version string, socketPath string, config hypervisor.VMConfig) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)
	var err error
	config, err = s.applyMachineType(config, false)
	if err != nil {
		return 0, nil, fmt.Errorf("select qemu machine type: %w", err)
	}
	if err := ValidateConfig(config); err != nil {
		return 0, nil, fmt.Errorf("validate qemu config: %w", err)
	}

	// Some distro QEMU builds may not support newer balloon sub-options.
	// Retry with progressively more conservative balloon args before failing.
	attempts := []hypervisor.VMConfig{config}
	if config.GuestMemory.EnableBalloon && (config.GuestMemory.FreePageReporting || config.GuestMemory.FreePageHinting) {
		fallback := config
		fallback.GuestMemory.FreePageReporting = false
		fallback.GuestMemory.FreePageHinting = false
		attempts = append(attempts, fallback)
	}
	if config.GuestMemory.EnableBalloon && config.GuestMemory.DeflateOnOOM {
		fallback := config
		fallback.GuestMemory.FreePageReporting = false
		fallback.GuestMemory.FreePageHinting = false
		fallback.GuestMemory.DeflateOnOOM = false
		attempts = append(attempts, fallback)
	}

	var (
		pid     int
		hv      *QEMU
		cu      *cleanup.Cleanup
		booted  hypervisor.VMConfig
		started bool
	)
	for i, attempt := range attempts {
		// Retry the same attempt once for transient monitor/socket startup races.
		for transientRetry := 0; transientRetry < 2; transientRetry++ {
			// Build command arguments: QMP socket + VM configuration
			args := buildQMPArgs(socketPath)
			args = append(args, BuildArgs(attempt)...)
			pid, hv, cu, err = s.startQEMUProcess(ctx, p, version, socketPath, args)
			if err == nil {
				booted = attempt
				started = true
				break
			}
			if transientRetry == 0 && shouldRetrySameConfig(err) {
				_ = os.Remove(socketPath)
				time.Sleep(100 * time.Millisecond)
				log.WarnContext(ctx, "qemu start hit transient startup race, retrying with same configuration", "error", err)
				continue
			}
			break
		}
		if started {
			break
		}
		if i < len(attempts)-1 && shouldRetryWithReducedBalloon(err) {
			// Ensure a failed prior attempt doesn't keep the old socket path reserved.
			_ = os.Remove(socketPath)
			time.Sleep(100 * time.Millisecond)
			log.WarnContext(ctx, "qemu start failed, retrying with reduced balloon features", "attempt", i+1, "error", err)
			continue
		}
		return 0, nil, err
	}
	defer cu.Clean()

	// Save config for potential restore later
	// QEMU migration files only contain memory state, not device config
	instanceDir := filepath.Dir(socketPath)
	if err := saveVMConfig(instanceDir, booted); err != nil {
		// Non-fatal - restore just won't work
		log.WarnContext(ctx, "failed to save VM config for restore", "error", err)
	}

	cu.Release()
	return pid, hv, nil
}

func appendVMMLog(err error, logsDir string) error {
	vmmLogPath := filepath.Join(logsDir, "vmm.log")
	if logData, readErr := os.ReadFile(vmmLogPath); readErr == nil && len(logData) > 0 {
		return fmt.Errorf("%w; vmm.log: %s", err, string(logData))
	}
	return err
}

func shouldRetrySameConfig(err error) bool {
	if err == nil {
		return false
	}
	if shouldRetryWithReducedBalloon(err) {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such file or directory") ||
		strings.Contains(msg, "timed out")
}

func shouldRetryWithReducedBalloon(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	mentionsBalloonOption := strings.Contains(msg, "virtio-balloon") ||
		strings.Contains(msg, "free-page-reporting") ||
		strings.Contains(msg, "free-page-hint") ||
		strings.Contains(msg, "deflate-on-oom")
	if !mentionsBalloonOption {
		return false
	}

	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "unknown property") ||
		strings.Contains(msg, "unknown option") ||
		strings.Contains(msg, "invalid parameter") ||
		strings.Contains(msg, "invalid option") ||
		strings.Contains(msg, "invalid value") ||
		strings.Contains(msg, "requires 'iothread'") ||
		strings.Contains(msg, "requires iothread") ||
		strings.Contains(msg, "is unexpected")
}

// RestoreVM starts QEMU and restores VM state from a snapshot.
// The VM is in paused state after restore; caller should call Resume() to continue execution.
func (s *Starter) RestoreVM(ctx context.Context, p *paths.Paths, version string, socketPath string, snapshotPath string, _ hypervisor.RestoreOptions) (int, hypervisor.Hypervisor, error) {
	log := logger.FromContext(ctx)
	startTime := time.Now()

	// Load saved VM config from snapshot directory
	// QEMU requires exact same command-line args as when snapshot was taken
	configLoadStart := time.Now()
	config, err := loadVMConfig(snapshotPath)
	if err != nil {
		return 0, nil, fmt.Errorf("load vm config from snapshot: %w", err)
	}
	config, err = s.applyMachineType(config, true)
	if err != nil {
		return 0, nil, fmt.Errorf("select qemu snapshot machine type: %w", err)
	}
	log.DebugContext(ctx, "loaded VM config from snapshot", "duration_ms", time.Since(configLoadStart).Milliseconds())
	if err := ValidateConfig(config); err != nil {
		return 0, nil, fmt.Errorf("validate qemu snapshot config: %w", err)
	}
	if machine, _ := ResolveMachineType(config.MachineType); machine == MachineTypeMicroVM {
		if err := s.validateMicroVMRestoreVersion(p, version); err != nil {
			return 0, nil, err
		}
	}

	// Build command arguments: QMP socket + VM configuration + incoming migration
	args := buildQMPArgs(socketPath)
	args = append(args, BuildArgs(config)...)

	// Add incoming migration flag to restore from snapshot
	// The "file:" protocol is deprecated in QEMU 7.2+, use "exec:cat < path" instead
	memoryFile := filepath.Join(snapshotPath, "memory")
	incomingURI := "exec:cat < " + memoryFile
	args = append(args, "-incoming", incomingURI)

	pid, hv, cu, err := s.startQEMUProcess(ctx, p, version, socketPath, args)
	if err != nil {
		return 0, nil, err
	}
	defer cu.Clean()

	// Wait for VM to be ready after loading migration data
	// QEMU transitions from "inmigrate" to "paused" when loading completes
	migrationWaitStart := time.Now()
	if err := hv.client.WaitVMReady(ctx, migrationTimeout); err != nil {
		return 0, nil, fmt.Errorf("wait for vm ready: %w", err)
	}
	log.DebugContext(ctx, "VM ready", "duration_ms", time.Since(migrationWaitStart).Milliseconds())

	if err := saveVMConfig(filepath.Dir(socketPath), config); err != nil {
		return 0, nil, fmt.Errorf("save restored vm config: %w", err)
	}

	cu.Release()
	log.DebugContext(ctx, "QEMU restore complete", "pid", pid, "total_duration_ms", time.Since(startTime).Milliseconds())
	return pid, hv, nil
}

func (s *Starter) validateMicroVMRestoreVersion(p *paths.Paths, storedVersion string) error {
	if storedVersion == "" || storedVersion == "unknown" {
		return fmt.Errorf("cannot restore microvm snapshot without the original QEMU version; cold-start the instance instead")
	}
	currentVersion, err := s.GetVersion(p)
	if err != nil {
		return fmt.Errorf("get QEMU version for microvm restore: %w", err)
	}
	return validateMicroVMRestoreVersions(storedVersion, currentVersion)
}

func validateMicroVMRestoreVersions(storedVersion, currentVersion string) error {
	if storedVersion == "" || storedVersion == "unknown" {
		return fmt.Errorf("cannot restore microvm snapshot without the original QEMU version; cold-start the instance instead")
	}
	if currentVersion != storedVersion {
		return fmt.Errorf("cannot restore microvm snapshot created with QEMU %s using QEMU %s; cold-start the instance instead", storedVersion, currentVersion)
	}
	return nil
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
	conn, err := net.DialTimeout("unix", socketPath, socketDialTimeout)
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
		conn, err := net.DialTimeout("unix", socketPath, socketDialTimeout)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(socketPollInterval)
	}
	return fmt.Errorf("timeout waiting for socket")
}

func waitForSocketOrExit(socketPath string, timeout time.Duration, proc *startedProcess) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, socketDialTimeout)
		if err == nil {
			conn.Close()
			return nil
		}

		if waitErr, exited := proc.checkExited(); exited {
			return fmt.Errorf("qemu exited early: %w", waitErr)
		}

		time.Sleep(socketPollInterval)
	}

	if waitErr, exited := proc.checkExited(); exited {
		return fmt.Errorf("qemu exited early: %w", waitErr)
	}

	return fmt.Errorf("timeout waiting for socket")
}
