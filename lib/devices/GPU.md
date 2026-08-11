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
  "profiles": [
    {"name": "L40S-1Q", "framebuffer_mb": 1024, "available": 59},
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

Hypeman reconciles orphaned assignments on server restart while preserving devices held open by a running VMM.

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
    "profiles": [
      {"name": "L40S-1Q", "framebuffer_mb": 1024, "available": 59}
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
