package api

import (
	"encoding/json"
	"net/http"

	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/resources"
)

// InstanceStats represents utilization statistics for a single instance
type InstanceStats struct {
	InstanceID   string `json:"instance_id"`
	InstanceName string `json:"instance_name"`

	// CPU stats
	CPUSeconds float64 `json:"cpu_seconds"` // Total CPU time consumed

	// Memory stats (from /proc/<pid>/statm)
	MemoryRSSBytes uint64 `json:"memory_rss_bytes"` // Resident Set Size - actual physical memory
	MemoryVMSBytes uint64 `json:"memory_vms_bytes"` // Virtual Memory Size - total virtual memory

	// Network stats (from TAP interface)
	NetworkRxBytes uint64 `json:"network_rx_bytes"` // Total bytes received
	NetworkTxBytes uint64 `json:"network_tx_bytes"` // Total bytes transmitted

	// Allocated resources
	AllocatedVcpus       int   `json:"allocated_vcpus"`
	AllocatedMemoryBytes int64 `json:"allocated_memory_bytes"`

	// Utilization ratios
	MemoryUtilizationRatio *float64 `json:"memory_utilization_ratio,omitempty"` // RSS / allocated
}

// StatsHandler handles GET /instances/{id}/stats requests
func (s *ApiService) StatsHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logger.FromContext(ctx)

	// Get resolved instance from context (set by ResolveResource middleware)
	inst := mw.GetResolvedInstance[instances.Instance](ctx)
	if inst == nil {
		http.Error(w, `{"error": "instance not found"}`, http.StatusNotFound)
		return
	}

	// Build InstanceUtilizationInfo for this specific instance
	info := resources.InstanceUtilizationInfo{
		ID:   inst.Id,
		Name: inst.Name,
	}

	// Get hypervisor PID if running
	if inst.HypervisorPID != nil {
		info.HypervisorPID = inst.HypervisorPID
	}

	// Get allocated resources
	info.AllocatedVcpus = inst.Vcpus

	// Calculate allocated memory (Size + HotplugSize)
	info.AllocatedMemoryBytes = inst.Size + inst.HotplugSize

	// Get TAP device if network enabled
	if inst.NetworkEnabled {
		info.TAPDevice = generateTAPName(inst.Id)
	}

	// Collect stats directly for this instance
	stats := InstanceStats{
		InstanceID:           inst.Id,
		InstanceName:         inst.Name,
		AllocatedVcpus:       info.AllocatedVcpus,
		AllocatedMemoryBytes: info.AllocatedMemoryBytes,
	}

	// Read /proc stats if we have a PID
	if info.HypervisorPID != nil {
		pid := *info.HypervisorPID

		// Read CPU from /proc/<pid>/stat
		cpuUsec, err := resources.ReadProcStat(pid)
		if err != nil {
			log.DebugContext(ctx, "failed to read proc stat", "pid", pid, "error", err)
		} else {
			stats.CPUSeconds = float64(cpuUsec) / 1_000_000.0
		}

		// Read memory from /proc/<pid>/statm
		rssBytes, vmsBytes, err := resources.ReadProcStatm(pid)
		if err != nil {
			log.DebugContext(ctx, "failed to read proc statm", "pid", pid, "error", err)
		} else {
			stats.MemoryRSSBytes = rssBytes
			stats.MemoryVMSBytes = vmsBytes

			// Compute utilization ratio
			if info.AllocatedMemoryBytes > 0 {
				ratio := float64(rssBytes) / float64(info.AllocatedMemoryBytes)
				stats.MemoryUtilizationRatio = &ratio
			}
		}
	}

	// Read TAP stats if we have a TAP device
	if info.TAPDevice != "" {
		rxBytes, txBytes, err := resources.ReadTAPStats(info.TAPDevice)
		if err != nil {
			log.DebugContext(ctx, "failed to read TAP stats", "tap", info.TAPDevice, "error", err)
		} else {
			stats.NetworkRxBytes = rxBytes
			stats.NetworkTxBytes = txBytes
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		log.ErrorContext(ctx, "failed to encode stats response", "error", err)
	}
}

// generateTAPName generates TAP device name from instance ID (same logic as instances package)
func generateTAPName(instanceID string) string {
	// TAP name format: "hype-" + first 8 chars of instance ID
	// Max TAP name length is 15 chars (IFNAMSIZ - 1)
	prefix := "hype-"
	maxIDLen := 15 - len(prefix) // 10 chars available for ID
	idPart := instanceID
	if len(idPart) > maxIDLen {
		idPart = idPart[:maxIDLen]
	}
	return prefix + idPart
}
