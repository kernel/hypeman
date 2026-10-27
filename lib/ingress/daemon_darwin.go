//go:build darwin

package ingress

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// isProcessRunning checks if a process with the given PID is running.
// Returns false for zombie processes (which have exited but not been reaped).
func (d *CaddyDaemon) isProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.Signal(0)) != nil {
		return false
	}
	out, err := ps("-o", "stat=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false
	}
	state := strings.TrimSpace(string(out))
	return state != "" && !strings.HasPrefix(state, "Z")
}

// findCaddyPID finds the Caddy process running our config. macOS has no /proc, so
// it scans ps output the same way the Linux version scans cmdlines.
func (d *CaddyDaemon) findCaddyPID() int {
	out, err := ps("-axww", "-o", "pid=,command=")
	if err != nil {
		return 0
	}
	return caddyPIDFromPS(out, d.paths.CaddyConfig())
}

func caddyPIDFromPS(out []byte, configPath string) int {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		pidField, cmd, ok := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidField)
		if err != nil {
			continue
		}
		if strings.Contains(cmd, "caddy") && strings.Contains(cmd, "run") && strings.Contains(cmd, configPath) {
			return pid
		}
	}
	return 0
}

func ps(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/bin/ps", args...).Output()
}
