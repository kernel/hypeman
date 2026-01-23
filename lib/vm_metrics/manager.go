package vm_metrics

import (
	"context"
	"sync"

	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	"go.opentelemetry.io/otel/metric"
)

// Manager collects and exposes VM resource utilization metrics.
// It reads from /proc and TAP interfaces to gather real-time statistics.
type Manager struct {
	mu     sync.RWMutex
	source InstanceSource
	otel   *otelMetrics
}

// NewManager creates a new VM metrics manager.
func NewManager() *Manager {
	return &Manager{}
}

// SetInstanceSource sets the source for instance information.
// Must be called before collecting metrics.
func (m *Manager) SetInstanceSource(source InstanceSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.source = source
}

// InitializeOTel sets up OpenTelemetry metrics.
// If meter is nil, OTel metrics are disabled.
func (m *Manager) InitializeOTel(meter metric.Meter) error {
	if meter == nil {
		return nil
	}

	otel, err := newOTelMetrics(meter, m)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.otel = otel
	m.mu.Unlock()

	return nil
}

// GetInstanceStats collects metrics for a single instance.
// Returns nil if the instance is not running or stats cannot be collected.
func (m *Manager) GetInstanceStats(ctx context.Context, info InstanceInfo) *VMStats {
	log := logger.FromContext(ctx)

	stats := &VMStats{
		InstanceID:           info.ID,
		InstanceName:         info.Name,
		AllocatedVcpus:       info.AllocatedVcpus,
		AllocatedMemoryBytes: info.AllocatedMemoryBytes,
	}

	// Read /proc stats if we have a hypervisor PID
	if info.HypervisorPID != nil {
		pid := *info.HypervisorPID

		// Read CPU from /proc/<pid>/stat
		cpuUsec, err := ReadProcStat(pid)
		if err != nil {
			log.DebugContext(ctx, "failed to read proc stat", "instance_id", info.ID, "pid", pid, "error", err)
		} else {
			stats.CPUUsec = cpuUsec
		}

		// Read memory from /proc/<pid>/statm
		rssBytes, vmsBytes, err := ReadProcStatm(pid)
		if err != nil {
			log.DebugContext(ctx, "failed to read proc statm", "instance_id", info.ID, "pid", pid, "error", err)
		} else {
			stats.MemoryRSSBytes = rssBytes
			stats.MemoryVMSBytes = vmsBytes
		}
	}

	// Read TAP stats if we have a TAP device
	if info.TAPDevice != "" {
		rxBytes, txBytes, err := ReadTAPStats(info.TAPDevice)
		if err != nil {
			log.DebugContext(ctx, "failed to read TAP stats", "instance_id", info.ID, "tap", info.TAPDevice, "error", err)
		} else {
			// TAP stats are from host perspective, swap for VM perspective:
			// - TAP rx_bytes = host receives = VM transmits
			// - TAP tx_bytes = host transmits = VM receives
			stats.NetRxBytes = txBytes
			stats.NetTxBytes = rxBytes
		}
	}

	return stats
}

// CollectAll gathers metrics for all running VMs.
// Used by OTel metrics callback.
func (m *Manager) CollectAll(ctx context.Context) ([]VMStats, error) {
	m.mu.RLock()
	source := m.source
	m.mu.RUnlock()

	if source == nil {
		return nil, nil
	}

	instances, err := source.ListRunningInstancesForMetrics()
	if err != nil {
		return nil, err
	}

	var stats []VMStats
	for _, info := range instances {
		s := m.GetInstanceStats(ctx, info)
		if s != nil {
			stats = append(stats, *s)
		}
	}

	return stats, nil
}

// BuildInstanceInfo creates an InstanceInfo from instance metadata.
// This is a helper for the API layer to avoid duplicating TAP name logic.
func BuildInstanceInfo(id, name string, pid *int, networkEnabled bool, vcpus int, memoryBytes int64) InstanceInfo {
	info := InstanceInfo{
		ID:                   id,
		Name:                 name,
		HypervisorPID:        pid,
		AllocatedVcpus:       vcpus,
		AllocatedMemoryBytes: memoryBytes,
	}

	if networkEnabled {
		info.TAPDevice = network.GenerateTAPName(id)
	}

	return info
}
