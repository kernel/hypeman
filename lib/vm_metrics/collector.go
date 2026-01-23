package vm_metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// userHZ is the clock tick rate used by /proc for CPU times.
// This is USER_HZ (not kernel CONFIG_HZ), which is always 100 on Linux.
// The kernel converts internal HZ to USER_HZ when writing to /proc to
// maintain a stable userspace ABI. This has been 100 since Linux 2.4.
// See: https://man7.org/linux/man-pages/man5/proc.5.html (search for "clock ticks")
const userHZ = 100

// ReadProcStat reads CPU time from /proc/<pid>/stat.
// Returns total CPU time (user + system) in microseconds.
// Fields 14 and 15 are utime and stime in clock ticks.
func ReadProcStat(pid int) (uint64, error) {
	statPath := fmt.Sprintf("/proc/%d/stat", pid)
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, fmt.Errorf("read proc stat: %w", err)
	}

	// /proc/<pid>/stat format: pid (comm) state ppid ... field14 field15 ...
	// We need to handle comm which may contain spaces and parentheses
	content := string(data)

	// Find the last ')' to skip past the comm field
	lastParen := strings.LastIndex(content, ")")
	if lastParen == -1 {
		return 0, fmt.Errorf("invalid proc stat format: no closing paren")
	}

	// Fields after comm start at index 2 (0-indexed: state is field 2)
	// utime is field 13 (0-indexed), stime is field 14 (0-indexed)
	// After the ')', fields are space-separated starting from field 2
	fields := strings.Fields(content[lastParen+1:])
	if len(fields) < 13 {
		return 0, fmt.Errorf("invalid proc stat format: not enough fields")
	}

	// fields[11] = utime (field 14 in 1-indexed stat, but field 11 after comm)
	// fields[12] = stime (field 15 in 1-indexed stat, but field 12 after comm)
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse utime: %w", err)
	}

	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse stime: %w", err)
	}

	// Convert clock ticks to microseconds
	// /proc reports CPU times in USER_HZ (always 100 on Linux)
	const usecPerTick = 1_000_000 / userHZ

	totalUsec := (utime + stime) * usecPerTick
	return totalUsec, nil
}

// ReadProcStatm reads memory stats from /proc/<pid>/statm.
// Returns RSS (resident set size) and VMS (virtual memory size) in bytes.
// Format: size resident shared text lib data dt (all in pages)
func ReadProcStatm(pid int) (rssBytes, vmsBytes uint64, err error) {
	statmPath := fmt.Sprintf("/proc/%d/statm", pid)
	data, err := os.ReadFile(statmPath)
	if err != nil {
		return 0, 0, fmt.Errorf("read proc statm: %w", err)
	}

	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("invalid proc statm format")
	}

	// Field 0: size (total virtual memory in pages)
	// Field 1: resident (resident set size in pages)
	vmsPages, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse vms: %w", err)
	}

	rssPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse rss: %w", err)
	}

	// Convert pages to bytes using system page size (varies by architecture)
	// x86_64: typically 4KB, ARM64: can be 4KB, 16KB, or 64KB
	pageSize := uint64(os.Getpagesize())
	return rssPages * pageSize, vmsPages * pageSize, nil
}

// ReadTAPStats reads network statistics from a TAP device.
// Reads from /sys/class/net/<tap>/statistics/{rx,tx}_bytes.
// Note: Returns stats from host perspective. Caller must swap for VM perspective:
// - rxBytes = host receives = VM transmits
// - txBytes = host transmits = VM receives
func ReadTAPStats(tapName string) (rxBytes, txBytes uint64, err error) {
	basePath := filepath.Join("/sys/class/net", tapName, "statistics")

	// Read RX bytes
	rxData, err := os.ReadFile(filepath.Join(basePath, "rx_bytes"))
	if err != nil {
		return 0, 0, fmt.Errorf("read rx_bytes: %w", err)
	}
	rxBytes, err = strconv.ParseUint(strings.TrimSpace(string(rxData)), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse rx_bytes: %w", err)
	}

	// Read TX bytes
	txData, err := os.ReadFile(filepath.Join(basePath, "tx_bytes"))
	if err != nil {
		return 0, 0, fmt.Errorf("read tx_bytes: %w", err)
	}
	txBytes, err = strconv.ParseUint(strings.TrimSpace(string(txData)), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse tx_bytes: %w", err)
	}

	return rxBytes, txBytes, nil
}
