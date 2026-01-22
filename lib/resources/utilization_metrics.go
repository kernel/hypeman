package resources

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// UtilizationMetrics holds the observable instruments for VM utilization.
type UtilizationMetrics struct {
	cpuSecondsTotal        metric.Float64ObservableCounter
	allocatedVcpus         metric.Int64ObservableGauge
	memoryRSSBytes         metric.Int64ObservableGauge
	memoryVMSBytes         metric.Int64ObservableGauge
	allocatedMemoryBytes   metric.Int64ObservableGauge
	networkRxBytesTotal    metric.Int64ObservableCounter
	networkTxBytesTotal    metric.Int64ObservableCounter
	memoryUtilizationRatio metric.Float64ObservableGauge
}

// newUtilizationMetrics creates and registers all VM utilization metrics.
// These are observable gauges/counters that read from /proc and TAP interfaces.
func newUtilizationMetrics(meter metric.Meter, m *Manager) (*UtilizationMetrics, error) {
	// CPU time in seconds (converted from microseconds)
	cpuSecondsTotal, err := meter.Float64ObservableCounter(
		"hypeman_vm_cpu_seconds_total",
		metric.WithDescription("Total CPU time consumed by the VM hypervisor process in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	// Allocated vCPUs
	allocatedVcpus, err := meter.Int64ObservableGauge(
		"hypeman_vm_allocated_vcpus",
		metric.WithDescription("Number of vCPUs allocated to the VM"),
		metric.WithUnit("{vcpu}"),
	)
	if err != nil {
		return nil, err
	}

	// Memory RSS (Resident Set Size) - actual physical memory used
	memoryRSSBytes, err := meter.Int64ObservableGauge(
		"hypeman_vm_memory_rss_bytes",
		metric.WithDescription("Resident Set Size - actual physical memory used by the VM"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	// Memory VMS (Virtual Memory Size) - total allocated virtual memory
	memoryVMSBytes, err := meter.Int64ObservableGauge(
		"hypeman_vm_memory_vms_bytes",
		metric.WithDescription("Virtual Memory Size - total virtual memory allocated for the VM"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	// Allocated memory bytes
	allocatedMemoryBytes, err := meter.Int64ObservableGauge(
		"hypeman_vm_allocated_memory_bytes",
		metric.WithDescription("Total memory allocated to the VM (Size + HotplugSize)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	// Network RX bytes (from TAP - bytes received by VM)
	networkRxBytesTotal, err := meter.Int64ObservableCounter(
		"hypeman_vm_network_rx_bytes_total",
		metric.WithDescription("Total network bytes received by the VM"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	// Network TX bytes (from TAP - bytes transmitted by VM)
	networkTxBytesTotal, err := meter.Int64ObservableCounter(
		"hypeman_vm_network_tx_bytes_total",
		metric.WithDescription("Total network bytes transmitted by the VM"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	// Memory utilization ratio (RSS / allocated)
	memoryUtilizationRatio, err := meter.Float64ObservableGauge(
		"hypeman_vm_memory_utilization_ratio",
		metric.WithDescription("Memory utilization ratio (RSS / allocated memory)"),
		metric.WithUnit("1"),
	)
	if err != nil {
		return nil, err
	}

	// Register the callback that will collect all utilization metrics
	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			utilizations, err := m.CollectVMUtilization(ctx)
			if err != nil {
				// Log error but don't fail the callback
				return nil
			}

			for _, util := range utilizations {
				attrs := metric.WithAttributes(
					attribute.String("instance_id", util.InstanceID),
					attribute.String("instance_name", util.InstanceName),
				)

				// Convert CPU microseconds to seconds
				cpuSeconds := float64(util.CPUUsec) / 1_000_000.0
				o.ObserveFloat64(cpuSecondsTotal, cpuSeconds, attrs)

				// Allocated resources
				o.ObserveInt64(allocatedVcpus, int64(util.AllocatedVcpus), attrs)
				o.ObserveInt64(allocatedMemoryBytes, util.AllocatedMemoryBytes, attrs)

				// Actual usage
				o.ObserveInt64(memoryRSSBytes, int64(util.MemoryRSSBytes), attrs)
				o.ObserveInt64(memoryVMSBytes, int64(util.MemoryVMSBytes), attrs)
				o.ObserveInt64(networkRxBytesTotal, int64(util.NetRxBytes), attrs)
				o.ObserveInt64(networkTxBytesTotal, int64(util.NetTxBytes), attrs)

				// Compute utilization ratio (RSS vs allocated)
				if util.AllocatedMemoryBytes > 0 {
					memRatio := float64(util.MemoryRSSBytes) / float64(util.AllocatedMemoryBytes)
					o.ObserveFloat64(memoryUtilizationRatio, memRatio, attrs)
				}
			}

			return nil
		},
		cpuSecondsTotal,
		allocatedVcpus,
		memoryRSSBytes,
		memoryVMSBytes,
		allocatedMemoryBytes,
		networkRxBytesTotal,
		networkTxBytesTotal,
		memoryUtilizationRatio,
	)
	if err != nil {
		return nil, err
	}

	return &UtilizationMetrics{
		cpuSecondsTotal:        cpuSecondsTotal,
		allocatedVcpus:         allocatedVcpus,
		memoryRSSBytes:         memoryRSSBytes,
		memoryVMSBytes:         memoryVMSBytes,
		allocatedMemoryBytes:   allocatedMemoryBytes,
		networkRxBytesTotal:    networkRxBytesTotal,
		networkTxBytesTotal:    networkTxBytesTotal,
		memoryUtilizationRatio: memoryUtilizationRatio,
	}, nil
}
