package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
)

const swtpmSocketWaitTimeout = 30 * time.Second

type swtpmProcessRecord struct {
	pid      int
	identity string
}

func startSWTPM(config *hypervisor.TPMConfig, instanceDir string) (*startedProcess, error) {
	if config == nil {
		return nil, nil
	}

	deadline := time.Now().Add(swtpmSocketWaitTimeout)
	logsDir := filepath.Join(instanceDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return nil, fmt.Errorf("create swtpm logs directory: %w", err)
	}
	processRecordPath := filepath.Join(logsDir, "swtpm.pid")
	if err := waitForPreviousSWTPM(config, processRecordPath, deadline); err != nil {
		return nil, err
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

	if !time.Now().Before(deadline) {
		return nil, fmt.Errorf("timeout before starting swtpm")
	}
	proc, err := startManagedProcess(cmd, config.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("start swtpm: %w", err)
	}
	if err := writeSWTPMProcessRecord(processRecordPath, proc.pid); err != nil {
		proc.cleanup()
		return nil, err
	}
	if err := waitForSocketFileOrExit(config.SocketPath, time.Until(deadline), proc); err != nil {
		proc.cleanup()
		_ = os.Remove(processRecordPath)
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

func waitForPreviousSWTPM(config *hypervisor.TPMConfig, recordPath string, deadline time.Time) error {
	record, err := readSWTPMProcessRecord(recordPath)
	if err == nil {
		identity, alive, inspectErr := swtpmProcessIdentity(record.pid)
		if inspectErr != nil {
			return fmt.Errorf("inspect previous swtpm process %d: %w", record.pid, inspectErr)
		}
		if alive && identity == record.identity {
			return waitForSWTPMExit(record, recordPath, deadline)
		}
		_ = os.Remove(recordPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read previous swtpm process: %w", err)
	}

	record, found, err := discoverSWTPMProcess(config)
	if err != nil {
		return fmt.Errorf("reconcile previous swtpm process: %w", err)
	}
	if !found {
		return nil
	}
	return waitForSWTPMExit(record, recordPath, deadline)
}

func waitForSWTPMExit(record swtpmProcessRecord, recordPath string, deadline time.Time) error {
	for {
		identity, alive, err := swtpmProcessIdentity(record.pid)
		if err != nil {
			return fmt.Errorf("inspect previous swtpm process %d: %w", record.pid, err)
		}
		if !alive || identity != record.identity {
			if !time.Now().Before(deadline) {
				return fmt.Errorf("timeout waiting for previous swtpm process %d", record.pid)
			}
			_ = os.Remove(recordPath)
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("timeout waiting for previous swtpm process %d", record.pid)
		}
		time.Sleep(socketPollInterval)
	}
}

func readSWTPMProcessRecord(path string) (swtpmProcessRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return swtpmProcessRecord{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return swtpmProcessRecord{}, fmt.Errorf("invalid process record %q", strings.TrimSpace(string(data)))
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return swtpmProcessRecord{}, fmt.Errorf("invalid process id %q", fields[0])
	}
	return swtpmProcessRecord{pid: pid, identity: fields[1]}, nil
}

func writeSWTPMProcessRecord(path string, pid int) error {
	identity, alive, err := swtpmProcessIdentity(pid)
	if err != nil {
		return fmt.Errorf("inspect new swtpm process %d: %w", pid, err)
	}
	if !alive {
		return fmt.Errorf("new swtpm process %d exited before recording", pid)
	}
	data := []byte(fmt.Sprintf("%d %s\n", pid, identity))
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write swtpm process record: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish swtpm process record: %w", err)
	}
	return nil
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
