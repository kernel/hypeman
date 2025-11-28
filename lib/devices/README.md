# Device Passthrough

This package provides GPU and PCI device passthrough for virtual machines using the Linux VFIO (Virtual Function I/O) framework.

## Overview

Device passthrough allows a VM to have direct, near-native access to physical hardware (GPUs, network cards, etc.) by bypassing the host's device drivers and giving the guest exclusive control.

```
┌─────────────────────────────────────────────────────────────┐
│  Host                                                       │
│  ┌─────────────┐     ┌─────────────────────────────────┐    │
│  │  hypeman    │     │  VFIO Driver                    │    │
│  │  (VMM)      │────▶│  /dev/vfio/<group>              │    │
│  └─────────────┘     └─────────────────────────────────┘    │
│                              │                              │
│  ┌───────────────────────────┼──────────────────────────┐   │
│  │  IOMMU (hardware)         ▼                          │   │
│  │  - Translates guest physical → host physical         │   │
│  │  - Isolates DMA (device can only access VM memory)   │   │
│  └──────────────────────────────────────────────────────┘   │
│                              │                              │
│                              ▼                              │
│                    ┌──────────────┐                         │
│                    │  GPU (PCIe)  │                         │
│                    └──────────────┘                         │
└─────────────────────────────────────────────────────────────┘
```

## Package Structure

```
lib/devices/
├── types.go        # Device, AvailableDevice, CreateDeviceRequest
├── errors.go       # Error definitions
├── discovery.go    # PCI device discovery from sysfs
├── vfio.go         # VFIO bind/unbind operations
├── manager.go      # Manager interface and implementation
├── manager_test.go # Unit tests
├── gpu_e2e_test.go # End-to-end GPU passthrough test
└── scripts/
    └── gpu-reset.sh  # GPU recovery script (see Troubleshooting)
```

## Example: Full Workflow

```bash
# 1. Discover available devices
curl localhost:8080/devices/available
# → [{"pci_address": "0000:a2:00.0", "vendor_name": "NVIDIA Corporation", ...}]

# 2. Register the GPU
curl -X POST localhost:8080/devices \
  -d '{"name": "l4-gpu", "pci_address": "0000:a2:00.0"}'

# 3. Create instance with GPU (auto-binds to VFIO)
curl -X POST localhost:8080/instances \
  -d '{"name": "ml-training", "image": "nvidia/cuda:12.0-base", "devices": ["l4-gpu"]}'

# 4. Inside VM: verify GPU
lspci | grep -i nvidia
nvidia-smi

# 5. Delete instance (auto-unbinds from VFIO)
curl -X DELETE localhost:8080/instances/{id}
# GPU returns to host control
```

## Device Lifecycle

### 1. Discovery

Discover passthrough-capable devices on the host:

```
GET /devices/available
```

Returns PCI devices that are candidates for passthrough (GPUs, 3D controllers). Each device includes its PCI address, vendor/device IDs, IOMMU group, and current driver.

### 2. Registration

Register a device with a unique name:

```
POST /devices
{
  "name": "l4-gpu",
  "pci_address": "0000:a2:00.0"
}
```

Registration does not modify the device's driver binding. The device remains usable by the host until an instance requests it.

### 3. Instance Creation (Auto-Bind)

When an instance is created with devices:

```
POST /instances
{
  "name": "gpu-workload",
  "image": "docker.io/nvidia/cuda:12.0-base",
  "devices": ["l4-gpu"]
}
```

The system automatically:
1. **Validates** the device exists and isn't attached to another instance
2. **Binds to VFIO** if not already bound (unbinds native driver like `nvidia`)
3. **Passes to cloud-hypervisor** via the `--device` flag
4. **Marks as attached** to prevent concurrent use

### 4. Instance Deletion (Auto-Unbind)

When an instance is deleted, the system automatically:
1. **Marks device as detached**
2. **Unbinds from VFIO** (triggers kernel driver probe to restore native driver)

This returns the device to host control so it can be used by other processes or a new instance.

### 5. Unregistration

```
DELETE /devices/{id}
```

Removes the device from hypeman's registry. Fails if the device is currently attached to an instance.

## Cloud Hypervisor Integration

Cloud-hypervisor receives device passthrough configuration via the `VmConfig.Devices` field:

```go
vmConfig.Devices = &[]vmm.DeviceConfig{
    {
        Path: "/sys/bus/pci/devices/0000:a2:00.0/",
    },
}
```

Cloud-hypervisor then:
1. Opens the VFIO group file (`/dev/vfio/<group>`)
2. Maps device BARs (memory regions) into guest physical address space
3. Configures interrupt routing (MSI/MSI-X) to the guest
4. The guest sees a real PCIe device and loads native drivers

### NVIDIA-Specific Options

For multi-GPU configurations, cloud-hypervisor supports GPUDirect P2P:

```go
DeviceConfig{
    Path: "/sys/bus/pci/devices/0000:a2:00.0/",
    XNvGpudirectClique: ptr(int8(0)),  // Enable P2P within clique 0
}
```

