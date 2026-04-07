# Resource Management

Host resource discovery, capacity tracking, oversubscription-aware allocation management, and burst-proof admission control for CPU, memory, disk, network, and disk I/O.

## Features

- **Resource Discovery**: Automatically detects host capacity from `/proc/cpuinfo`, `/proc/meminfo`, filesystem stats, and network interface speed
- **Oversubscription**: Configurable ratios per resource type (e.g., 2x CPU oversubscription)
- **Allocation Tracking**: Tracks resource usage across all running instances
- **Burst-Proof Admission**: In-flight create/start/restore operations reserve capacity before they become visible to normal allocation tracking
- **Bidirectional Network Rate Limiting**: Separate download/upload limits with fair sharing
- **API Endpoint**: `GET /resources` returns capacity, allocations, and per-instance breakdown

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `OVERSUB_CPU` | `4.0` | CPU oversubscription ratio |
| `OVERSUB_MEMORY` | `1.0` | Memory oversubscription ratio |
| `OVERSUB_DISK` | `1.0` | Disk oversubscription ratio |
| `OVERSUB_NETWORK` | `2.0` | Network oversubscription ratio |
| `OVERSUB_DISK_IO` | `2.0` | Disk I/O oversubscription ratio |
| `DISK_LIMIT` | auto | Hard disk limit (e.g., `500GB`), auto-detects from filesystem |
| `NETWORK_LIMIT` | auto | Hard network limit (e.g., `10Gbps`), auto-detects from uplink speed |
| `DISK_IO_LIMIT` | `1GB/s` | Hard disk I/O limit (e.g., `500MB/s`, `2GB/s`) |
| `MAX_IMAGE_STORAGE` | `0.2` | Max image storage as fraction of disk (OCI cache + rootfs) |
| `UPLOAD_BURST_MULTIPLIER` | `4` | Multiplier for upload burst ceiling (HTB ceil = rate × multiplier) |
| `DOWNLOAD_BURST_MULTIPLIER` | `4` | Multiplier for download burst bucket (TBF bucket size) |

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
- Admission uses the same logical/provisioned disk model as `GET /resources` and `hypeman resources`
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
- Upload ceiling = 4x guaranteed rate by default (configurable via `UPLOAD_BURST_MULTIPLIER`)
- Download burst bucket = 4x rate by default (configurable via `DOWNLOAD_BURST_MULTIPLIER`)

**Capacity tracking:**
- Uses max(download, upload) per instance since they share physical link

### Disk I/O

Per-VM disk I/O rate limiting with burst support:

- **Cloud Hypervisor**: Uses native `RateLimiterConfig` with token bucket
- **QEMU**: Uses drive `throttling.bps-total` options
- **Default**: Proportional to CPU: `(vcpus / cpu_capacity) * disk_io_capacity * 2.0`
- **Burst**: 4x sustained rate (allows fast cold starts)

## Example: Default Limits

**Host**: 16-core server with 10Gbps NIC (default disk I/O = 1GB/s)

**VM**: 2 vCPUs (12.5% of host)

| Resource | Calculation | Default Limit |
|----------|-------------|---------------|
| Network (down/up) | 10Gbps × 2.0 × 12.5% | 2.5 Gbps (312 MB/s) |
| Disk I/O (sustained) | 1GB/s × 2.0 × 12.5% | 250 MB/s |
| Disk I/O (burst) | 250 MB/s × 4 | 1 GB/s |

## Effective Limits

The effective allocatable capacity is:

```
effective_limit = capacity × oversub_ratio
available = effective_limit - allocated
```

For example, with 64 CPUs and `OVERSUB_CPU=2.0`, up to 128 vCPUs can be allocated across instances.

## Admission Reservations

Resource accounting uses two views of allocation:

- **Visible allocation**: what `GetStatus`, `/resources`, metrics, `hypeman resources`, and admission checks use from the current instance/image/volume state
- **Pending allocation**: temporary in-memory reservations for in-flight `create`, `start`, and `restore` operations

When Hypeman checks admission, it computes:

```text
admission_allocated = visible_allocated + pending_allocated
admission_available = effective_limit - admission_allocated
```

This prevents burst oversubscription: if one lifecycle operation reserves capacity first, the next one sees that reservation immediately even before the first VM is fully visible in normal allocation tracking.

Important behavior:

- Pending reservations affect **admission only**
- `GetStatus`, `/resources`, and metrics continue to show **visible allocation only**
- The resource-manager lock only covers reservation bookkeeping and validation
- VM boot, restore, guest-agent waits, and other slow lifecycle steps happen **outside** that lock

### Hot-Path Note

Visible allocation is optimized around one conservative cached instance-allocation view plus cached image, OCI cache, and volume totals. Those caches are warmed at startup and then updated when lifecycle or storage state changes. Admission validation reads that visible usage once per reservation and combines it with O(1) pending totals. This keeps both `hypeman resources` and the admission hot path fast without live hypervisor queries or repeated full filesystem scans on each request.

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
