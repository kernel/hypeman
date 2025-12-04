package ingress

import (
	"context"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

// EnvoyDaemon manages the Envoy proxy daemon lifecycle with hot restart support.
// See: https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart
// Supports multiple hypeman instances on the same host via unique baseID derived from data directory.
type EnvoyDaemon struct {
	paths          *paths.Paths
	adminAddress   string
	adminPort      int
	baseID         int // Unique identifier derived from data directory, allows multiple instances per host
	pid            int
	epoch          int  // Current restart epoch
	stopOnShutdown bool // If true, stop Envoy when hypeman shuts down
}

// NewEnvoyDaemon creates a new EnvoyDaemon manager.
// The baseID is derived from the data directory path to allow multiple hypeman instances
// on the same host without shared memory conflicts.
func NewEnvoyDaemon(p *paths.Paths, adminAddress string, adminPort int, stopOnShutdown bool) *EnvoyDaemon {
	return &EnvoyDaemon{
		paths:          p,
		adminAddress:   adminAddress,
		adminPort:      adminPort,
		baseID:         deriveBaseID(p.DataDir()),
		stopOnShutdown: stopOnShutdown,
	}
}

// deriveBaseID generates a unique base ID from the data directory path.
// This ensures multiple hypeman instances on the same host don't conflict.
func deriveBaseID(dataDir string) int {
	h := fnv.New32a()
	h.Write([]byte(dataDir))
	// Use modulo to keep in reasonable range (1-999), add 1 to avoid 0
	return int(h.Sum32()%999) + 1
}

// StopOnShutdown returns whether Envoy should be stopped when hypeman shuts down.
func (d *EnvoyDaemon) StopOnShutdown() bool {
	return d.stopOnShutdown
}

// Start starts the Envoy daemon. If Envoy is already running (discovered via PID file
// or admin API), this is a no-op and returns the existing PID.
func (d *EnvoyDaemon) Start(ctx context.Context) (int, error) {
	// Check if already running
	if pid, running := d.DiscoverRunning(); running {
		d.pid = pid
		// Load current epoch from file
		d.epoch = d.loadEpoch()
		return pid, nil
	}

	// Clean up any stale shared memory from previous crashed instances
	d.cleanupStaleSharedMemory()

	// Start fresh with epoch 0
	return d.startEnvoy(ctx, 0)
}

// startEnvoy starts a new Envoy process with the given restart epoch.
func (d *EnvoyDaemon) startEnvoy(ctx context.Context, epoch int) (int, error) {
	// Get binary path (extracts if needed)
	binaryPath, err := GetEnvoyBinaryPath(d.paths)
	if err != nil {
		return 0, fmt.Errorf("get envoy binary: %w", err)
	}

	// Ensure envoy directory exists
	envoyDir := d.paths.EnvoyDir()
	if err := os.MkdirAll(envoyDir, 0755); err != nil {
		return 0, fmt.Errorf("create envoy directory: %w", err)
	}

	// Ensure config exists (create empty bootstrap if not)
	configPath := d.paths.EnvoyConfig()
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Write minimal bootstrap config
		if err := d.writeMinimalConfig(); err != nil {
			return 0, fmt.Errorf("write minimal config: %w", err)
		}
	}

	// Build command arguments with hot restart support
	args := []string{
		"--config-path", configPath,
		"--log-path", d.paths.EnvoyLogFile(),
		"--log-level", "info",
		"--base-id", strconv.Itoa(d.baseID),
		"--restart-epoch", strconv.Itoa(epoch),
	}

	// Use Command (not CommandContext) so process survives parent context cancellation
	cmd := exec.Command(binaryPath, args...)

	// Daemonize: create new session to fully detach from parent
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // Create new session (implies new process group)
	}

	// Redirect stdout/stderr to log file
	logFile, err := os.OpenFile(
		filepath.Join(envoyDir, "envoy-stdout.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return 0, fmt.Errorf("create stdout log: %w", err)
	}
	defer logFile.Close()

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start envoy: %w", err)
	}

	pid := cmd.Process.Pid

	// Write PID file
	pidPath := d.paths.EnvoyPIDFile()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		// Non-fatal, log but continue
		log := logger.FromContext(ctx)
		log.WarnContext(ctx, "failed to write PID file", "error", err)
	}

	// Save epoch
	d.saveEpoch(epoch)

	// Wait for admin API to be ready
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.waitForAdmin(waitCtx); err != nil {
		// Try to kill the process if it failed to start properly
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Kill()
		}
		return 0, fmt.Errorf("envoy failed to start: %w", err)
	}

	d.pid = pid
	d.epoch = epoch
	return pid, nil
}

// Stop gracefully stops the Envoy daemon.
func (d *EnvoyDaemon) Stop() error {
	pid, running := d.DiscoverRunning()
	if !running {
		return nil // Already stopped
	}

	// Try graceful shutdown via admin API first
	client := &http.Client{Timeout: 5 * time.Second}
	adminURL := fmt.Sprintf("http://%s:%d/quitquitquit", d.adminAddress, d.adminPort)
	resp, err := client.Post(adminURL, "", nil)
	if err == nil {
		resp.Body.Close()
		// Wait for process to exit
		time.Sleep(2 * time.Second)
	}

	// Check if still running, send SIGTERM
	if d.isProcessRunning(pid) {
		proc, err := os.FindProcess(pid)
		if err == nil {
			proc.Signal(syscall.SIGTERM)
			time.Sleep(2 * time.Second)
		}
	}

	// Final check, send SIGKILL if needed
	if d.isProcessRunning(pid) {
		proc, err := os.FindProcess(pid)
		if err == nil {
			proc.Signal(syscall.SIGKILL)
		}
	}

	// Clean up PID file and epoch file
	os.Remove(d.paths.EnvoyPIDFile())
	os.Remove(d.epochFilePath())
	d.pid = 0
	d.epoch = 0

	// Clean up shared memory
	d.cleanupStaleSharedMemory()

	return nil
}