This is not currently exposed through the hypeman API but could be added for HPC workloads.

## Constraints and Limitations

### IOMMU Requirements

- **IOMMU must be enabled** in BIOS and kernel (`intel_iommu=on` or `amd_iommu=on`)
- All devices in an IOMMU group must be passed through together
- Some motherboards place many devices in the same group (ACS override may help)

### VFIO Module Requirements

The following kernel modules must be loaded:
```bash
modprobe vfio_pci
modprobe vfio_iommu_type1
```

### Driver Binding

- Binding to VFIO **unloads the native driver** (e.g., `nvidia`, `amdgpu`)
- Host processes using the device will lose access
- Some drivers (like NVIDIA) may resist unbinding if in use

### Single Attachment

A device can only be attached to one instance at a time. Attempts to attach an already-attached device will fail.

### No Hot-Plug

Devices must be specified at instance creation time. Hot-adding devices to a running VM is not currently supported (though cloud-hypervisor has this capability).

### Guest Driver Requirements

The guest must have appropriate drivers:
- **NVIDIA GPUs**: Install NVIDIA drivers in the guest image
- **AMD GPUs**: Install amdgpu/ROCm in the guest image

### Performance Considerations

- **ACS (Access Control Services)**: Required for proper isolation on some systems
- **Huge Pages**: Recommended for GPU workloads (`hugepages=on` in cloud-hypervisor)
- **CPU Pinning**: Can improve latency for GPU compute workloads

## Troubleshooting

### GPU Reset Script

If GPU passthrough tests fail or hang, the GPU may be left in a bad state (still bound to vfio-pci, or stuck without a driver). Use the provided reset script:

```bash
# Reset all NVIDIA GPUs to their native driver
sudo ./lib/devices/scripts/gpu-reset.sh

# Reset a specific GPU
sudo ./lib/devices/scripts/gpu-reset.sh 0000:a2:00.0
```

The script will:
1. Kill any stuck cloud-hypervisor processes holding the GPU
2. Unbind from vfio-pci if still bound
3. Clear `driver_override`
4. Trigger driver probe to rebind to the nvidia driver
5. Restart `nvidia-persistenced`

### Common Issues

#### VFIO Bind Hangs

**Symptom**: `BindToVFIO` hangs indefinitely.

**Cause**: The `nvidia-persistenced` service keeps `/dev/nvidia*` open, preventing driver unbind.

**Solution**: The code now automatically stops `nvidia-persistenced` before unbinding. If you're testing manually:
```bash
sudo systemctl stop nvidia-persistenced
# ... do VFIO bind/unbind ...
sudo systemctl start nvidia-persistenced
```

#### VM Exec Fails After Boot

**Symptom**: VM boots but exec commands time out.

**Cause**: Usually the container's main process exited (e.g., `alpine` image runs `/bin/sh` which exits immediately), causing init to exit and the VM to kernel panic.

**Solution**: Use an image with a long-running process (e.g., `nginx:alpine`) or ensure your container has a persistent entrypoint.

#### GPU Not Restored After Test

**Symptom**: GPU has no driver bound, `nvidia-smi` fails.

**Solution**:
```bash
# Trigger kernel driver probe
sudo sh -c 'echo 0000:a2:00.0 > /sys/bus/pci/drivers_probe'
# Restart nvidia-persistenced
sudo systemctl start nvidia-persistenced
# Verify
nvidia-smi
```

If that fails, a system **reboot** may be necessary.

#### VFIO Modules Not Loaded

**Symptom**: `ErrVFIONotAvailable` error.

**Solution**:
```bash
sudo modprobe vfio_pci vfio_iommu_type1
# Verify
ls /dev/vfio/
```

Add to `/etc/modules-load.d/vfio.conf` for persistence across reboots.

#### IOMMU Not Enabled

**Symptom**: No IOMMU groups found, passthrough fails.

**Solution**: Add kernel parameter to bootloader:
- Intel: `intel_iommu=on iommu=pt`
- AMD: `amd_iommu=on iommu=pt`

Then reboot.

### Running the E2E Test

The GPU passthrough E2E test requires:
- Root permissions (for driver bind/unbind)
- NVIDIA GPU on host
- IOMMU enabled
- `vfio_pci` and `vfio_iommu_type1` modules loaded
- `/sbin` in PATH (for `mkfs.ext4`)

```bash
# Load VFIO modules
sudo modprobe vfio_pci vfio_iommu_type1

# Run the test
sudo env PATH=$PATH:/sbin:/usr/sbin \
  go test -v -tags=integration,gpu -run TestGPUPassthrough \
  -timeout 5m ./lib/devices/...
```

The test will:
1. Discover available NVIDIA GPUs
2. Register the first GPU found
3. Create a VM with GPU passthrough
4. Verify the GPU is visible inside the VM
5. Clean up (delete VM, unbind from VFIO, restore nvidia driver)

## Future Plans: GPU Sharing Across Multiple VMs

### The Problem

