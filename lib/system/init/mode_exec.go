package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/vmconfig"
)

const (
	guestAgentReadyFilePath = "/run/hypeman/guest-agent-ready"
	guestAgentReadyTimeout  = 10 * time.Second
	guestAgentReadyFDEnv    = "HYPEMAN_AGENT_READY_FD"
)

// runExecMode runs the container in exec mode (default).
// This is the Docker-like behavior where:
// - The init binary remains PID 1
// - Guest-agent runs as a background process
// - The container entrypoint runs as a child process
// - After entrypoint exits, init logs exit info and cleanly shuts down the VM
func runExecMode(log *Logger, cfg *vmconfig.Config) {
	const newroot = "/overlay/newroot"

	// Change root to the new filesystem using chroot (consistent with systemd mode)
	log.Info("hypeman-init:setup", "executing chroot")
	if err := syscall.Chroot(newroot); err != nil {
		log.Error("hypeman-init:setup", "chroot failed", err)
		dropToShell()
	}

	// Change to new root directory
	if err := os.Chdir("/"); err != nil {
		log.Error("hypeman-init:setup", "chdir / failed", err)
		dropToShell()
	}

	// Set up environment
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", "/root")

	// Start guest-agent in background (skip if guest-agent was not copied)
	// Pass environment variables so they're available via hypeman exec
	var agentCmd *exec.Cmd
	if cfg.SkipGuestAgent {
		log.Info("hypeman-init:setup", "skipping guest-agent (skip_guest_agent=true)")
	} else {
		// Clear stale readiness marker from previous runs.
		_ = os.Remove(guestAgentReadyFilePath)

		readyPipeReader, readyPipeWriter, err := os.Pipe()
		if err != nil {
			log.Error("hypeman-init:setup", "failed to create guest-agent readiness pipe", err)
			syscall.Sync()
			syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		}

		log.Info("hypeman-init:setup", "starting guest-agent in background")
		agentCmd = exec.Command("/opt/hypeman/guest-agent")
		agentCmd.Env = append(
			buildEnv(cfg.Env),
			"HYPEMAN_AGENT_READY_FILE="+guestAgentReadyFilePath,
			fmt.Sprintf("%s=%d", guestAgentReadyFDEnv, 3),
		)
		agentCmd.ExtraFiles = []*os.File{readyPipeWriter}
		agentCmd.Stdout = os.Stdout
		agentCmd.Stderr = os.Stderr
		if err := agentCmd.Start(); err != nil {
			_ = readyPipeReader.Close()
			_ = readyPipeWriter.Close()
			log.Error("hypeman-init:setup", "failed to start guest-agent", err)
			syscall.Sync()
			syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		}
		_ = readyPipeWriter.Close()

		agentExited := make(chan error, 1)
		go func() {
			agentExited <- agentCmd.Wait()
		}()

		// Strict startup gate: do not launch the guest program until agent is ready.
		if err := waitForGuestAgentReady(readyPipeReader, guestAgentReadyTimeout, agentExited); err != nil {
			_ = readyPipeReader.Close()
			log.Error("hypeman-init:setup", "guest-agent readiness gate failed; not launching entrypoint", err)
			syscall.Sync()
			syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
		}
		_ = readyPipeReader.Close()
	}

	// Build the entrypoint command
	workdir := cfg.Workdir
	if workdir == "" {
		workdir = "/"
	}

	// Shell-quote the entrypoint and cmd arrays for safe execution
	entrypoint := shellQuoteArgs(cfg.Entrypoint)
	cmd := shellQuoteArgs(cfg.Cmd)

	log.Info("hypeman-init:entrypoint", fmt.Sprintf("workdir=%s entrypoint=%v cmd=%v", workdir, cfg.Entrypoint, cfg.Cmd))

	// Construct the shell command to run
	shellCmd := fmt.Sprintf("cd %s && exec %s %s", shellQuote(workdir), entrypoint, cmd)

	log.Info("hypeman-init:entrypoint", "launching entrypoint")

	// Run the entrypoint without stdin (defaults to /dev/null).
	// This matches the old shell script behavior where the app ran in background with &
	// and couldn't read from stdin. Interactive shells like bash will see EOF and exit.
	// Users interact with the VM via guest-agent exec, not the entrypoint's stdin.
	appCmd := exec.Command("/bin/sh", "-c", shellCmd)
	appCmd.Stdout = os.Stdout
	appCmd.Stderr = os.Stderr

	// Set up environment for the app
	appCmd.Env = buildEnv(cfg.Env)

	if err := appCmd.Start(); err != nil {
		log.Error("hypeman-init:entrypoint", "failed to start entrypoint", err)
		dropToShell()
	}

	// Program-start sentinel used by host state derivation.
	log.Info("hypeman-init:entrypoint", formatProgramStartSentinel("exec"))
	log.Info("hypeman-init:entrypoint", fmt.Sprintf("container app started (PID %d)", appCmd.Process.Pid))

	// Set up signal forwarding: when init receives a signal (e.g. from guest-agent
	// Shutdown RPC), forward it to the entrypoint child process so it can gracefully
	// shut down. This is how Docker/containerd works -- SIGTERM to PID 1 gets
	// forwarded to the app.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGINT)
	go func() {
		for sig := range sigCh {
			if appCmd.Process != nil {
				appCmd.Process.Signal(sig)
			}
		}
	}()

	// Wait for app to exit
	err := appCmd.Wait()
	signal.Stop(sigCh)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// Go's ExitCode() returns -1 when the process was killed by a signal.
			// Check WaitStatus directly to get the signal and compute 128+signal
			// (the standard shell convention for signal-killed processes).
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				exitCode = 128 + int(ws.Signal())
			} else {
				exitCode = exitErr.ExitCode()
			}
		}
	}

	// Build human-readable exit description
	exitMsg := describeExitCode(exitCode)

	// Log the exit with appropriate level
	if exitCode == 0 {
		log.Info("hypeman-init:entrypoint", "app exited with code 0 (success)")
	} else {
		log.Error("hypeman-init:entrypoint", fmt.Sprintf("app exited with code %d (%s)", exitCode, exitMsg), nil)
	}

	// Write machine-parseable exit sentinel to serial console.
	// The host reads this lazily from the serial console log file when it
	// discovers the VM has stopped (socket gone -> Stopped state).
	log.Info("hypeman-init:entrypoint", formatExitSentinel(exitCode, exitMsg))

	// Clean shutdown: use reboot(POWER_OFF) instead of syscall.Exit to avoid
	// kernel panic ("Attempted to kill init!"). This cleanly terminates the VM
	// and causes the hypervisor process to exit on the host.
	syscall.Sync()
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