// ReloadConfig performs a hot restart to reload Envoy's configuration.
// This spawns a new Envoy process that coordinates with the old one to take over
// without dropping connections during the drain period.
func (d *EnvoyDaemon) ReloadConfig() error {
	if !d.IsRunning() {
		return fmt.Errorf("envoy is not running")
	}

	// Increment epoch and start new Envoy process
	// The new process will coordinate with the old one via shared memory
	newEpoch := d.epoch + 1
	_, err := d.startEnvoy(context.Background(), newEpoch)
	if err != nil {
		return fmt.Errorf("hot restart failed: %w", err)
	}

	return nil
}

// DiscoverRunning checks if Envoy is already running and returns its PID.
// Returns (pid, true) if running, (0, false) otherwise.
func (d *EnvoyDaemon) DiscoverRunning() (int, bool) {
	// First, try to read PID file
	pidPath := d.paths.EnvoyPIDFile()
	data, err := os.ReadFile(pidPath)
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil && d.isProcessRunning(pid) {
			// Verify it's actually Envoy by checking admin API
			if d.isAdminResponding() {
				return pid, true
			}
		}
	}

	// Try admin API directly (might be running without PID file)
	if d.isAdminResponding() {
		// We don't know the PID, but it's running
		// Try to find it via procfs
		pid := d.findEnvoyPID()
		if pid > 0 {
			// Update PID file
			os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644)
			return pid, true
		}
	}

	return 0, false
}

// IsRunning returns true if Envoy is currently running.
func (d *EnvoyDaemon) IsRunning() bool {
	_, running := d.DiscoverRunning()
	return running
}

// GetPID returns the PID of the running Envoy process, or 0 if not running.
func (d *EnvoyDaemon) GetPID() int {
	pid, running := d.DiscoverRunning()
	if running {
		return pid
	}
	return 0
}

// AdminURL returns the admin API URL.
func (d *EnvoyDaemon) AdminURL() string {
	return fmt.Sprintf("http://%s:%d", d.adminAddress, d.adminPort)
}

// ValidateConfig validates an Envoy configuration file without starting Envoy.
// Returns nil if valid, or an error with details if invalid.
func (d *EnvoyDaemon) ValidateConfig(configPath string) error {
	binaryPath, err := GetEnvoyBinaryPath(d.paths)
	if err != nil {
		return fmt.Errorf("get envoy binary: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binaryPath,
		"--mode", "validate",
		"--config-path", configPath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("config validation failed: %w: %s", err, string(output))
	}

	return nil
}

// epochFilePath returns the path to the epoch file.
func (d *EnvoyDaemon) epochFilePath() string {
	return filepath.Join(d.paths.EnvoyDir(), "epoch")
}

// loadEpoch loads the current epoch from file.
func (d *EnvoyDaemon) loadEpoch() int {
	data, err := os.ReadFile(d.epochFilePath())
	if err != nil {
		return 0
	}
	epoch, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return epoch
}

// saveEpoch saves the current epoch to file.
func (d *EnvoyDaemon) saveEpoch(epoch int) {
	os.WriteFile(d.epochFilePath(), []byte(strconv.Itoa(epoch)), 0644)
}

// cleanupStaleSharedMemory removes any stale shared memory regions from crashed Envoy instances.
func (d *EnvoyDaemon) cleanupStaleSharedMemory() {
	// Envoy uses /dev/shm/envoy_shared_memory_{base_id * 10 + epoch} for hot restart coordination
	// Clean up all possible epochs (0-9) for our base ID
	for epoch := 0; epoch < 10; epoch++ {
		shmPath := fmt.Sprintf("/dev/shm/envoy_shared_memory_%d", d.baseID*10+epoch)
		os.Remove(shmPath)
	}
}

// writeMinimalConfig writes a minimal Envoy bootstrap config.
// This is used when starting Envoy for the first time before any ingresses are created.
func (d *EnvoyDaemon) writeMinimalConfig() error {
	config := fmt.Sprintf(`# Envoy bootstrap configuration
# This file is auto-generated by hypeman

admin:
  address:
    socket_address:
      address: %s
      port_value: %d

static_resources:
  listeners: []
  clusters: []
`, d.adminAddress, d.adminPort)

	return os.WriteFile(d.paths.EnvoyConfig(), []byte(config), 0644)
}

// waitForAdmin waits for the admin API to become responsive.
func (d *EnvoyDaemon) waitForAdmin(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for envoy admin API")
		case <-ticker.C:
			if d.isAdminResponding() {
				return nil
			}
		}
	}
}

// isAdminResponding checks if the admin API is responding.
func (d *EnvoyDaemon) isAdminResponding() bool {
	client := &http.Client{Timeout: 1 * time.Second}
	adminURL := fmt.Sprintf("http://%s:%d/ready", d.adminAddress, d.adminPort)
	resp, err := client.Get(adminURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// isProcessRunning checks if a process with the given PID is running.
func (d *EnvoyDaemon) isProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Use signal 0 to check if process exists.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// findEnvoyPID tries to find the Envoy process PID by scanning /proc.
func (d *EnvoyDaemon) findEnvoyPID() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		// Read cmdline
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}

		// Check if it's envoy with our base-id
		cmdStr := string(cmdline)
		if strings.Contains(cmdStr, "envoy") && strings.Contains(cmdStr, fmt.Sprintf("--base-id\x00%d", d.baseID)) {
			return pid
		}
	}

	return 0
}