With current VFIO passthrough, a GPU is assigned **exclusively** to one VM. To share a single GPU across multiple VMs (e.g., give each VM a "slice"), you need NVIDIA's **vGPU (GRID)** technology.

### Why MIG Alone Doesn't Help

**MIG (Multi-Instance GPU)** partitions a GPU into isolated instances at the hardware level, but:

- MIG partitions are **not separate PCI devices**—the GPU remains one PCI endpoint
- MIG partitions are accessed via CUDA APIs (`CUDA_VISIBLE_DEVICES=MIG-<uuid>`)
- You can only VFIO-passthrough the **whole GPU** to one VM
- MIG is useful for workload isolation **within** a single host or VM, not for multi-VM sharing

```
Physical GPU (0000:a2:00.0) ─── still ONE PCI device
    └── MIG partitions (logical, not separate devices)
        ├── MIG Instance 0 ─┐
        ├── MIG Instance 1 ─┼── All accessed via CUDA on the same GPU
        └── MIG Instance 2 ─┘
```

**Supported MIG Hardware**: A100, A30, H100, H200 (NOT L4 or consumer GPUs)

### vGPU/mdev: The Only Path to Multi-VM GPU Sharing

To assign GPU shares to **separate VMs**, NVIDIA requires their **vGPU (GRID)** technology, which uses the Linux mediated device (mdev) framework.

#### Cloud-Hypervisor mdev Support Status

Cloud-hypervisor **does** support mdev passthrough:

```bash
cloud-hypervisor --device path=/sys/bus/mdev/devices/<uuid>/
```

However, NVIDIA's proprietary vGPU manager has a QEMU-specific quirk: it reads the VMM process's `/proc/<pid>/cmdline` looking for a `-uuid` argument to map mdev UUIDs to VMs. This doesn't work out-of-the-box with cloud-hypervisor.

**Workarounds** (from [cloud-hypervisor#5319](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/5319)):
- Patch CH to accept a dummy `-uuid` flag
- Use wrapper scripts that inject the UUID into the process name
- Wait for NVIDIA to fix their driver's VMM assumptions

#### vGPU Requirements

- **Hardware**: Datacenter GPUs (A100, L40, etc.)
- **Licensing**: NVIDIA GRID subscription ($$/GPU/year)
- **Host Software**: NVIDIA vGPU Manager installed on host
- **Guest Drivers**: vGPU-aware guest drivers

### Design Changes for mdev/vGPU Support

#### 1. New Device Type: `MdevDevice`

```go
type MdevDevice struct {
    UUID         string   // mdev instance UUID
    ParentGPU    string   // PCI address of parent GPU  
    Type         string   // vGPU type (e.g., "nvidia-256")
    Available    bool     // Not assigned to a VM
}
```

#### 2. Discovery Extensions

```go
// List mdev types supported by a GPU
func (m *manager) ListMdevTypes(ctx context.Context, pciAddress string) ([]MdevType, error)

// List existing mdev instances
func (m *manager) ListMdevInstances(ctx context.Context) ([]MdevDevice, error)

// Create an mdev instance
func (m *manager) CreateMdevInstance(ctx context.Context, pciAddress, mdevType string) (*MdevDevice, error)

// Destroy an mdev instance
func (m *manager) DestroyMdevInstance(ctx context.Context, uuid string) error
```

#### 3. Passthrough Mechanism

mdev devices use a different sysfs path:

```
# mdev device path
/sys/bus/mdev/devices/<uuid>/

# vs VFIO-PCI (current)
/sys/bus/pci/devices/0000:a2:00.0/
```

Cloud-hypervisor's `--device` flag already accepts mdev paths.

#### 4. NVIDIA vGPU Workaround

To work around NVIDIA's QEMU-specific UUID detection, we may need to:
- Add a `--platform uuid=<uuid>` option to cloud-hypervisor invocation
- Or use a wrapper that sets the process name appropriately

### Implementation Phases

**Phase 1**: mdev Discovery & Passthrough
- Detect mdev-capable GPUs
- List available mdev types and instances
- Pass mdev devices to VMs (path already works)

**Phase 2**: mdev Lifecycle Management
- Create/destroy mdev instances via sysfs
- API endpoints for mdev management

**Phase 3**: NVIDIA vGPU Integration
- Implement UUID workaround for NVIDIA's driver
- Test with GRID licensing
- Document guest driver requirements

### How vGPU + MIG Work Together

vGPU creates mdev devices that can be backed by MIG partitions, giving you both hardware isolation (MIG) and multi-VM assignment (vGPU):

```
Physical GPU (one PCI device)
    │
    ├── Without vGPU: VFIO passthrough gives whole GPU to ONE VM
    │
    └── With vGPU (GRID license required):
        └── MIG Mode enabled on host
            ├── MIG Instance 0 ──→ vGPU mdev A ──→ VM 1
            ├── MIG Instance 1 ──→ vGPU mdev B ──→ VM 2
            └── MIG Instance 2 ──→ vGPU mdev C ──→ VM 3
```

Without vGPU, MIG is only useful for workload isolation on the host or within a single VM that owns the whole GPU.
