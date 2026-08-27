# GPU and vGPU Support

This document covers GPU passthrough and vGPU (SR-IOV) support in hypeman.

## Overview

hypeman supports two GPU modes, automatically detected based on host configuration:

| Mode | Description | Use Case |
|------|-------------|----------|
| **vGPU (SR-IOV)** | Virtual GPUs on SR-IOV VFs via mdev or vendor VFIO | Multi-tenant, shared GPU resources |
| **Passthrough** | Whole GPU VFIO passthrough | Dedicated GPU per instance |

The host's GPU mode is determined by the host driver configuration:
- If `/sys/class/mdev_bus/` contains VFs → mdev vGPU mode
- If VFs expose `/sys/bus/pci/devices/<VF>/nvidia/current_vgpu_type` → vendor VFIO vGPU mode
- If NVIDIA GPUs are available for whole-device VFIO → passthrough mode

## vGPU Mode (Recommended)

vGPU mode uses NVIDIA's SR-IOV technology to create Virtual Functions (VFs). Hosts on older kernels represent each vGPU as an mdev. Hosts using NVIDIA's vendor VFIO framework assign the profile directly to the VF through `current_vgpu_type`.

### How It Works

```
┌─────────────────────────────────────────────────────────────────────┐
│  Physical GPU (e.g., NVIDIA L40S)                                   │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │  SR-IOV Virtual Functions (VFs)                               │   │
│  │  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐              │   │
│  │  │ VF 0    │ │ VF 1    │ │ VF 2    │ │ VF 3    │ ...         │   │
│  │  │ mdev    │ │ (avail) │ │ mdev    │ │ (avail) │              │   │
│  │  │ L40S-1Q │ │         │ │ L40S-2Q │ │         │              │   │
│  │  └─────────┘ └─────────┘ └─────────┘ └─────────┘              │   │
│  └──────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

### Available Profiles

Query available profiles via the resources API:

```bash
curl -s http://localhost:4973/resources | jq .gpu
```

```json
{
  "mode": "vgpu",
  "total_slots": 64,
  "used_slots": 5,
  "allocatable_slots": 57,
  "quarantined_slots": 2,
  "profiles": [
    {"name": "L40S-1Q", "framebuffer_mb": 1024, "available": 57},
    {"name": "L40S-2Q", "framebuffer_mb": 2048, "available": 30},
    {"name": "L40S-4Q", "framebuffer_mb": 4096, "available": 16}
  ]
}
```

### Creating an Instance with vGPU

Request a vGPU by specifying the profile name:

```bash
curl -X POST http://localhost:4973/instances \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ml-training",
    "image": "nvidia/cuda:12.4-runtime-ubuntu22.04",
    "vcpus": 4,
    "size": "8GB",
    "gpu": {
      "profile": "L40S-1Q"
    }
  }'
