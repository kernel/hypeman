// Package resources provides host resource discovery, capacity tracking,
// and oversubscription-aware allocation management for CPU, memory, disk, and network.
package resources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernel/hypeman/lib/logger"
)

// VMUtilization holds actual resource utilization metrics for a VM.
// These are real-time values read from /proc/<pid>/stat, /proc/<pid>/statm, and TAP interfaces.
type VMUtilization struct {
	InstanceID     string
	InstanceName   string
	CPUUsec        uint64 // Total CPU time in microseconds (user + system)
	MemoryRSSBytes uint64 // Resident Set Size - actual physical memory used
	MemoryVMSBytes uint64 // Virtual Memory Size - total allocated virtual memory
	NetRxBytes     uint64 // Total network bytes received
	NetTxBytes     uint64 // Total network bytes transmitted

	// Allocated resources (for computing utilization ratios)
	AllocatedVcpus       int   // Number of allocated vCPUs
	AllocatedMemoryBytes int64 // Allocated memory in bytes
}

// UtilizationSource provides access to instance data for utilization collection.
type UtilizationSource interface {
	// ListRunningInstancesInfo returns basic info for running instances.
	ListRunningInstancesInfo(ctx context.Context) ([]InstanceUtilizationInfo, error)
}

// InstanceUtilizationInfo contains the minimal info needed to collect utilization.
type InstanceUtilizationInfo struct {
	ID            string
	Name          string
	HypervisorPID *int   // PID of the hypervisor process
	TAPDevice     string // Name of the TAP device (e.g., "hype-01234567")

	// Allocated resources (for computing utilization ratios)
	AllocatedVcpus       int   // Number of allocated vCPUs
	AllocatedMemoryBytes int64 // Allocated memory in bytes (Size + HotplugSize)
}

// CollectVMUtilization gathers utilization metrics for all running VMs.
// Uses /proc/<pid>/stat and /proc/<pid>/statm for per-process metrics (no cgroups needed).
func (m *Manager) CollectVMUtilization(ctx context.Context) ([]VMUtilization, error) {
	m.mu.RLock()
	source := m.utilizationSource
	m.mu.RUnlock()

	if source == nil {
		return nil, nil
	}

	log := logger.FromContext(ctx)

	instances, err := source.ListRunningInstancesInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("list running instances: %w", err)
	}

	var utilizations []VMUtilization
	for _, inst := range instances {
		util := VMUtilization{
			InstanceID:           inst.ID,
			InstanceName:         inst.Name,
			AllocatedVcpus:       inst.AllocatedVcpus,
			AllocatedMemoryBytes: inst.AllocatedMemoryBytes,
		}

		// Collect per-process metrics from /proc if we have a PID
		if inst.HypervisorPID != nil {
			pid := *inst.HypervisorPID

			// Read CPU time from /proc/<pid>/stat
			cpuUsec, err := ReadProcStat(pid)
			if err != nil {
				log.DebugContext(ctx, "failed to read proc stat", "instance_id", inst.ID, "pid", pid, "error", err)
			} else {
				util.CPUUsec = cpuUsec
			}

			// Read memory from /proc/<pid>/statm
			rssBytes, vmsBytes, err := ReadProcStatm(pid)
			if err != nil {
				log.DebugContext(ctx, "failed to read proc statm", "instance_id", inst.ID, "pid", pid, "error", err)
			} else {
				util.MemoryRSSBytes = rssBytes
				util.MemoryVMSBytes = vmsBytes
			}
		}

		// Collect TAP stats if we have a TAP device
		if inst.TAPDevice != "" {
			rxBytes, txBytes, err := ReadTAPStats(inst.TAPDevice)
			if err != nil {
				log.DebugContext(ctx, "failed to read TAP stats", "instance_id", inst.ID, "tap", inst.TAPDevice, "error", err)
			} else {
				util.NetRxBytes = rxBytes
				util.NetTxBytes = txBytes
			}
		}

		utilizations = append(utilizations, util)
	}

	return utilizations, nil
}

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
	// Clock ticks are typically 100 per second (sysconf(_SC_CLK_TCK))
	// 1 tick = 10000 microseconds (for 100 Hz)
	const ticksPerSecond = 100
	const usecPerTick = 1_000_000 / ticksPerSecond

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
	pageSize := uint64(os.Getpagesize())
	return rssPages * pageSize, vmsPages * pageSize, nil
}

// ReadTAPStats reads network statistics from a TAP device.
// Reads from /sys/class/net/<tap>/statistics/{rx,tx}_bytes.
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
