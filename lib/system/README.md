# System Manager

Manages versioned kernel and initrd files for Cloud Hypervisor VMs.

## Features

- **Automatic Downloads**: Kernel downloaded from Cloud Hypervisor releases on first use
- **Automatic Build**: Initrd built from busybox + custom init script
- **Versioned**: Side-by-side support for multiple kernel/initrd versions
- **Zero Docker**: Uses OCI directly (reuses image manager infrastructure)
- **Zero Image Modifications**: All init logic in initrd, OCI images used as-is

## Architecture

### Storage Layout

```
{dataDir}/system/
├── kernel/
│   ├── ch-v6.12.8/
│   │   ├── x86_64/vmlinux   (~70MB)
│   │   └── aarch64/Image    (~70MB)
│   └── ch-v6.12.9/
│       └── ... (future version)
├── initrd/
│   ├── v1.0.0/
│   │   ├── x86_64/initrd    (~1-2MB)
│   │   └── aarch64/initrd   (~1-2MB)
│   └── v1.1.0/
│       └── ... (when init script changes)
└── oci-cache/              (shared with images manager)
    └── blobs/sha256/       (busybox layers cached)
```

### Versioning Rules

**Snapshots require exact matches:**
```
Standby:  kernel v6.12.9, initrd v1.0.0, CH v49.0
Restore:  kernel v6.12.9, initrd v1.0.0, CH v49.0 (MUST match)
```

**Maintenance upgrades (shutdown → boot):**
```
1. Update DefaultKernelVersion in versions.go
2. Shutdown instance
3. Boot instance (uses new kernel/initrd)
```

**Multi-version support:**
```
Instance A (standby): kernel v6.12.8, initrd v1.0.0
Instance B (running): kernel v6.12.9, initrd v1.0.0
Both work independently
```

## Init Script Consolidation

All init logic moved from app rootfs to initrd:

**Initrd handles:**
- ✅ Mount overlay filesystem
- ✅ Mount and source config disk
- ✅ Network configuration (if enabled)
- ✅ Execute container entrypoint

**Result:** OCI images require **zero modifications** - no `/init` script needed!

## Usage

### Application Startup

```go
// cmd/api/main.go
systemMgr := system.NewManager(dataDir)

// Ensure files exist (download/build if needed)
err := systemMgr.EnsureSystemFiles(ctx)

// Files are ready, instances can be created
```

### Instance Creation

```go
// Instances manager uses system manager automatically
inst, err := instanceManager.CreateInstance(ctx, req)
// Uses default kernel/initrd versions
// Versions stored in instance metadata for restore compatibility
```

### Get File Paths

```go
kernelPath, _ := systemMgr.GetKernelPath(system.KernelV6_12_9)
initrdPath, _ := systemMgr.GetInitrdPath(system.InitrdV1_0_0)
```

## Kernel Sources

Kernels downloaded from Cloud Hypervisor releases:
- https://github.com/cloud-hypervisor/linux/releases

Example URLs:
- x86_64: `https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.9/vmlinux-x86_64`
- aarch64: `https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.9/Image-aarch64`

## Initrd Build Process

1. **Pull busybox** (using image manager's OCI client)
2. **Inject init script** (comprehensive, handles all init logic)
3. **Package as cpio.gz** (initramfs format)

**Build tools required:** `find`, `cpio`, `gzip` (standard Unix tools)

## Adding New Versions

### New Kernel Version

```go
// lib/system/versions.go

const (
    KernelV6_12_10 KernelVersion = "ch-v6.12.10"  // Add constant
)

var KernelDownloadURLs = map[KernelVersion]map[string]string{
    // ... existing ...
    KernelV6_12_10: {
        "x86_64":  "https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.10/vmlinux-x86_64",
        "aarch64": "https://github.com/cloud-hypervisor/linux/releases/download/ch-v6.12.10/Image-aarch64",
    },
}

// Update default if needed
var DefaultKernelVersion = KernelV6_12_10
```

### New Initrd Version

```go
// lib/system/versions.go

const (
    InitrdV1_1_0 InitrdVersion = "v1.1.0"  // Add constant
)

// lib/system/init_script.go
// Update GenerateInitScript() if init logic changes

// Update default
var DefaultInitrdVersion = InitrdV1_1_0
```

## Testing

```bash
# Unit tests (no downloads)
go test -short ./lib/system/...

# Integration tests (downloads kernel, builds initrd)
go test ./lib/system/...
```

## Files Generated

| File | Size | Purpose |
|------|------|---------|
| kernel/*/vmlinux | ~70MB | Cloud Hypervisor optimized kernel |
| initrd/*/initrd | ~1-2MB | Busybox + comprehensive init script |

Files downloaded/built once per version, reused for all instances using that version.