```

On an mdev host, the response also includes the assigned mdev UUID:

```json
{
  "id": "abc123",
  "name": "ml-training",
  "gpu": {
    "profile": "L40S-1Q",
    "mdev_uuid": "aa618089-8b16-4d01-a136-25a0f3c73123"
  }
}
```

### Ephemeral vGPU Lifecycle

vGPU assignments are created on instance start and released on stop or delete. Hypeman creates/removes an mdev on mdev hosts and writes the profile ID/`0` to `current_vgpu_type` on vendor VFIO hosts.

```
Instance Create → Assign profile to VF → Attach VF to VM → Instance Running
Instance Stop/Delete → Release profile → VF available again
```

Hypeman reconciles orphaned assignments with a periodic fail-closed pass: once at startup and every minute afterward (skipped entirely on hosts without GPUs). Each pass releases assignments whose owning instance is no longer live and clears their metadata, then sweeps device-level leftovers with no live metadata claim. Devices held open by a running VMM and assignments younger than five minutes are preserved, so a release that fails during stop or delete (typically because a GPU-busy VMM's kernel-side VFIO teardown outlives the force-kill wait) is simply retried on later passes until the device is free. An ambiguous hypervisor ownership check also preserves the assignment, logs a warning, and increments `hypeman_instances_vgpu_reconcile_liveness_uncertain_total`.

### Hypervisor Support

Hypervisor selection for vGPU instances is caller policy; hypeman does not enforce it. In practice **QEMU is the only hypervisor with working vGPU support**:

- **QEMU**: fully supported and validated on both mdev and vendor VFIO hosts.
- **Cloud Hypervisor**: vendor VFIO vGPUs are known broken upstream ([cloud-hypervisor#7572](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/7572)) — the VM boots and the VF attaches, but VFIO region reads fail, the guest driver cannot initialize, and the vGPU is non-functional. Do not place vGPU instances on Cloud Hypervisor.

## Passthrough Mode

Passthrough mode assigns entire physical GPUs to instances via VFIO.

### Checking Available GPUs

```bash
curl -s http://localhost:4973/resources | jq .gpu
```

```json
{
  "mode": "passthrough",
  "total_slots": 4,
  "used_slots": 2,
  "allocatable_slots": 2,
  "quarantined_slots": 0,
  "devices": [
    {"name": "NVIDIA L40S", "available": true},
    {"name": "NVIDIA L40S", "available": false}
  ]
}
```

### Using Passthrough

For whole-GPU passthrough, use the devices API (see [README.md](README.md)):

```bash
# Register GPU
curl -X POST http://localhost:4973/devices \
  -d '{"pci_address": "0000:82:00.0", "name": "gpu-0"}'

# Create instance with GPU
curl -X POST http://localhost:4973/instances \
  -d '{"name": "ml-job", "image": "nvidia/cuda:12.4", "devices": ["gpu-0"]}'
```

## Guest Driver Requirements

**Important**: hypeman does NOT inject NVIDIA drivers into guest VMs. The guest image must include pre-installed NVIDIA drivers.

### Recommended Base Images

Use NVIDIA's official CUDA images with driver utilities:

```dockerfile
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04

