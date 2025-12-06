package ingress

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// CaddyDaemon manages the Caddy proxy daemon lifecycle.
// Caddy uses its admin API for configuration updates - no restart needed.
type CaddyDaemon struct {
	paths          *paths.Paths
	adminAddress   string
	adminPort      int
	pid            int
	stopOnShutdown bool
}

// NewCaddyDaemon creates a new CaddyDaemon manager.
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

// Start starts the Caddy daemon. If Caddy is already running (discovered via PID file
// or admin API), this is a no-op and returns the existing PID.
func (d *CaddyDaemon) Start(ctx context.Context) (int, error) {
	// Check if already running
	if pid, running := d.DiscoverRunning(); running {
		d.pid = pid
		return pid, nil
	}

	return d.startCaddy(ctx)
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

	// Wait for admin API to be ready
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := d.waitForAdmin(waitCtx); err != nil {
		// Try to kill the process if it failed to start properly
		if proc, err := os.FindProcess(pid); err == nil {
			proc.Kill()
		}
		return 0, fmt.Errorf("caddy failed to start: %w", err)
	}

	d.pid = pid
	return pid, nil
}

// Stop gracefully stops the Caddy daemon.
func (d *CaddyDaemon) Stop() error {
	pid, running := d.DiscoverRunning()
	if !running {
		return nil
	}

	// Try graceful shutdown via admin API first
	client := &http.Client{Timeout: 5 * time.Second}
	adminURL := fmt.Sprintf("http://%s:%d/stop", d.adminAddress, d.adminPort)
	resp, err := client.Post(adminURL, "", nil)
	if err == nil {
		resp.Body.Close()
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

	// Clean up PID file
	os.Remove(d.paths.CaddyPIDFile())
	d.pid = 0

	return nil
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
		return fmt.Errorf("caddy reload failed (status %d): %s", resp.StatusCode, string(body))
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
	ticker := time.NewTicker(100 * time.Millisecond)
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
func (d *CaddyDaemon) isProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// findCaddyPID tries to find the Caddy process PID by scanning /proc.
func (d *CaddyDaemon) findCaddyPID() int {
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

		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}

		cmdStr := string(cmdline)
		if strings.Contains(cmdStr, "caddy") && strings.Contains(cmdStr, "run") {
			return pid
		}
	}

	return 0
}
