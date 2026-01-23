# VM Metrics

This package provides real-time resource utilization metrics for VMs managed by Hypeman.

## Overview

VM metrics are collected from the **host's perspective** by reading:
- `/proc/<pid>/stat` - CPU time (user + system) for the hypervisor process
- `/proc/<pid>/statm` - Memory usage (RSS and VMS) for the hypervisor process  
- `/sys/class/net/<tap>/statistics/` - Network I/O from TAP interfaces

This approach works for both Cloud Hypervisor and QEMU without requiring any in-guest agents.

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `hypeman_vm_cpu_seconds_total` | Counter | Total CPU time consumed by VM |
| `hypeman_vm_allocated_vcpus` | Gauge | Number of vCPUs allocated |
| `hypeman_vm_memory_rss_bytes` | Gauge | Resident Set Size (physical memory) |
| `hypeman_vm_memory_vms_bytes` | Gauge | Virtual Memory Size |
| `hypeman_vm_allocated_memory_bytes` | Gauge | Total allocated memory |
| `hypeman_vm_network_rx_bytes_total` | Counter | Network bytes received |
| `hypeman_vm_network_tx_bytes_total` | Counter | Network bytes transmitted |
| `hypeman_vm_memory_utilization_ratio` | Gauge | RSS / allocated memory |

All metrics include `instance_id` and `instance_name` labels.

## API Endpoint

```bash
GET /instances/{id}/stats
```

Returns current utilization for a specific instance:

```json
{
  "instance_id": "abc123",
  "instance_name": "my-vm",
  "cpu_seconds": 42.5,
  "memory_rss_bytes": 536870912,
  "memory_vms_bytes": 4294967296,
  "network_rx_bytes": 1048576,
  "network_tx_bytes": 524288,
  "allocated_vcpus": 2,
  "allocated_memory_bytes": 4294967296,
  "memory_utilization_ratio": 0.125
}
```

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                          Host                                   │
│  ┌──────────────┐    ┌──────────────┐    ┌──────────────┐      │
│  │ /proc/<pid>  │    │ /proc/<pid>  │    │ /sys/class/  │      │
│  │    /stat     │    │   /statm     │    │ net/<tap>/   │      │
│  │  (CPU time)  │    │  (memory)    │    │ statistics/  │      │
│  └──────┬───────┘    └──────┬───────┘    └──────┬───────┘      │
│         │                   │                   │               │
│         └───────────────────┼───────────────────┘               │
│                             │                                   │
│                    ┌────────▼────────┐                          │
│                    │  vm_metrics     │                          │
│                    │    Manager      │                          │
│                    └────────┬────────┘                          │
│                             │                                   │
│              ┌──────────────┼──────────────┐                    │
│              │              │              │                    │
│       ┌──────▼──────┐ ┌─────▼─────┐ ┌─────▼─────┐              │
│       │  OTel/OTLP  │ │  REST API │ │  Grafana  │              │
│       │  Exporter   │ │ /stats    │ │ Dashboard │              │
│       └─────────────┘ └───────────┘ └───────────┘              │
└─────────────────────────────────────────────────────────────────┘
```

## Limitations

These metrics measure the **hypervisor process**, not the guest OS:

- **CPU**: Time spent by the hypervisor process, not guest CPU utilization
- **Memory RSS**: Physical memory used by hypervisor, closely correlates with guest memory
- **Memory VMS**: Virtual address space of hypervisor process
- **Network**: Bytes through TAP interface (accurate for guest traffic)

For detailed in-guest metrics (per-process CPU, filesystem usage, etc.), 
consider running an exporter like Prometheus Node Exporter inside the guest.

## Usage

```go
// Create manager
mgr := vm_metrics.NewManager()

// Set instance source (implements InstanceSource interface)
mgr.SetInstanceSource(instanceManager)

// Initialize OTel metrics (optional)
meter := otel.GetMeterProvider().Meter("hypeman")
if err := mgr.InitializeOTel(meter); err != nil {
    return err
}

// Get stats for a specific instance
info := vm_metrics.BuildInstanceInfo(
    inst.Id, 
    inst.Name, 
    inst.HypervisorPID,
    inst.NetworkEnabled,
    inst.Vcpus,
    inst.Size + inst.HotplugSize,
)
stats := mgr.GetInstanceStats(ctx, info)
```

## Prometheus Queries

```promql
# CPU utilization rate (per vCPU)
rate(hypeman_vm_cpu_seconds_total[1m]) / hypeman_vm_allocated_vcpus

# Memory utilization percentage
hypeman_vm_memory_rss_bytes / hypeman_vm_allocated_memory_bytes * 100

# Network throughput (bytes/sec)
rate(hypeman_vm_network_rx_bytes_total[1m])
rate(hypeman_vm_network_tx_bytes_total[1m])
```