# Install NVIDIA driver userspace utilities
RUN apt-get update && \
    apt-get install -y nvidia-utils-550 && \
    rm -rf /var/lib/apt/lists/*

# Your application
COPY app /app
CMD ["/app/run"]
```

### Driver Version Compatibility

The guest driver version must be compatible with:
- The host's NVIDIA vGPU Manager (for vGPU mode)
- The CUDA toolkit version your application requires

Check NVIDIA's [vGPU Documentation](https://docs.nvidia.com/grid/) for compatibility matrices.

## API Reference

### GET /resources

Returns GPU status along with other resources:

```json
{
  "cpu": { ... },
  "memory": { ... },
  "gpu": {
    "mode": "vgpu",
    "total_slots": 64,
    "used_slots": 5,
    "allocatable_slots": 57,
    "quarantined_slots": 2,
    "profiles": [
      {"name": "L40S-1Q", "framebuffer_mb": 1024, "available": 57}
    ]
  }
}
```

### POST /instances (with GPU)

```json
{
  "name": "my-instance",
  "image": "nvidia/cuda:12.4-runtime",
  "gpu": {
    "profile": "L40S-1Q"
  }
}
```

### Instance Response

```json
{
  "id": "abc123",
  "gpu": {
    "profile": "L40S-1Q",
    "mdev_uuid": "aa618089-8b16-4d01-a136-25a0f3c73123"
  }
}
```

## Upgrading NVIDIA Drivers

To upgrade the NVIDIA driver version:

1. **Choose a new version** from [NVIDIA's Linux drivers](https://www.nvidia.com/Download/index.aspx)

2. **Update kernel/linux:**
   - Edit `.github/workflows/release.yaml`
   - Change `DRIVER_VERSION=` in all locations (search for the current version)
   - The workflow file contains comments explaining what to update
   - Create a new release tag (e.g., `ch-6.12.8-kernel-2-YYYYMMDD`)

3. **Update hypeman:**
   - Edit `lib/system/versions.go`
   - Add new `KernelVersion` constant
   - Update `DefaultKernelVersion`
   - Update `NvidiaDriverVersion` map entry
   - Update `NvidiaModuleURLs` with new release URL
   - Update `NvidiaDriverLibURLs` with new release URL

4. **Test thoroughly** before deploying:
   - Run GPU passthrough E2E tests
   - Verify with real CUDA workloads (e.g., ollama inference)

## Troubleshooting

### No GPU shown in /resources

1. Check host GPU mode detection:
   ```bash
   ls /sys/class/mdev_bus/
   find /sys/bus/pci/devices -path '*/nvidia/current_vgpu_type'
   ```

2. Verify NVIDIA drivers are loaded on host:
   ```bash
   nvidia-smi
   ```

### Profile not available

The requested profile may require more VRAM than available. Check:
```bash
curl -s http://localhost:4973/resources | jq '.gpu.profiles'
```

### nvidia-smi fails in guest

1. Verify guest image has NVIDIA drivers installed
2. Check driver version compatibility with vGPU Manager
3. Inspect guest boot logs:
   ```bash
   curl http://localhost:4973/instances/<id>/logs?source=app
   ```

### Guest driver init times out on one VF (vendor VFIO)

A VF can be wedged inside the NVIDIA stack while its sysfs interface stays
healthy: assignment succeeds, the host plugin logs `display_init inst: 0
successful`, but the guest driver loops on

```
NVRM: GPU 0000:00:03.0: RmInitAdapter failed! (0x22:0x65:884)
```

(0x65 = timeout; the guest's init requests are never answered, and
`/proc/interrupts` shows the GPU's MSI-X vectors allocated but idle).

Hypeman detects this automatically: the guest agent watches the guest kernel
log (`/dev/kmsg`) for that line and records it as its GPU init state, which
the vGPU sentinel controller polls over vsock (`GetGPUInitStatus`) for every
vendor VFIO instance. A guest-reported failure records one init failure
against the VF in `<data-dir>/gpu/vf-health.json` (it survives restarts),
tallied per instance assignment; once failures accumulate from
`gpu.vf_quarantine_threshold` distinct assignments (default 2), the VF is
quarantined: excluded from placement and from advertised profile availability,
and its parent GPU becomes overflow-only — deprioritized for new placements.
Selection among a card's equivalent free VFs is randomized so a wedged VF
cannot capture every placement. The guest agent also probes driver init at boot
with `nvidia-smi -L` (when present in the image): the device open runs
RmInitAdapter, so on a wedged VF the probe itself triggers the failure line
without waiting for the workload to touch the GPU. On success the reported
state becomes a terminal OK, suppressing later failure reports. An OK state
clears failures only when that exact assignment has a recorded failure,
removing the match and older tallies. If that assignment crossed the threshold,
its later success also rescinds the quarantine. A success with no exact match
clears nothing; other quarantines require manual recovery. If the state file
exists but cannot be loaded, or the last write to it failed, placement and
advertised availability fail closed until a load or write succeeds. Recorded
tallies are re-evaluated against the configured threshold at load, so lowering
`gpu.vf_quarantine_threshold` quarantines VFs whose persisted failures already
meet the new value.

`used_slots` includes quarantined VFs still held by running instances, so it
can overlap `quarantined_slots`; use `allocatable_slots` for admission.

Below-threshold failures log at warn and increment
`hypeman_instances_vgpu_sentinel_init_failures_total`; quarantines log at
error and increment `hypeman_instances_vgpu_sentinel_quarantines_total`.
`hypeman_instances_vgpu_sentinel_checks_total` records checks by result
(`ok`, `failed`, `unknown`, `rpc_error`, or `list_error`) so hosts that lose
sentinel coverage are visible. `hypeman_instances_vgpu_quarantined_vfs`
gauges the current count. A systemic guest/host driver mismatch can still
quarantine every VF, so validate driver changes on a test host and alert on
the failure counter.

Detection requires the hypeman guest agent and a running instance: the state
lives in the agent, so a wedge whose instance stops before the next poll (5s)
is detected on the next boot that lands on the VF. The state travels only
over the vsock control channel — the serial console is shared with workload
output, so nothing a workload prints can influence the tally.

The wedge-creating kill itself leaves no host-side log: no kernel error, no
XID, no plugin crash. Detection therefore happens on the next boot that lands
on the VF, whose guest driver starts failing ~27s after spawn.

The trigger is a SIGKILL delivered to QEMU while the vGPU plugin is
still initializing the VF (roughly the first seconds after process start):
a single hard kill in that window wedges the VF near-deterministically,
while QEMU processes that exit voluntarily — error exits, QMP quit, SIGTERM —
run their VFIO teardown and never wedge, and hard kills of fully-initialized
vGPU VMs are also safe. Hypeman therefore SIGTERMs a vGPU QEMU first and only
escalates to SIGKILL after a grace period, both in start-failure cleanup and
when force-killing any vGPU instance (the instance reports Running seconds
before driver init completes, so no state reliably marks the window); a hard
kill after an ignored SIGTERM logs `VF may wedge` with the device path.
External SIGKILLs (OOM killer, manual `kill -9`) can still trigger it.

Confirm by assigning the same profile on a different VF: if that guest
initializes, the VF is wedged, not the driver stack. Remediate by cycling
SR-IOV on the parent GPU (this destroys and recreates all of its VFs, so it
requires no vGPU assignments on that GPU). The DCGM quiesce is not optional:
with `nv-hostengine`/`dcgm-exporter` holding the GPUs open, `sriov-manage -d`
fails with `Cannot obtain unbindLock` on first contact.

Any manual edit to `vf-health.json` needs an immediate hypeman restart: the
store loads only at startup, and a failure report landing first re-persists
the in-memory set over your edit. The restart does not disturb running VMs —
startup reconciliation protects live VFs.

**Draining the parent GPU.** Overflow-only is a preference, not a cordon:
under capacity pressure new placements still land on the card's healthy VFs
and refill it. To drain the card, quarantine all of its VFs by hand — add
records to the versioned `vf-health.json` (`{"version": 1, "records":
[{"vf_address": "...", "quarantined_at": "..."}]}`) and restart. Running
instances are untouched and
drain through their normal lifecycle: standby is blocked for vGPU instances,
so only a running VM pins a VF, and each stop or delete frees one for good.
Monitor by listing instances whose `gpu.device_path` sits under the parent
GPU; once none remain, run the cycle below.

```bash
# 1. Quiesce the services holding the GPU (required for the unbind lock).
systemctl stop nvidia-dcgm-exporter nvidia-dcgm

