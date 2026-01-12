package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/kernel/hypeman/lib/vmconfig"
)

// runExecMode runs the container in exec mode (default).
// This is the Docker-like behavior where:
// - The init binary remains PID 1
// - Guest-agent runs as a background process
// - The container entrypoint runs as a child process
// - After entrypoint exits, guest-agent keeps VM alive
func runExecMode(log *Logger, cfg *vmconfig.Config) {
	const newroot = "/overlay/newroot"

	// Change root to the new filesystem using chroot (consistent with systemd mode)
	log.Info("exec", "executing chroot")
	if err := syscall.Chroot(newroot); err != nil {
		log.Error("exec", "chroot failed", err)
		dropToShell()
	}

	// Change to new root directory
	if err := os.Chdir("/"); err != nil {
		log.Error("exec", "chdir / failed", err)
		dropToShell()
	}

	// Set up environment
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", "/root")

	// Start guest-agent in background
	log.Info("exec", "starting guest-agent in background")
	agentCmd := exec.Command("/opt/hypeman/guest-agent")
	agentCmd.Stdout = os.Stdout
	agentCmd.Stderr = os.Stderr
	if err := agentCmd.Start(); err != nil {
		log.Error("exec", "failed to start guest-agent", err)
	}

	// Build the entrypoint command
	workdir := cfg.Workdir
	if workdir == "" {
		workdir = "/"
	}

	// Shell-quote the entrypoint and cmd arrays for safe execution
	entrypoint := shellQuoteArgs(cfg.Entrypoint)
	cmd := shellQuoteArgs(cfg.Cmd)

	log.Info("exec", fmt.Sprintf("workdir=%s entrypoint=%v cmd=%v", workdir, cfg.Entrypoint, cfg.Cmd))

	// Construct the shell command to run
	shellCmd := fmt.Sprintf("cd %s && exec %s %s", shellQuote(workdir), entrypoint, cmd)

	log.Info("exec", "launching entrypoint")

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
		log.Error("exec", "failed to start entrypoint", err)
		dropToShell()
	}

	log.Info("exec", fmt.Sprintf("container app started (PID %d)", appCmd.Process.Pid))

	// Wait for app to exit
	err := appCmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	log.Info("exec", fmt.Sprintf("app exited with code %d", exitCode))

	// Wait for guest-agent (keeps init alive, prevents kernel panic)
	// The guest-agent runs forever, so this effectively keeps the VM alive
	// until it's explicitly terminated
	if agentCmd.Process != nil {
		agentCmd.Wait()
	}

	// Exit with the app's exit code
	syscall.Exit(exitCode)
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
