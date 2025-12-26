# Resource Management

Host resource discovery, capacity tracking, and oversubscription-aware allocation management for CPU, memory, disk, and network.

## Features

- **Resource Discovery**: Automatically detects host capacity from `/proc/cpuinfo`, `/proc/meminfo`, filesystem stats, and network interface speed
- **Oversubscription**: Configurable ratios per resource type (e.g., 2x CPU oversubscription)
- **Allocation Tracking**: Tracks resource usage across all running instances
- **Bidirectional Network Rate Limiting**: Separate download/upload limits with fair sharing
- **API Endpoint**: `GET /resources` returns capacity, allocations, and per-instance breakdown

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `OVERSUB_CPU` | `1.0` | CPU oversubscription ratio |
| `OVERSUB_MEMORY` | `1.0` | Memory oversubscription ratio |
| `OVERSUB_DISK` | `1.0` | Disk oversubscription ratio |
| `OVERSUB_NETWORK` | `1.0` | Network oversubscription ratio |
| `DISK_LIMIT` | auto | Hard disk limit (e.g., `500GB`), auto-detects from filesystem |
| `NETWORK_LIMIT` | auto | Hard network limit (e.g., `10Gbps`), auto-detects from uplink speed |
| `MAX_IMAGE_STORAGE` | `0.2` | Max image storage as fraction of disk (OCI cache + rootfs) |

## Resource Types

### CPU
- Discovered from `/proc/cpuinfo` (threads × cores × sockets)
- Allocated = sum of `vcpus` from active instances

### Memory
- Discovered from `/proc/meminfo` (`MemTotal`)
- Allocated = sum of `size + hotplug_size` from active instances

### Disk
- Discovered via `statfs()` on DataDir, or configured via `DISK_LIMIT`
- Allocated = images (rootfs) + OCI cache + volumes + overlays (rootfs + volume)
- Image pulls blocked when <5GB available or image storage exceeds `MAX_IMAGE_STORAGE`

### Network

Bidirectional rate limiting with separate download and upload controls:

**Downloads (external → VM):**
- TBF (Token Bucket Filter) shaping on each TAP device egress
- Simple per-VM caps, independent of other VMs
- Smooth traffic shaping (queues packets, doesn't drop)

**Uploads (VM → external):**
- HTB (Hierarchical Token Bucket) on bridge egress
- Per-VM classes with guaranteed rate and burst ceiling
- Fair sharing when VMs contend for bandwidth
- fq_codel leaf qdisc for low latency under load

**Default limits:**
- Proportional to CPU: `(vcpus / cpu_capacity) * network_capacity`
- Symmetric download/upload by default
- Upload ceiling = 2x guaranteed rate (allows bursting when bandwidth available)

**Capacity tracking:**
- Uses max(download, upload) per instance since they share physical link

## Effective Limits

The effective allocatable capacity is:

```
effective_limit = capacity × oversub_ratio
available = effective_limit - allocated
```

For example, with 64 CPUs and `OVERSUB_CPU=2.0`, up to 128 vCPUs can be allocated across instances.

## API Response

```json
{
  "cpu": {
    "capacity": 64,
    "effective_limit": 128,
    "allocated": 48,
    "available": 80,
    "oversub_ratio": 2.0
  },
  "memory": { ... },
  "disk": { ... },
  "network": { ... },
  "disk_breakdown": {
    "images_bytes": 214748364800,
    "oci_cache_bytes": 53687091200,
    "volumes_bytes": 107374182400,
    "overlays_bytes": 227633306624
  },
  "allocations": [
    {
      "instance_id": "abc123",
      "instance_name": "my-vm",
      "cpu": 4,
      "memory_bytes": 8589934592,
      "disk_bytes": 10737418240,
      "network_download_bps": 125000000,
      "network_upload_bps": 125000000
    }
  ]
}
```
