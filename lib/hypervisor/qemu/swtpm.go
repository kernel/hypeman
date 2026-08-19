package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

func startSWTPM(config *hypervisor.TPMConfig, instanceDir string) (*startedProcess, error) {
	if config == nil {
		return nil, nil
	}

	binary, err := exec.LookPath("swtpm")
	if err != nil {
		return nil, fmt.Errorf("find swtpm: %w", err)
	}
	if err := os.MkdirAll(config.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("create swtpm state directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(config.SocketPath), 0755); err != nil {
		return nil, fmt.Errorf("create swtpm socket directory: %w", err)
	}
	if err := os.Remove(config.SocketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale swtpm socket: %w", err)
	}

	logsDir := filepath.Join(instanceDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, fmt.Errorf("create swtpm logs directory: %w", err)
	}
	logPath := filepath.Join(logsDir, "swtpm.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("create swtpm log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(binary,
		"socket",
		"--tpm2",
		"--tpmstate", "dir="+config.StateDir,
		"--ctrl", "type=unixio,path="+config.SocketPath,
		"--terminate",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	proc, err := startManagedProcess(cmd, config.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("start swtpm: %w", err)
	}
	if err := waitForSocketFileOrExit(config.SocketPath, socketWaitTimeout, proc); err != nil {
		proc.cleanup()
		if logData, readErr := os.ReadFile(logPath); readErr == nil && len(logData) > 0 {
			const maxLogBytes = 32 << 10
			if len(logData) > maxLogBytes {
				logData = logData[len(logData)-maxLogBytes:]
			}
			return nil, fmt.Errorf("wait for swtpm: %w; swtpm.log: %s", err, logData)
		}
		return nil, fmt.Errorf("wait for swtpm: %w", err)
	}
	return proc, nil
}

// A connect probe would make swtpm treat the probe as its control client; with
// --terminate, closing that probe would stop swtpm before QEMU connects.
func waitForSocketFileOrExit(socketPath string, timeout time.Duration, proc *startedProcess) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(socketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if waitErr, exited := proc.checkExited(); exited {
			return fmt.Errorf("swtpm exited early: %w", waitErr)
		}
		time.Sleep(socketPollInterval)
	}
	return fmt.Errorf("timeout waiting for socket")
}
