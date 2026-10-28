//go:build !darwin

package ingress

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

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