# 2. Cycle SR-IOV on the parent GPU.
/usr/lib/nvidia/sriov-manage -d <parent-gpu-pci-addr>
/usr/lib/nvidia/sriov-manage -e <parent-gpu-pci-addr>

# 3. Restart the quiesced services.
systemctl start nvidia-dcgm nvidia-dcgm-exporter
```

After the cycle, remove the card's entries from `vf-health.json`, restart,
and boot a GPU instance to verify recovery. If the cycle did not work, the
sentinel quarantines the VF again after the configured number of fresh
assignment failures.

Do not unbind/rebind the VF from the nvidia driver — it breaks the
nvidia-vgpu-vfio core-device registration (`vfio_pci_core_device not found`)
and the VF stops accepting assignments entirely until the SR-IOV cycle.

### vGPU assignment fails

Check the files for the framework detected on the host:

```bash
# mdev
cat /sys/class/mdev_bus/*/mdev_supported_types/*/available_instances

# vendor VFIO
cat /sys/bus/pci/devices/*/nvidia/creatable_vgpu_types
cat /sys/bus/pci/devices/*/nvidia/current_vgpu_type
```

## Performance Tuning

### Huge Pages

For best vGPU performance, enable huge pages on the host:

```bash
echo 1024 > /proc/sys/vm/nr_hugepages
```

### IOMMU Configuration

Ensure IOMMU is properly configured for either mode:

```bash
# Intel
intel_iommu=on iommu=pt

# AMD
amd_iommu=on iommu=pt
```

## Supported Hardware

### vGPU Mode (SR-IOV)
- NVIDIA L40, L40S
- NVIDIA A100 (with appropriate vGPU license)
- Other NVIDIA GPUs supporting SR-IOV

### Passthrough Mode
All NVIDIA datacenter GPUs supported by open-gpu-kernel-modules:
- NVIDIA H100, H200
- NVIDIA L4, L40, L40S
- NVIDIA A100, A10, A30
- NVIDIA T4
