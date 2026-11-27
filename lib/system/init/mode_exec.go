package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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

	log.Info("hypeman-init:setup", "switching root")
	if err := switchRoot(newroot); err != nil {
		log.Error("hypeman-init:setup", "switch root failed", err)
		dropToShell()
	}

	if err := installEgressProxyCA(log, cfg); err != nil {
		log.Error("hypeman-init:egress-proxy", "egress proxy CA certificate setup failed", err)
		log.Info("hypeman-init:entrypoint", formatExitSentinel(78, fmt.Sprintf("egress proxy CA certificate setup failed: %v", err)))
		syscall.Sync()
		syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	}

	// Set up environment
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", "/root")

	// Start guest-agent in background (skip if guest-agent was not copied)
	// Pass environment variables so they're available via hypeman exec
	knownChildren := newKnownChildPIDs()
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
		agentPID := agentCmd.Process.Pid
		knownChildren.add(agentPID)
		go func(cmd *exec.Cmd, pid int) {
			err := cmd.Wait()
			knownChildren.remove(pid)
			agentExited <- err
		}(agentCmd, agentPID)

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

	log.Info("hypeman-init:entrypoint", fmt.Sprintf("workdir=%s entrypoint=%v cmd=%v", workdir, cfg.Entrypoint, cfg.Cmd))
	log.Info("hypeman-init:entrypoint", "launching entrypoint")

	// Exec the entrypoint directly, like Docker's exec form, so images without
	// /bin/sh (scratch, distroless) can run. Shell-form entrypoints are already
	// stored in the image config as ["/bin/sh", "-c", ...].
	// Stdin defaults to /dev/null; users interact with the VM via guest-agent exec.
	env := buildEnv(cfg.Env)
	argv := append(append([]string{}, cfg.Entrypoint...), cfg.Cmd...)
	appCmd, err := entrypointCommand(argv, workdir, env)
	if err == nil {
		appCmd.Stdout = os.Stdout
		appCmd.Stderr = os.Stderr
		err = appCmd.Start()
	}
	if err != nil {
		log.Error("hypeman-init:entrypoint", "failed to start entrypoint", err)
		exitApp(log, startFailureExitCode(err))
		return
	}

	// Program-start sentinel used by host state derivation.
	log.Info("hypeman-init:entrypoint", formatProgramStartSentinel("exec"))
	log.Info("hypeman-init:entrypoint", fmt.Sprintf("container app started (PID %d)", appCmd.Process.Pid))

	// exec.Cmd.Wait owns these direct children; the orphan reaper must not consume their statuses.
	knownChildren.add(appCmd.Process.Pid)
	stopOrphanReaper := startOrphanReaper(knownChildren)
	defer stopOrphanReaper()

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
	err = appCmd.Wait()
	knownChildren.remove(appCmd.Process.Pid)
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

	exitApp(log, exitCode)
}

// exitApp reports the app's exit code to the host and powers off the VM.
func exitApp(log *Logger, exitCode int) {
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

// startFailureExitCode maps an entrypoint start error to the exit code a shell
// would report: 127 when the program does not exist, 126 when it cannot run.
func startFailureExitCode(err error) int {
	if errors.Is(err, errEntrypointNotFound) || errors.Is(err, fs.ErrNotExist) {
		return 127
	}
	return 126
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

var errEntrypointNotFound = errors.New("executable not found")

// entrypointCommand builds the command for argv, resolving a bare program name
// against the PATH in the app's environment rather than init's own.
func entrypointCommand(argv []string, workdir string, env []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%w: image has no entrypoint or cmd", errEntrypointNotFound)
	}
	path, err := lookPath(argv[0], envValue(env, "PATH"))
	if err != nil {
		return nil, err
	}
	return &exec.Cmd{Path: path, Args: argv, Dir: workdir, Env: env}, nil
}

// lookPath finds an executable named file in the directories of pathEnv.
// Names containing a slash are used as-is.
func lookPath(file, pathEnv string) (string, error) {
	if strings.Contains(file, "/") {
		return file, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, file)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("%w: %q in PATH %q", errEntrypointNotFound, file, pathEnv)
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
