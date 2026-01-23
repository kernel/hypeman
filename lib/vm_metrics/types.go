// Package vm_metrics provides real-time resource utilization metrics for VMs.
// It collects CPU, memory, and network statistics from the host's perspective
// by reading /proc/<pid>/stat, /proc/<pid>/statm, and TAP interface statistics.
package vm_metrics

// VMStats holds resource utilization metrics for a single VM.
// These are point-in-time values collected from the hypervisor process.
type VMStats struct {
	InstanceID   string
	InstanceName string

	// CPU stats (from /proc/<pid>/stat)
	CPUUsec uint64 // Total CPU time in microseconds (user + system)

	// Memory stats (from /proc/<pid>/statm)
	MemoryRSSBytes uint64 // Resident Set Size - actual physical memory used
	MemoryVMSBytes uint64 // Virtual Memory Size - total allocated virtual memory

	// Network stats (from TAP interface)
	NetRxBytes uint64 // Total network bytes received
	NetTxBytes uint64 // Total network bytes transmitted

	// Allocated resources (for computing utilization ratios)
	AllocatedVcpus       int   // Number of allocated vCPUs
	AllocatedMemoryBytes int64 // Allocated memory in bytes
}

// CPUSeconds returns CPU time in seconds (for API responses).
func (s *VMStats) CPUSeconds() float64 {
	return float64(s.CPUUsec) / 1_000_000.0
}

// MemoryUtilizationRatio returns RSS / allocated memory (0.0 to 1.0+).
// Returns nil if allocated memory is 0.
func (s *VMStats) MemoryUtilizationRatio() *float64 {
	if s.AllocatedMemoryBytes <= 0 {
		return nil
	}
	ratio := float64(s.MemoryRSSBytes) / float64(s.AllocatedMemoryBytes)
	return &ratio
}

// InstanceInfo contains the minimal info needed to collect VM metrics.
// This is provided by the instances package.
type InstanceInfo struct {
	ID            string
	Name          string
	HypervisorPID *int   // PID of the hypervisor process (nil if not running)
	TAPDevice     string // Name of the TAP device (e.g., "hype-01234567")

	// Allocated resources
	AllocatedVcpus       int   // Number of allocated vCPUs
	AllocatedMemoryBytes int64 // Allocated memory in bytes (Size + HotplugSize)
}

// InstanceSource provides access to running instance information.
// Implemented by instances.Manager.
type InstanceSource interface {
	// ListRunningInstancesForMetrics returns info for all running instances.
	ListRunningInstancesForMetrics() ([]InstanceInfo, error)
}
