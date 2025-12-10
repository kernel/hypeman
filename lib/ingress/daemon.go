package ingress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

// Polling intervals for Caddy daemon lifecycle management.
const (
	// adminPollInterval is the interval for polling the admin API during startup.
	adminPollInterval = 100 * time.Millisecond

	// processExitPollInterval is the interval for polling process exit during shutdown.
	// This is faster than adminPollInterval to ensure responsive shutdown.
	processExitPollInterval = 50 * time.Millisecond
)

// CaddyDaemon manages the Caddy proxy daemon lifecycle.
// Caddy uses its admin API for configuration updates - no restart needed.
type CaddyDaemon struct {
	paths          *paths.Paths
	adminAddress   string
	adminPort      int // actual port to use (resolved from config or picked fresh)
	pid            int
	stopOnShutdown bool
}

// NewCaddyDaemon creates a new CaddyDaemon manager.
// If adminPort is 0, it will be resolved later from existing config or picked fresh.
func NewCaddyDaemon(p *paths.Paths, adminAddress string, adminPort int, stopOnShutdown bool) *CaddyDaemon {
	return &CaddyDaemon{
		paths:          p,
		adminAddress:   adminAddress,
		adminPort:      adminPort,
		stopOnShutdown: stopOnShutdown,
	}
}

// StopOnShutdown returns whether Caddy should be stopped when hypeman shuts down.
func (d *CaddyDaemon) StopOnShutdown() bool {
	return d.stopOnShutdown
}

// AdminPort returns the admin port. If it was configured as 0 (random),
// this returns the actual port after it's been resolved.
func (d *CaddyDaemon) AdminPort() int {
	return d.adminPort
}

// Start starts the Caddy daemon. If Caddy is already running (discovered via PID file
// or admin API), this is a no-op and returns the existing PID.
// Note: adminPort must be resolved (non-zero) before calling Start.
func (d *CaddyDaemon) Start(ctx context.Context) (int, error) {
	// Check if already running
	if pid, running := d.DiscoverRunning(); running {
		d.pid = pid
		return pid, nil
	}

	return d.startCaddy(ctx)
}

