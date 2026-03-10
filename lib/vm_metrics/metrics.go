package vm_metrics

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// otelMetrics holds the OpenTelemetry instruments for VM metrics.
type otelMetrics struct {
	cpuSecondsTotal          metric.Float64ObservableCounter
	allocatedVcpus           metric.Int64ObservableGauge
	memoryRSSBytes           metric.Int64ObservableGauge
	memoryVMSBytes           metric.Int64ObservableGauge
	allocatedMemoryBytes     metric.Int64ObservableGauge
	networkRxBytesTotal      metric.Int64ObservableCounter
	networkTxBytesTotal      metric.Int64ObservableCounter
	instancesObserved        metric.Int64ObservableGauge
	labelBudgetExceededTotal metric.Int64ObservableCounter
}

// newOTelMetrics creates and registers all VM utilization metrics.
func newOTelMetrics(meter metric.Meter, m *Manager) (*otelMetrics, error) {
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

	// Number of instances currently represented by per-VM metrics.
	instancesObserved, err := meter.Int64ObservableGauge(
		"hypeman_vm_metrics_instances_observed",
		metric.WithDescription("Current number of VM instances represented by per-VM labeled metrics"),
		metric.WithUnit("{instance}"),
	)
	if err != nil {
		return nil, err
	}

	labelBudgetExceededTotal, err := meter.Int64ObservableCounter(
		"hypeman_vm_metrics_label_budget_exceeded_total",
		metric.WithDescription("Total number of transitions into over-budget VM metric label cardinality"),
	)
	if err != nil {
		return nil, err
	}

	// Register the callback that will collect all utilization metrics
	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			stats, err := m.CollectAll(ctx)
			if err != nil {
				// Log error but don't fail the callback
				return nil
			}
			observed := len(stats)
			o.ObserveInt64(instancesObserved, int64(observed))
			m.observeVMLabelBudget(ctx, observed)
			o.ObserveInt64(labelBudgetExceededTotal, m.vmLabelBudgetEventCount())

			for _, s := range stats {
				attrs := metric.WithAttributes(
					attribute.String("instance_id", s.InstanceID),
					attribute.String("instance_name", s.InstanceName),
				)

				// CPU time in seconds
				o.ObserveFloat64(cpuSecondsTotal, s.CPUSeconds(), attrs)

				// Allocated resources
				o.ObserveInt64(allocatedVcpus, int64(s.AllocatedVcpus), attrs)
				o.ObserveInt64(allocatedMemoryBytes, s.AllocatedMemoryBytes, attrs)

				// Actual usage
				o.ObserveInt64(memoryRSSBytes, int64(s.MemoryRSSBytes), attrs)
				o.ObserveInt64(memoryVMSBytes, int64(s.MemoryVMSBytes), attrs)
				o.ObserveInt64(networkRxBytesTotal, int64(s.NetRxBytes), attrs)
				o.ObserveInt64(networkTxBytesTotal, int64(s.NetTxBytes), attrs)
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
		instancesObserved,
		labelBudgetExceededTotal,
	)
	if err != nil {
		return nil, err
	}

	return &otelMetrics{
		cpuSecondsTotal:          cpuSecondsTotal,
		allocatedVcpus:           allocatedVcpus,
		memoryRSSBytes:           memoryRSSBytes,
		memoryVMSBytes:           memoryVMSBytes,
		allocatedMemoryBytes:     allocatedMemoryBytes,
		networkRxBytesTotal:      networkRxBytesTotal,
		networkTxBytesTotal:      networkTxBytesTotal,
		instancesObserved:        instancesObserved,
		labelBudgetExceededTotal: labelBudgetExceededTotal,
	}, nil
}
