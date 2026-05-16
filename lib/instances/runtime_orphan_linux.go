//go:build linux

package instances

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func scanOrphanRuntimeProcesses(guestsDir string) ([]orphanRuntimeProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	bootTime, _ := linuxBootTime()
	now := time.Now()
	var out []orphanRuntimeProcess
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if name != "qemu-system-x86" && name != "firecracker" {
			continue
		}
		ppid, startTime, err := readProcStatusAndStart(pid)
		if err != nil || ppid != 1 {
			continue
		}
		cmdlineBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := string(bytes.ReplaceAll(cmdlineBytes, []byte{0}, []byte{' '}))
		instanceID := instanceIDFromRuntimeCmdline(guestsDir, cmdline)
		if instanceID == "" {
			continue
		}
		age := time.Duration(0)
		if !bootTime.IsZero() && startTime > 0 {
			age = now.Sub(bootTime.Add(startTime))
		}
		out = append(out, orphanRuntimeProcess{
			PID:        pid,
			InstanceID: instanceID,
			Age:        age,
			Command:    cmdline,
		})
	}
	return out, nil
}

func instanceIDFromRuntimeCmdline(guestsDir, cmdline string) string {
	prefix := filepath.Clean(guestsDir) + string(filepath.Separator)
	idx := strings.Index(cmdline, prefix)
	if idx < 0 {
		return ""
	}
	rest := cmdline[idx+len(prefix):]
	end := strings.IndexAny(rest, string(filepath.Separator)+" ")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func readProcStatusAndStart(pid int) (ppid int, start time.Duration, err error) {
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				ppid, _ = strconv.Atoi(fields[1])
			}
			break
		}
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ppid, 0, nil
	}
	fields := strings.Fields(string(stat))
	if len(fields) > 21 {
		ticks, _ := strconv.ParseInt(fields[21], 10, 64)
		hz := int64(100)
		start = time.Duration(ticks) * time.Second / time.Duration(hz)
	}
	return ppid, start, nil
}

func linuxBootTime() (time.Time, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			break
		}
		sec, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return time.Time{}, err
		}
		return time.Unix(sec, 0), nil
	}
	return time.Time{}, fmt.Errorf("btime not found")
}

func terminateRuntimeProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			if err == syscall.ESRCH {
				return nil
			}
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