// pickAvailablePort finds an available TCP port by briefly binding to port 0.
func pickAvailablePort(address string) (int, error) {
	listener, err := net.Listen("tcp", address+":0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port, nil
}

// readPortFromConfig reads the admin port from the existing Caddy config file.
func (d *CaddyDaemon) readPortFromConfig() int {
	data, err := os.ReadFile(d.paths.CaddyConfig())
	if err != nil {
		return 0
	}

	var config struct {
		Admin struct {
			Listen string `json:"listen"`
		} `json:"admin"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return 0
	}

	// Parse "127.0.0.1:2019" format
	if config.Admin.Listen == "" {
		return 0
	}
	_, portStr, err := net.SplitHostPort(config.Admin.Listen)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// startCaddy starts a new Caddy process.
func (d *CaddyDaemon) startCaddy(ctx context.Context) (int, error) {
	// Get binary path (extracts if needed)
	binaryPath, err := GetCaddyBinaryPath(d.paths)
	if err != nil {
		return 0, fmt.Errorf("get caddy binary: %w", err)
	}

	// Ensure caddy directory exists
	caddyDir := d.paths.CaddyDir()
	if err := os.MkdirAll(caddyDir, 0755); err != nil {
		return 0, fmt.Errorf("create caddy directory: %w", err)
	}

	// Ensure data directory exists (for certificates)
	if err := os.MkdirAll(d.paths.CaddyDataDir(), 0755); err != nil {
		return 0, fmt.Errorf("create caddy data directory: %w", err)
	}

	// Build command arguments
	configPath := d.paths.CaddyConfig()
	args := []string{
		"run",
		"--config", configPath,
	}

	// Use Command (not CommandContext) so process survives parent context cancellation
	cmd := exec.Command(binaryPath, args...)

	// Set environment for Caddy data/config paths
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("XDG_DATA_HOME=%s", d.paths.CaddyDataDir()),
		fmt.Sprintf("XDG_CONFIG_HOME=%s", d.paths.CaddyConfigDir()),
	)

	// Daemonize: create new session to fully detach from parent
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	// Redirect stdout/stderr to log file
	logFile, err := os.OpenFile(
		d.paths.CaddyLogFile(),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return 0, fmt.Errorf("create log file: %w", err)
	}
	defer logFile.Close()

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start caddy: %w", err)
	}

	pid := cmd.Process.Pid

	// Write PID file
	pidPath := d.paths.CaddyPIDFile()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		log := logger.FromContext(ctx)
		log.WarnContext(ctx, "failed to write PID file", "error", err)
	}

	// Wait for admin API to be ready.
	// Use context.Background() instead of the parent context to ensure the startup
	// wait isn't cancelled if the parent context times out during server startup.
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := d.waitForAdmin(waitCtx); err != nil {
		// Try to kill the process if it failed to start properly
		if proc, findErr := os.FindProcess(pid); findErr == nil {
			proc.Kill()
		}
		// Clean up PID file to avoid stale file on restart
		os.Remove(d.paths.CaddyPIDFile())
		return 0, fmt.Errorf("caddy failed to start: %w", err)
	}

	d.pid = pid
	return pid, nil
}

// Stop gracefully stops the Caddy daemon.
func (d *CaddyDaemon) Stop(ctx context.Context) error {
	log := logger.FromContext(ctx)

	pid, running := d.DiscoverRunning()
	if !running {
		log.InfoContext(ctx, "Caddy not running, nothing to stop")
		return nil
	}

	// Try graceful shutdown via admin API first
	client := &http.Client{Timeout: 5 * time.Second}
	adminURL := fmt.Sprintf("http://%s:%d/stop", d.adminAddress, d.adminPort)
	resp, err := client.Post(adminURL, "", nil)
	if err != nil {
		log.InfoContext(ctx, "Caddy admin API stop request failed, will try SIGTERM", "error", err)
	} else {
		resp.Body.Close()
		log.InfoContext(ctx, "Caddy admin API stop request sent")
	}

	// Wait for process to exit after admin API stop (up to 5s)
	log.InfoContext(ctx, "waiting for Caddy process to exit", "pid", pid)
	if d.waitForProcessExit(pid, 5*time.Second) {
		os.Remove(d.paths.CaddyPIDFile())
		d.pid = 0
		log.InfoContext(ctx, "Caddy stopped via admin API")
		return nil
	}

	log.InfoContext(ctx, "Caddy still running after admin API stop, sending SIGTERM")

	// Send SIGTERM if still running
	if proc, err := os.FindProcess(pid); err == nil {
		proc.Signal(syscall.SIGTERM)
	}

	// Wait for process to exit after SIGTERM
	if d.waitForProcessExit(pid, 2*time.Second) {
		os.Remove(d.paths.CaddyPIDFile())
		d.pid = 0
		log.InfoContext(ctx, "Caddy stopped via SIGTERM")
		return nil
	}

	log.InfoContext(ctx, "Caddy still running after SIGTERM, sending SIGKILL")

	// Final resort: SIGKILL
	if proc, err := os.FindProcess(pid); err == nil {
		proc.Signal(syscall.SIGKILL)
	}

	// Brief wait after SIGKILL with timeout
	d.waitForProcessExit(pid, 1*time.Second)

	// Clean up PID file
	os.Remove(d.paths.CaddyPIDFile())
	d.pid = 0

	log.InfoContext(ctx, "Caddy stopped via SIGKILL")
	return nil
}

// waitForProcessExit polls until the process exits or timeout.
func (d *CaddyDaemon) waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !d.isProcessRunning(pid) {
			return true
		}
		time.Sleep(processExitPollInterval)
	}
	return !d.isProcessRunning(pid)
}

// ReloadConfig reloads Caddy configuration by posting to the admin API.
func (d *CaddyDaemon) ReloadConfig(config []byte) error {
	client := &http.Client{Timeout: 30 * time.Second}
	adminURL := fmt.Sprintf("http://%s:%d/load", d.adminAddress, d.adminPort)

	resp, err := client.Post(adminURL, "application/json", bytes.NewReader(config))
	if err != nil {
		return fmt.Errorf("post to admin API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)

		// Try to parse a more specific error
		if specificErr := ParseCaddyError(bodyStr); specificErr != nil {
			return specificErr
		}

		return fmt.Errorf("caddy reload failed (status %d): %s", resp.StatusCode, bodyStr)
	}

	return nil
}

// DiscoverRunning checks if Caddy is already running and returns its PID.
func (d *CaddyDaemon) DiscoverRunning() (int, bool) {
	// First, try to read PID file
	pidPath := d.paths.CaddyPIDFile()
	data, err := os.ReadFile(pidPath)
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil && d.isProcessRunning(pid) {
			if d.isAdminResponding() {
				return pid, true
			}
		}
	}

	// Try admin API directly
	if d.isAdminResponding() {
		pid := d.findCaddyPID()
		if pid > 0 {
			os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644)
			return pid, true
		}
	}

	return 0, false
}

// IsRunning returns true if Caddy is currently running.
func (d *CaddyDaemon) IsRunning() bool {
	_, running := d.DiscoverRunning()
	return running
}

// GetPID returns the PID of the running Caddy process, or 0 if not running.
func (d *CaddyDaemon) GetPID() int {
	pid, running := d.DiscoverRunning()
	if running {
		return pid
	}
	return 0
}

// AdminURL returns the admin API URL.
func (d *CaddyDaemon) AdminURL() string {
	return fmt.Sprintf("http://%s:%d", d.adminAddress, d.adminPort)
}

// waitForAdmin waits for the admin API to become responsive.
func (d *CaddyDaemon) waitForAdmin(ctx context.Context) error {
	ticker := time.NewTicker(adminPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for caddy admin API")
		case <-ticker.C:
			if d.isAdminResponding() {
				return nil
			}
		}
	}
}

// isAdminResponding checks if the admin API is responding.
func (d *CaddyDaemon) isAdminResponding() bool {
	if d.adminPort == 0 {
		return false
	}
	client := &http.Client{Timeout: 1 * time.Second}
	adminURL := fmt.Sprintf("http://%s:%d/config/", d.adminAddress, d.adminPort)
	resp, err := client.Get(adminURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	// Caddy returns 200 for /config/ when running
	return resp.StatusCode == http.StatusOK
}

// isProcessRunning checks if a process with the given PID is running.
// Returns false for zombie processes (which have exited but not been reaped).
func (d *CaddyDaemon) isProcessRunning(pid int) bool {
	// Check if process exists
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err != nil {
		return false
	}

	// Check if it's a zombie process by reading /proc/[pid]/stat
	// A zombie has state 'Z' and shouldn't be considered "running"
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	data, err := os.ReadFile(statPath)
	if err != nil {
		// Can't read stat, process might have exited
		return false
	}

	// Format: pid (comm) state ...
	// Find the state character after the closing parenthesis
	statStr := string(data)
	closeParenIdx := strings.LastIndex(statStr, ")")
	if closeParenIdx == -1 || closeParenIdx+2 >= len(statStr) {
		return false
	}
	state := statStr[closeParenIdx+2] // Skip ") " to get state char

	// Z = zombie, X = dead
	return state != 'Z' && state != 'X'
}

// findCaddyPID tries to find the Caddy process PID by scanning /proc.
// It matches both the binary name and our specific config path to avoid
// colliding with other Caddy instances or other hypeman instances on the same server.
func (d *CaddyDaemon) findCaddyPID() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}

	configPath := d.paths.CaddyConfig()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}

		cmdStr := string(cmdline)
		// Match caddy run command with our specific config path
		if strings.Contains(cmdStr, "caddy") && strings.Contains(cmdStr, "run") && strings.Contains(cmdStr, configPath) {
			return pid
		}
	}

	return 0
}
