# System Manager

Manages versioned kernel and initrd files for Cloud Hypervisor VMs.

## Features

- **Automatic Downloads**: Kernel downloaded from kernel/linux releases on first use
- **Automatic Build**: Initrd built from Alpine base + Go init binary + guest-agent
- **Versioned**: Side-by-side support for multiple kernel versions
- **Zero Docker**: Uses OCI directly (reuses image manager infrastructure)
- **Zero Image Modifications**: All init logic in initrd, OCI images used as-is
- **Dual Mode Support**: Exec mode (container-like) and systemd mode (full VM)

## Architecture

### Storage Layout

```
{dataDir}/system/
├── kernel/
│   ├── ch-6.12.8-kernel-1-202511182/
│   │   ├── x86_64/vmlinux   (~70MB)
│   │   └── aarch64/Image    (~70MB)
│   └── ch-6.12.8-kernel-1.2-20251213/
│       └── ... (newer version)
├── initrd/
│   ├── 1734567890/              (timestamp-based)
│   │   ├── x86_64/initrd        (~5-10MB)
│   │   └── aarch64/initrd
│   ├── x86_64/latest -> 1734567890  (symlink to latest)
│   └── aarch64/latest -> 1734567890
└── oci-cache/                   (shared with images manager)
    └── blobs/sha256/            (Alpine layers cached)
```

### Versioning Rules

**Snapshots require exact matches:**
```
Standby:  kernel ch-6.12.8-kernel-1.2-20251213, CH v49.0
Restore:  kernel ch-6.12.8-kernel-1.2-20251213, CH v49.0 (MUST match)
```

**Maintenance upgrades (shutdown → boot):**
```
1. Update DefaultKernelVersion in versions.go
2. Shutdown instance
3. Boot instance (uses new kernel/initrd)
```

**Multi-version support:**
```
Instance A (standby): kernel ch-6.12.8-kernel-1-202511182
Instance B (running): kernel ch-6.12.8-kernel-1.2-20251213
Both work independently
```

## Go Init Binary

The init binary (`lib/system/init/`) is a Go program that runs as PID 1 in the guest VM.
It replaces the previous shell-based init script with cleaner logic and structured logging.

**Initrd handles:**
- ✅ Mount overlay filesystem
- ✅ Mount and source config disk
- ✅ Network configuration (if enabled)
- ✅ Load GPU drivers (if GPU attached)
- ✅ Mount volumes
- ✅ Execute container entrypoint (exec mode)
- ✅ Hand off to systemd via chroot + exec (systemd mode)

**Two boot modes:**
- **Exec mode** (default): Init chroots to container rootfs, runs entrypoint as child process, then waits on guest-agent to keep VM alive
- **Systemd mode** (auto-detected on host): Init chroots to container rootfs, then execs /sbin/init so systemd becomes PID 1

**Systemd detection:** Host-side detection in `lib/images/systemd.go` checks if image CMD is
`/sbin/init`, `/lib/systemd/systemd`, or similar. The detected mode is passed to the initrd
via `INIT_MODE` in the config disk.

**Result:** OCI images require **zero modifications** - no `/init` script needed!

## Kernel Headers

Kernel headers are bundled in the initrd and automatically installed at boot, enabling DKMS to build out-of-tree kernel modules (e.g., NVIDIA vGPU drivers).

**Why:** Guest images come with headers for their native kernel (e.g., Ubuntu's 5.15), but hypeman VMs run a custom kernel. Without matching headers, DKMS cannot compile drivers.

**How:** The initrd includes `kernel-headers.tar.gz` from the same release as the kernel. At boot, init extracts headers to `/usr/src/linux-headers-{version}/`, creates the `/lib/modules/{version}/build` symlink, and removes mismatched headers from the guest image.

**Result:** Guests can `apt install nvidia-driver-xxx` and DKMS builds modules for the running kernel automatically.

## Kernel Sources

Kernels downloaded from kernel/linux releases (Cloud Hypervisor-optimized fork):
- https://github.com/kernel/linux/releases

Example URLs:
- x86_64: `https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.2-20251213/vmlinux-x86_64`
- aarch64: `https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.2-20251213/Image-arm64`

## Initrd Build Process

1. **Pull Alpine base** (using image manager's OCI client)
2. **Add guest-agent binary** (embedded, runs in guest for exec/shell)
3. **Add init.sh wrapper** (mounts /proc, /sys, /dev before Go runtime)
4. **Add init binary** (embedded Go binary, runs as PID 1)
5. **Add kernel headers tarball** (downloaded from release, for DKMS)
6. **Package as cpio** (initramfs format, pure Go - no shell tools required)

## Adding New Versions

### New Kernel Version

```go
// lib/system/versions.go

const (
    Kernel_20251220 KernelVersion = "ch-6.12.8-kernel-1.3-20251220"  // Add constant
)

var KernelDownloadURLs = map[KernelVersion]map[string]string{
    // ... existing ...
    Kernel_20251220: {
        "x86_64":  "https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.3-20251220/vmlinux-x86_64",
        "aarch64": "https://github.com/kernel/linux/releases/download/ch-6.12.8-kernel-1.3-20251220/Image-arm64",
    },
}

// Update default if needed
var DefaultKernelVersion = Kernel_20251220
```

### Updating the Init Binary

The init binary is in `lib/system/init/`. After making changes:

1. Build the init binary (statically linked for Alpine):
   ```bash
   make build-init
   ```

2. The binary is embedded via `lib/system/init_binary.go`

3. The initrd hash includes the binary, so it will auto-rebuild on next startup

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
| initrd/*/initrd | ~20MB | Alpine base + init binary + guest-agent + kernel headers |

Files downloaded/built once per version, reused for all instances using that version.

## Init Binary Package Structure

```
lib/system/init/
    main.go           # Entry point, orchestrates boot
    init.sh           # Shell wrapper (mounts /proc, /sys, /dev before Go runtime)
    mount.go          # Mount operations (overlay, bind mounts)
    config.go         # Parse config disk
    network.go        # Network configuration
    headers.go        # Kernel headers setup for DKMS
    volumes.go        # Volume mounting
    mode_exec.go      # Exec mode: chroot, run entrypoint, wait on guest-agent
    mode_systemd.go   # Systemd mode: chroot + exec /sbin/init
    logger.go         # Human-readable logging to hypeman operations log
```
