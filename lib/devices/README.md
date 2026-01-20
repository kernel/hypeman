# Device Passthrough

This package provides GPU, vGPU, and PCI device passthrough for virtual machines.

## Overview

hypeman supports two GPU modes:

| Mode | Description | Use Case |
|------|-------------|----------|
| **vGPU (SR-IOV)** | Virtual GPUs via mdev on SR-IOV VFs | Multi-tenant, shared GPU resources |
| **Passthrough** | Whole GPU VFIO passthrough | Dedicated GPU per instance |

For GPU-specific documentation, see [GPU.md](GPU.md).

## Package Structure

```
lib/devices/
├── types.go         # Device, AvailableDevice, GPUProfile, MdevDevice types
├── errors.go        # Error definitions
├── discovery.go     # PCI device discovery from sysfs
├── vfio.go          # VFIO bind/unbind operations
├── gpu_mode.go      # GPU mode detection (vGPU vs passthrough)
├── mdev.go          # mdev lifecycle (create, destroy, list, reconcile)
├── manager.go       # Manager interface and implementation
├── manager_test.go  # Unit tests
├── gpu_e2e_test.go  # End-to-end GPU passthrough test
├── GPU.md           # GPU and vGPU documentation
└── scripts/
    └── gpu-reset.sh # GPU recovery script
```

## Quick Start

### vGPU Mode (Recommended for Multi-Tenant)

```bash
# Check available profiles
curl localhost:8080/resources | jq .gpu

# Create instance with vGPU
curl -X POST localhost:8080/instances \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ml-training",
    "image": "nvidia/cuda:12.4-runtime-ubuntu22.04",
    "gpu": {"profile": "L40S-1Q"}
  }'

# Inside VM: verify GPU
nvidia-smi
```

### Passthrough Mode (Dedicated GPU)

```bash
# Discover available devices
curl localhost:8080/devices/available

# Register the GPU
curl -X POST localhost:8080/devices \
  -d '{"name": "l4-gpu", "pci_address": "0000:a2:00.0"}'

# Create instance with GPU
curl -X POST localhost:8080/instances \
  -d '{"name": "ml-training", "image": "nvidia/cuda:12.0-base", "devices": ["l4-gpu"]}'

# Inside VM: verify GPU
nvidia-smi

# Delete instance (auto-unbinds from VFIO)
curl -X DELETE localhost:8080/instances/{id}
```

## Device Lifecycle

### Registration (Passthrough Mode)

Register a device for whole-GPU passthrough:

```
POST /devices
{
  "name": "l4-gpu",
  "pci_address": "0000:a2:00.0"
}
```

### Instance Creation

**vGPU Mode:**
```json
{
  "name": "gpu-workload",
  "image": "nvidia/cuda:12.4-runtime",
  "gpu": {"profile": "L40S-1Q"}
}
```

**Passthrough Mode:**
```json
{
  "name": "gpu-workload", 
  "image": "nvidia/cuda:12.4-runtime",
  "devices": ["l4-gpu"]
}
```

### Automatic Cleanup

- **vGPU**: mdev destroyed when instance is deleted
- **Passthrough**: Device unbound from VFIO when instance is deleted
- **Orphaned mdevs**: Cleaned up on server startup

## Hypervisor Integration

Both Cloud Hypervisor and QEMU receive device paths:

**VFIO passthrough:**
```
/sys/bus/pci/devices/0000:a2:00.0/
```

**mdev (vGPU):**
```
/sys/bus/mdev/devices/<uuid>/
```

## Guest Driver Requirements

**Important**: Guest images must include pre-installed NVIDIA drivers.

```dockerfile
FROM nvidia/cuda:12.4.1-runtime-ubuntu22.04
RUN apt-get update && apt-get install -y nvidia-utils-550
```

hypeman does NOT inject drivers into guests.

## Constraints and Limitations

### IOMMU Requirements

- **IOMMU must be enabled** in BIOS and kernel (`intel_iommu=on` or `amd_iommu=on`)
- All devices in an IOMMU group must be passed through together

### VFIO Module Requirements

```bash
modprobe vfio_pci
modprobe vfio_iommu_type1
```

### Single Attachment

A device (or vGPU profile slot) can only be attached to one instance at a time.

### No Hot-Plug

Devices must be specified at instance creation time.

## Troubleshooting

### GPU Reset Script

If GPU passthrough tests fail or hang:

```bash
# Reset all NVIDIA GPUs
sudo ./lib/devices/scripts/gpu-reset.sh

# Reset specific GPU
sudo ./lib/devices/scripts/gpu-reset.sh 0000:a2:00.0
```

### Common Issues

#### VFIO Bind Hangs

**Solution**: Code automatically stops `nvidia-persistenced`. For manual testing:
```bash
sudo systemctl stop nvidia-persistenced
```

#### GPU Not Restored After Test

```bash
sudo sh -c 'echo 0000:a2:00.0 > /sys/bus/pci/drivers_probe'
sudo systemctl start nvidia-persistenced
```

#### vGPU Profile Not Available

Check available slots:
```bash
curl localhost:8080/resources | jq '.gpu.profiles'
```

### Running the E2E Test

```bash
# Prerequisites
sudo modprobe vfio_pci vfio_iommu_type1

# Run test (auto-skips if no GPU)
sudo env PATH=$PATH:/sbin:/usr/sbin \
  go test -v -run TestGPUPassthrough -timeout 5m ./lib/devices/...
```

**Why root is required**: sysfs driver operations require writing to files owned by root with mode 0200.
