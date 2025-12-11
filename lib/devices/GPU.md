# GPU Passthrough Support

This document covers NVIDIA GPU passthrough specifics. For general device passthrough, see [README.md](README.md).

## How GPU Passthrough Works

hypeman supports NVIDIA GPU passthrough via VFIO, with automatic driver injection:

```
┌─────────────────────────────────────────────────────────────────────┐
│  hypeman Initrd (built at startup)                                  │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │  /lib/modules/<kver>/kernel/drivers/gpu/                     │   │
│  │    ├── nvidia.ko                                             │   │
│  │    ├── nvidia-uvm.ko                                         │   │
│  │    ├── nvidia-modeset.ko                                     │   │
│  │    └── nvidia-drm.ko                                         │   │
│  ├──────────────────────────────────────────────────────────────┤   │
│  │  /usr/lib/nvidia/                                            │   │
│  │    ├── libcuda.so.570.86.16                                  │   │
│  │    ├── libnvidia-ml.so.570.86.16                             │   │
│  │    ├── libnvidia-ptxjitcompiler.so.570.86.16                 │   │
│  │    └── ... (other driver libraries)                          │   │
│  └──────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
                              │
                              ▼ (at VM boot, if HAS_GPU=1)
┌─────────────────────────────────────────────────────────────────────┐
│  Guest VM                                                           │
│  1. Load kernel modules (modprobe nvidia, etc.)                     │
│  2. Create device nodes (/dev/nvidia0, /dev/nvidiactl, etc.)        │
│  3. Copy driver libs to container rootfs                            │
│  4. Run ldconfig to update library cache                            │
│  5. Container can now use GPU!                                      │
└─────────────────────────────────────────────────────────────────────┘
```

## Container Image Requirements

With driver injection, containers **do not need** to bundle NVIDIA driver libraries.

**Minimal CUDA image example:**

```dockerfile
FROM nvidia/cuda:12.4-runtime-ubuntu22.04
# Your application - no driver installation needed!
RUN pip install torch
CMD ["python", "train.py"]
```

hypeman injects the following at boot:

- `libcuda.so` - CUDA driver API
- `libnvidia-ml.so` - NVML (nvidia-smi, monitoring)
- `libnvidia-ptxjitcompiler.so` - PTX JIT compilation
- `libnvidia-nvvm.so` - NVVM compiler
- `libnvidia-gpucomp.so` - GPU compute library
- `nvidia-smi` binary
- `nvidia-modprobe` binary

## Driver Version Compatibility

The driver libraries injected by hypeman are pinned to a specific version that matches the kernel modules. This version is tracked in:

- **Kernel release:** `onkernel/linux` GitHub releases (e.g., `ch-6.12.8-kernel-2-20251211`)
- **hypeman config:** `lib/system/versions.go` - `NvidiaDriverVersion` map

### Current Driver Version

| Kernel Version | Driver Version | Release Date |
|---------------|----------------|--------------|
| ch-6.12.8-kernel-2-20251211 | 570.86.16 | 2025-12-11 |

### CUDA Compatibility

Driver 570.86.16 supports CUDA 12.4 and earlier. Check [NVIDIA's compatibility matrix](https://docs.nvidia.com/deploy/cuda-compatibility/) for details.

## Upgrading the Driver

To upgrade the NVIDIA driver version:

1. **Choose a new version** from [NVIDIA's Linux drivers](https://www.nvidia.com/Download/index.aspx)

2. **Update onkernel/linux:**
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

## Supported GPUs

All NVIDIA datacenter GPUs supported by the open-gpu-kernel-modules are supported:

- NVIDIA H100, H200
- NVIDIA L4, L40, L40S
- NVIDIA A100, A10, A30
- NVIDIA T4
- And other Turing/Ampere/Hopper/Ada Lovelace architecture GPUs

Consumer GPUs (GeForce) are **not** supported by the open kernel modules.

## Troubleshooting

### nvidia-smi shows wrong driver version

The driver version shown by nvidia-smi should match hypeman's configured version. If it differs, the container may have its own driver libraries that are taking precedence. Either:

- Use a minimal CUDA runtime image without driver libs
- Or ensure the container's driver version matches

### CUDA initialization failed

Check that:

1. Kernel modules are loaded: `cat /proc/modules | grep nvidia`
2. Device nodes exist: `ls -la /dev/nvidia*`
3. Libraries are in LD_LIBRARY_PATH: `ldconfig -p | grep nvidia`

### Driver/library version mismatch

Error like `NVML_ERROR_LIB_RM_VERSION_MISMATCH` means the userspace library version doesn't match the kernel module version. This shouldn't happen with hypeman's automatic injection, but can occur if the container has its own driver libraries.

**Solution:** Use a base image that doesn't include driver libraries, or ensure any bundled libraries match the hypeman driver version.

### GPU not detected in container

1. Verify the GPU was attached to the instance:
   ```bash
   hypeman instance get <id> | jq .devices
   ```

2. Check the VM console log for module loading errors:
   ```bash
   cat /var/lib/hypeman/instances/<id>/console.log | grep -i nvidia
   ```

3. Verify VFIO binding on the host:
   ```bash
   ls -la /sys/bus/pci/devices/<pci-addr>/driver
   ```

## Performance Tuning

### Huge Pages

For best GPU performance, enable huge pages on the host:

```bash
echo 1024 > /proc/sys/vm/nr_hugepages
```

### IOMMU Configuration

Ensure IOMMU is properly configured:

```bash
# Intel
intel_iommu=on iommu=pt

# AMD
amd_iommu=on iommu=pt
```

The `iommu=pt` (passthrough) option improves performance for devices not using VFIO.

