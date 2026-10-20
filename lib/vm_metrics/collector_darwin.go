//go:build darwin

package vm_metrics

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// macOS has no /proc. ps(1) reports a process's cumulative CPU time as
// [[dd-]hh:]mm:ss[.ff] and its resident and virtual sizes in KiB.
const psTimeout = 2 * time.Second

// ReadProcStat returns cumulative CPU time (user + system) in microseconds.
func ReadProcStat(pid int) (uint64, error) {
	cpu, _, _, err := psSample(pid)
	if err != nil {
		return 0, err
	}
	return uint64(cpu.Microseconds()), nil
}

// ReadProcStatm returns resident and virtual memory sizes in bytes.
func ReadProcStatm(pid int) (rssBytes, vmsBytes uint64, err error) {
	_, rssKiB, vszKiB, err := psSample(pid)
	if err != nil {
		return 0, 0, err
	}
	return rssKiB * 1024, vszKiB * 1024, nil
}

// ReadTAPStats has no macOS equivalent: guests use NAT, so there is no per-instance
// host interface to count. Callers keep network counters at zero.
func ReadTAPStats(tapName string) (rxBytes, txBytes uint64, err error) {
	return 0, 0, fmt.Errorf("read TAP stats: no per-instance interface on macOS NAT")
}

func psSample(pid int) (cpu time.Duration, rssKiB, vszKiB uint64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "time=,rss=,vsz=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sample pid %d with ps: %w", pid, err)
	}
	return parsePSSample(string(out))
}

func parsePSSample(output string) (time.Duration, uint64, uint64, error) {
	fields := strings.Fields(output)
	if len(fields) != 3 {
		return 0, 0, 0, fmt.Errorf("unexpected ps output %q", output)
	}
	cpu, err := parsePSCPUTime(fields[0])
	if err != nil {
		return 0, 0, 0, err
	}
	rss, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse rss: %w", err)
	}
	vsz, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parse vsz: %w", err)
	}
	return cpu, rss, vsz, nil
}

func parsePSCPUTime(value string) (time.Duration, error) {
	days := 0.0
	if i := strings.IndexByte(value, '-'); i >= 0 {
		d, err := strconv.Atoi(value[:i])
		if err != nil {
			return 0, fmt.Errorf("parse cpu days: %w", err)
		}
		days = float64(d)
		value = value[i+1:]
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("parse cpu time %q", value)
	}
	seconds := 0.0
	for _, part := range parts {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0, fmt.Errorf("parse cpu time %q: %w", value, err)
		}
		seconds = seconds*60 + n
	}
	return time.Duration((days*86400 + seconds) * float64(time.Second)), nil
}

// relatedVMPIDs returns Virtualization.framework VM processes that hold storagePath
// open. The XPC process is reparented to launchd, so the shim cannot be its parent.
func relatedVMPIDs(storagePath string) []int {
	if storagePath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-t", storagePath).Output()
	if err != nil {
		return nil // no holder (VM stopped) or lsof unavailable
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		command, err := exec.CommandContext(ctx, "/bin/ps", "-o", "command=", "-p", field).Output()
		if err == nil && strings.Contains(string(command), vzVirtualMachineProcess) {
			pids = append(pids, pid)
		}
	}
	return pids
}

const vzVirtualMachineProcess = "com.apple.Virtualization.VirtualMachine"
