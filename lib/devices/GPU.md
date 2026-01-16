# GPU and vGPU Support

This document covers GPU passthrough and vGPU (SR-IOV) support in hypeman.

## Overview

hypeman supports two GPU modes, automatically detected based on host configuration:

| Mode | Description | Use Case |
|------|-------------|----------|
| **vGPU (SR-IOV)** | Virtual GPUs via mdev on SR-IOV VFs | Multi-tenant, shared GPU resources |
| **Passthrough** | Whole GPU VFIO passthrough | Dedicated GPU per instance |

The host's GPU mode is determined by the host driver configuration:
- If `/sys/class/mdev_bus/` contains VFs → vGPU mode
- If NVIDIA GPUs are available for VFIO → passthrough mode

## vGPU Mode (Recommended)

vGPU mode uses NVIDIA's SR-IOV technology to create Virtual Functions (VFs), each capable of hosting an mdev (mediated device) representing a vGPU.

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
curl -s http://localhost:8080/resources | jq .gpu
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
curl -X POST http://localhost:8080/instances \
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

The response includes the assigned mdev UUID:

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

### Ephemeral mdev Lifecycle

mdev devices are **ephemeral**: created on instance start, destroyed on instance delete.

```
Instance Create → Create mdev → Attach to VM → Instance Running
Instance Delete → Stop VM → Destroy mdev → VF available again
```

This ensures:
- **Security**: No VRAM data leakage between instances
- **Clean state**: Fresh vGPU for each instance
- **Automatic cleanup**: Orphaned mdevs cleaned up on server restart

## Passthrough Mode

Passthrough mode assigns entire physical GPUs to instances via VFIO.

### Checking Available GPUs

```bash
curl -s http://localhost:8080/resources | jq .gpu
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
curl -X POST http://localhost:8080/devices \
  -d '{"pci_address": "0000:82:00.0", "name": "gpu-0"}'

# Create instance with GPU
curl -X POST http://localhost:8080/instances \
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
   ls /sys/class/mdev_bus/  # Should show VFs for vGPU mode
   ```

2. Verify NVIDIA drivers are loaded on host:
   ```bash
   nvidia-smi
   ```

### Profile not available

The requested profile may require more VRAM than available. Check:
```bash
curl -s http://localhost:8080/resources | jq '.gpu.profiles'
```

### nvidia-smi fails in guest

1. Verify guest image has NVIDIA drivers installed
2. Check driver version compatibility with vGPU Manager
3. Inspect guest boot logs:
   ```bash
   curl http://localhost:8080/instances/<id>/logs?source=app
   ```

### mdev creation fails

1. Check if VFs are available:
   ```bash
   ls /sys/class/mdev_bus/
   ```

2. Verify mdev types:
   ```bash
   cat /sys/class/mdev_bus/*/mdev_supported_types/*/available_instances
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