// describeExitCode returns a human-readable description of an exit code.
func describeExitCode(code int) string {
	switch {
	case code == 0:
		return "success"
	case code == 126:
		return "permission denied (command not executable)"
	case code == 127:
		return "command not found"
	case code > 128:
		sig := syscall.Signal(code - 128)
		desc := fmt.Sprintf("killed by signal %d (%s)", code-128, sig.String())
		// Check for OOM on SIGKILL
		if code == 137 { // 128 + 9 (SIGKILL)
			if checkOOMKill() {
				desc += " - OOM"
			}
		}
		return desc
	default:
		return fmt.Sprintf("exit code %d", code)
	}
}

// formatExitSentinel returns a machine-parseable sentinel line for the host to parse.
// Format: HYPEMAN-EXIT code=<N> message="<description>"
func formatExitSentinel(code int, message string) string {
	return fmt.Sprintf("HYPEMAN-EXIT code=%d message=%q", code, message)
}

func formatProgramStartSentinel(mode string) string {
	return fmt.Sprintf("HYPEMAN-PROGRAM-START ts=%s mode=%s", time.Now().UTC().Format(time.RFC3339Nano), mode)
}

func waitForGuestAgentReady(readyReader *os.File, timeout time.Duration, agentExited <-chan error) error {
	readyErr := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := readyReader.Read(b[:])
		readyErr <- err
	}()

	agentExitCh := agentExited
	agentExitObserved := false
	var agentExitErr error
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case err := <-readyErr:
			if err != nil {
				if agentExitObserved {
					if agentExitErr == nil {
						return fmt.Errorf("guest-agent exited before readiness signal")
					}
					return fmt.Errorf("guest-agent exited before readiness signal: %w", agentExitErr)
				}
				return fmt.Errorf("failed waiting for guest-agent readiness signal: %w", err)
			}
			return nil
		case err := <-agentExitCh:
			agentExitErr = err
			agentExitObserved = true
			// Keep waiting for the readiness read to complete. If the agent wrote
			// readiness and then exited, the read succeeds and startup proceeds.
			agentExitCh = nil
		case <-timer.C:
			if agentExitObserved {
				if agentExitErr == nil {
					return fmt.Errorf("guest-agent exited before readiness signal")
				}
				return fmt.Errorf("guest-agent exited before readiness signal: %w", agentExitErr)
			}
			return fmt.Errorf("timed out after %s waiting for guest-agent readiness signal", timeout)
		}
	}
}

// checkOOMKill checks /dev/kmsg for recent OOM kill messages.
// Returns true if an OOM kill was detected.
// Uses a 1s timeout to avoid hanging if /dev/kmsg blocks at end of buffer.
func checkOOMKill() bool {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()

	// Use a goroutine with timeout since /dev/kmsg can still block in some cases
	result := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if isOOMLine(scanner.Text()) {
				result <- true
				return
			}
		}
		result <- false
	}()

	select {
	case found := <-result:
		return found
	case <-time.After(1 * time.Second):
		return false
	}
}

// isOOMLine returns true if a kernel log line indicates an OOM kill event.
func isOOMLine(line string) bool {
	return strings.Contains(line, "Out of memory") ||
		strings.Contains(line, "oom-kill") ||
		strings.Contains(line, "oom_reaper")
}

// buildEnv constructs environment variables from the config.
// User-provided env vars take precedence over defaults.
func buildEnv(env map[string]string) []string {
	// Start with user's environment variables
	result := make([]string, 0, len(env)+2)
	for k, v := range env {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}

	// Add defaults only if not already set by user
	if _, ok := env["PATH"]; !ok {
		result = append(result, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if _, ok := env["HOME"]; !ok {
		result = append(result, "HOME=/root")
	}

	return result
}

// shellQuote quotes a string for safe use in shell commands.
func shellQuote(s string) string {
	// Use single quotes and escape embedded single quotes
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// shellQuoteArgs quotes each argument and joins them with spaces.
func shellQuoteArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}
