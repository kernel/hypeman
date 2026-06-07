# RFC: Rosetta x86-64 emulation for macOS (vz) Linux guests

> **Status: implemented.** Rosetta host+guest emulation and multi-platform
> image pull have shipped. This document captures the original design plus the
> two decisions that changed during implementation: emulation is automatic (no
> user-facing flag — see "What was implemented" below) and the image/instance
> API exposes a Docker-style `platform` field rather than an architecture knob.

## Summary

Add a Rosetta directory share to hypeman's Apple Silicon `vz` Linux guests so that x86-64 (amd64) binaries execute inside the arm64 microVM via Apple's Rosetta dynamic binary translator. This unlocks x86-only OCI images and tools that today either fail to run under `vz` or fall back to slow userspace emulation. The host wires `VZVirtioFileSystemDeviceConfiguration` + `VZLinuxRosettaDirectoryShare` in the `vz-shim`, and the guest `init` binary automatically registers a `binfmt_misc` handler pointing at the mounted Rosetta interpreter so the feature works transparently rather than requiring manual guest setup. Rosetta is enabled automatically when an instance's image targets a non-host architecture, mirroring Docker Desktop where Rosetta is a host setting rather than a per-container flag.

## Motivation

hypeman runs OCI images in microVMs across multiple hypervisors; on macOS it uses Apple's Virtualization.framework via the `vz` hypervisor (`README.md`, "macOS 11.0+ on Apple Silicon"). On Apple Silicon the guest kernel and userspace are arm64. An amd64-only image, or an amd64 binary inside a multi-arch image, cannot be executed by the arm64 guest kernel: `execve` of an ELF with `e_machine = EM_X86_64` returns `ENOEXEC` unless a `binfmt_misc` interpreter is registered for that signature.

Two groups are helped:

- Users running x86-only container images on Apple Silicon developer machines (common for legacy build tooling, proprietary amd64 binaries, and images that have no arm64 variant).
- Build/CI workflows that need to exercise amd64 artifacts on Apple Silicon without standing up a separate x86 host or paying the latency cost of full userspace CPU emulation (e.g. QEMU user-mode), which is typically several times slower than Rosetta's ahead-of-time/just-in-time translation.

Apple's Virtualization.framework exposes Rosetta to Linux guests as a virtio-fs directory share: the host shares a synthetic directory containing the `rosetta` interpreter, the guest mounts it over virtio-fs, and a `binfmt_misc` rule routes x86-64 ELF execution through that interpreter. hypeman does not configure any directory-sharing device today, so this capability is entirely unavailable.

## Current behavior in hypeman

**No directory-sharing / filesystem device is configured for `vz` VMs.** `createVM` in `cmd/vz-shim/vm.go:19` builds the boot loader, serial console, network, entropy, storage, platform, vsock, and (optionally) a memory balloon, then validates and instantiates the VM. There is no call to `SetDirectorySharingDevicesVirtualMachineConfiguration`, and `configurePlatform` (`cmd/vz-shim/vm.go:101`) only sets a generic platform identifier. A repo-wide search for `rosetta`, `virtiofs`, and `binfmt` in `lib/` and `cmd/` returns nothing — none of this plumbing exists yet.

**Host -> shim configuration flow.** The API server builds a hypervisor-agnostic `hypervisor.VMConfig` (`lib/hypervisor/config.go:5`). For `vz`, `buildShimConfigFromVMConfig` (`lib/hypervisor/vz/starter.go:161`) translates it into a `shimconfig.ShimConfig` (`lib/hypervisor/vz/shimconfig/config.go:16`), which is JSON-marshalled and passed to the `vz-shim` subprocess via `-config` (`lib/hypervisor/vz/starter.go:206`). `ShimConfig` currently carries compute, disks, networks, console, boot, balloon, socket, log, and machine-identity fields — no directory shares.

**Host -> guest configuration flow (separate channel).** Guest-visible configuration travels on a different path: `createConfigDisk` / `buildGuestConfig` (`lib/instances/configdisk.go:20`, `:50`) serialize a `vmconfig.Config` (`lib/vmconfig/config.go:7`) to `config.json` on an ext4 "config disk" that the guest mounts at `/dev/vdc`. The guest `init` reads it in `readConfig` (`lib/system/init/config.go:14`). `vmconfig.Config` carries entrypoint/cmd/env, network, volume mounts, init mode, and boot-optimization toggles — nothing about emulation.

**Guest boot sequence.** `init.sh` (`lib/system/init/init.sh`) mounts `/proc`, `/sys`, `/dev`, then `exec`s the Go `init` binary. `main` (`lib/system/init/main.go:23`) runs phases: mount essentials (`lib/system/init/mount.go:18`), set up the overlay rootfs at `/overlay/newroot` (`lib/system/init/mount.go:91`), read config, network/volume setup, bind-mount `/proc`, `/sys`, `/dev` into the new root (`lib/system/init/mount.go:145`), copy the guest-agent (`lib/system/init/mount.go:235`), then run mode-specific execution. Both `runExecMode` (`lib/system/init/mode_exec.go:33`) and `runSystemdMode` (`lib/system/init/mode_systemd.go:46`) `syscall.Chroot("/overlay/newroot")` before launching the workload. There is no virtio-fs mount and no `binfmt_misc` registration anywhere in this sequence.

**Kernel command line.** `createVM` sets `console=hvc0 root=/dev/vda` by default and rewrites `console=ttyS0` to `console=hvc0` (`cmd/vz-shim/vm.go:21-26`). Console detection in the guest tries `hvc0`, `ttyAMA0`, `ttyS0` (`lib/system/init/mount.go:55`).

**Entitlements.** The `vz-shim` is codesigned at extraction time with embedded entitlements (`lib/hypervisor/vz/starter.go:81`, embedding `vz.entitlements` via `lib/hypervisor/vz/vz_entitlements.go`). Both `vz.entitlements` (repo root) and `lib/hypervisor/vz/vz.entitlements` declare `com.apple.security.virtualization` plus network client/server. No additional entitlement is declared.

**Snapshot/fork model.** `vz` supports snapshot only on arm64 (`lib/hypervisor/vz/client.go:87`). Snapshots persist the full `ShimConfig` in a manifest (`lib/hypervisor/vz/shimconfig/config.go:67`) and restore by replaying it (`lib/hypervisor/vz/starter.go:148`). Fork rewrites paths/sockets in that manifest (`lib/hypervisor/vz/fork.go:33`) and explicitly avoids mutating device identity because "VZ machine-state restore requires device configuration compatibility" (`lib/hypervisor/vz/fork.go:65-67`). Any new device added to the config participates in this restore-compatibility constraint.

## Prior art: Tart

Tart is an open-source VM toolset also built on Apple's Virtualization.framework. It is primarily a CLI, frequently used for macOS guests, and it already implements a Rosetta directory share for Linux guests. It is the closest available reference for the host-side wiring, and the parts hypeman should follow versus diverge from are clear.

**What Tart does (host side):**

- Exposes a `--rosetta=TAG` CLI option, gated to `#if arch(arm64)` (`Sources/tart/Commands/Run.swift:146-160`). The help text tells the user Rosetta must be installed on the host (`softwareupdate --install-rosetta`) and links Apple's documentation for the *guest-side* mount + registration step.
- Appends `rosettaDirectoryShare()` to the VM's directory-sharing devices (`Sources/tart/Commands/Run.swift:415`), which feed `configuration.directorySharingDevices` (`Sources/tart/VM.swift:422`).
- `rosettaDirectoryShare()` (`Sources/tart/Commands/Run.swift:725-752`) is the canonical sequence:
  - return early if no tag was passed;
  - require macOS 13 (`guard #available(macOS 13, *)`);
  - branch on `VZLinuxRosettaDirectoryShare.availability`, throwing for `.notInstalled` and `.notSupported`;
  - `VZVirtioFileSystemDeviceConfiguration.validateTag(rosettaTag)`;
  - construct `VZVirtioFileSystemDeviceConfiguration(tag: rosettaTag)`, set `device.share = try VZLinuxRosettaDirectoryShare()`, and return `[device]`;
  - on `#elseif arch(x86_64)` return `[]` ("there is no Rosetta on Intel").

**What to adopt:** the exact device-construction order — validate tag, create the virtio-fs device with that tag, attach a Rosetta share, and gate on the three-state availability — is correct and we mirror it. The Code-Hex/vz Go binding exposes the same primitives (see Proposed design), so this is a faithful translation of an established pattern, not a copy of code.

**Where to diverge:**

1. **No CLI flag; thread through config instead.** Tart is a single-process CLI, so a `--rosetta=TAG` flag is the natural surface. hypeman is a server that drives a `vz-shim` subprocess and a separate guest `init`, so the toggle belongs in `shimconfig.ShimConfig` (host -> shim) and `vmconfig.Config` (host -> guest), surfaced through the instance API rather than an argv flag.

2. **Automate guest-side registration.** Tart deliberately stops at the host device and points the user at Apple's docs to mount virtio-fs and write the `binfmt_misc` rule inside the guest. For hypeman that manual step is unacceptable: instances are ephemeral and launched programmatically. hypeman's guest `init` already owns the early-boot environment (mounts, overlay, config), so it is the right place to mount the Rosetta share and register the `binfmt_misc` handler automatically. The user-facing contract becomes "enable Rosetta" and nothing else.

3. **Fixed, internal mount tag.** Tart lets the user choose the tag because the user also performs the matching guest mount. Since hypeman performs both sides, the tag is an internal contract and can be a constant (e.g. `rosetta`), eliminating a class of user error.

## Proposed design

Introduce a single user-facing toggle, `EnableRosetta`, that hypeman threads through both configuration channels. The host attaches the Rosetta virtio-fs device; the guest `init` mounts it and registers `binfmt_misc` automatically.

### Verified Code-Hex/vz v3.7.1 binding surface

The bindings used below were verified against the module in cache (`github.com/Code-Hex/vz/v3 v3.7.1`, see `go.sum`). Exact symbols:

- `vz.NewVirtioFileSystemDeviceConfiguration(tag string) (*VirtioFileSystemDeviceConfiguration, error)` — `shared_directory.go:43`. Validates the tag internally (the ObjC layer calls `[VZVirtioFileSystemDeviceConfiguration validateTag:error:]`, `virtualization_12.m:297`) and returns an error on an invalid tag. **Note:** unlike Swift, the Go binding does *not* expose a standalone `ValidateTag` function — the constructor is the validation point.
- `(*VirtioFileSystemDeviceConfiguration).SetDirectoryShare(share DirectoryShare)` — `shared_directory.go:66`.
- `vz.NewLinuxRosettaDirectoryShare() (*LinuxRosettaDirectoryShare, error)` — `shared_directory_arm64.go:65`; internally requires macOS 13 (`macOSAvailable(13)`, `shared_directory_arm64.go:66`).
- `vz.LinuxRosettaDirectoryShareAvailability() LinuxRosettaAvailability` — `shared_directory_arm64.go:116`. Returns `LinuxRosettaAvailabilityNotSupported` on macOS < 13.
- Availability constants — `shared_directory_arm64.go:26-35`: `LinuxRosettaAvailabilityNotSupported` (0), `LinuxRosettaAvailabilityNotInstalled` (1), `LinuxRosettaAvailabilityInstalled` (2).
- `vz.LinuxRosettaDirectoryShareInstallRosetta() error` — `shared_directory_arm64.go:99` (programmatic install; macOS 13+).
- `(*VirtualMachineConfiguration).SetDirectorySharingDevicesVirtualMachineConfiguration(cs []DirectorySharingDeviceConfiguration)` — `configuration.go:185`.
- macOS 14+ translation cache (follow-up): `(*LinuxRosettaDirectoryShare).SetOptions(LinuxRosettaCachingOptions)` — `shared_directory_arm64.go:87`; `vz.NewLinuxRosettaUnixSocketCachingOptions(path string)` — `shared_directory_arm64.go:153`; `vz.NewLinuxRosettaAbstractSocketCachingOptions(name string)` — `shared_directory_arm64.go:201`.

The Rosetta bindings live behind the build tag `//go:build darwin && arm64` (`shared_directory_arm64.go:1`). `VirtioFileSystemDeviceConfiguration` itself is in the platform-neutral `shared_directory.go`. Because hypeman builds the shim from a single `vm.go` guarded by `//go:build darwin` (`cmd/vz-shim/vm.go:1`), the arm64-only Rosetta calls must be isolated into an arm64-tagged file with a non-arm64 stub. Note that `cmd/vz-shim` is arm64-only in practice: Code-Hex/vz v3.7.1 does not compile for `darwin/amd64` (its generated `virtualmachinestate_string.go` references `VirtualMachineState*` consts that the cgo file `virtualization.go` only provides on arm64), and the package already imports vz unconditionally (e.g. `save_restore_unsupported.go`). The `!arm64` stub therefore only satisfies the type checker for that build tag — mirroring the existing save/restore stub — rather than enabling a real Intel-macOS build.

### 1. Config plumbing

`shimconfig.ShimConfig` gains one field (`lib/hypervisor/vz/shimconfig/config.go:16`):

```go
// Rosetta x86-64 emulation. When true and the host supports it, the shim
// attaches a Linux Rosetta virtio-fs share so the guest can execute amd64
// binaries. Mount tag is fixed (shimconfig.RosettaMountTag).
EnableRosetta bool `json:"enable_rosetta,omitempty"`
```

Add the fixed tag as a shared constant in the same package so host and guest agree without a wire field:

```go
// RosettaMountTag is the virtio-fs tag for the Rosetta directory share.
// Host (vz-shim) and guest (init) both use this constant.
const RosettaMountTag = "rosetta"
```

`vmconfig.Config` gains the guest-side toggle (`lib/vmconfig/config.go:7`):

```go
// EnableRosetta registers a binfmt_misc handler for x86-64 ELF binaries
// backed by the Rosetta interpreter mounted from the "rosetta" virtio-fs share.
EnableRosetta bool `json:"enable_rosetta,omitempty"`
```

`buildShimConfigFromVMConfig` (`lib/hypervisor/vz/starter.go:161`) copies it from `hypervisor.VMConfig` (which gains `EnableRosetta bool`, `lib/hypervisor/config.go:5`):

```go
cfg := shimconfig.ShimConfig{
    // ... existing fields ...
    EnableRosetta: config.EnableRosetta,
}
```

`buildGuestConfig` (`lib/instances/configdisk.go:50`) sets the guest flag from the same instance-level source the API server already resolves:

```go
cfg.EnableRosetta = inst.EnableRosetta
```

Both flags derive from one instance-level setting, so a single user input drives host device attachment and guest registration. The guest mount tag is the `shimconfig.RosettaMountTag` constant; the guest reads it from the same package, so it is never serialized.

### 2. Host side: attach the Rosetta device in the shim

Add a directory-sharing configuration step to `createVM`, mirroring the existing device-setup style (each `configureX` returns an error and is called from `createVM`). Insert after `configureStorage` and before `configurePlatform` (`cmd/vz-shim/vm.go:61-67`):

```go
if err := configureDirectorySharing(vmConfig, config); err != nil {
    return nil, nil, fmt.Errorf("configure directory sharing: %w", err)
}
```

The architecture-neutral entry point lives in `vm.go` (build tag `darwin`); the Rosetta-specific construction lives in an arm64-tagged file with a non-arm64 stub, because `NewLinuxRosettaDirectoryShare` / `LinuxRosettaDirectoryShareAvailability` only exist under `darwin && arm64`.

`cmd/vz-shim/rosetta_arm64.go` (`//go:build darwin && arm64`):

```go
//go:build darwin && arm64

package main

import (
    "fmt"
    "log/slog"

    "github.com/Code-Hex/vz/v3"
    "github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// configureDirectorySharing attaches the Linux Rosetta share when enabled.
func configureDirectorySharing(vmConfig *vz.VirtualMachineConfiguration, config *shimconfig.ShimConfig) error {
    if !config.EnableRosetta {
        return nil
    }

    switch vz.LinuxRosettaDirectoryShareAvailability() {
    case vz.LinuxRosettaAvailabilityInstalled:
        // proceed
    case vz.LinuxRosettaAvailabilityNotInstalled:
        return fmt.Errorf("rosetta requested but not installed (run: softwareupdate --install-rosetta)")
    default: // LinuxRosettaAvailabilityNotSupported
        return fmt.Errorf("rosetta requested but not supported on this host (requires Apple silicon + macOS 13+)")
    }

    fsDevice, err := vz.NewVirtioFileSystemDeviceConfiguration(shimconfig.RosettaMountTag)
    if err != nil {
        return fmt.Errorf("create rosetta virtio-fs device (tag %q): %w", shimconfig.RosettaMountTag, err)
    }

    share, err := vz.NewLinuxRosettaDirectoryShare()
    if err != nil {
        return fmt.Errorf("create rosetta directory share: %w", err)
    }
    fsDevice.SetDirectoryShare(share)

    vmConfig.SetDirectorySharingDevicesVirtualMachineConfiguration(
        []vz.DirectorySharingDeviceConfiguration{fsDevice},
    )
    slog.Info("attached rosetta directory share", "tag", shimconfig.RosettaMountTag)
    return nil
}
```

`cmd/vz-shim/rosetta_other.go` (`//go:build darwin && !arm64`):

```go
//go:build darwin && !arm64

package main

import (
    "fmt"

    "github.com/Code-Hex/vz/v3"
    "github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
)

// configureDirectorySharing is a no-op stub on non-arm64 macOS: Rosetta does
// not exist on Intel, matching Tart's #elseif arch(x86_64) -> [] behavior.
func configureDirectorySharing(_ *vz.VirtualMachineConfiguration, config *shimconfig.ShimConfig) error {
    if config.EnableRosetta {
        return fmt.Errorf("rosetta is only available on Apple silicon")
    }
    return nil
}
```

The availability switch order matches Tart (`Sources/tart/Commands/Run.swift:734-741`): `Installed` proceeds, `NotInstalled` and `NotSupported` are hard errors. We choose a hard error over silent install: triggering `LinuxRosettaDirectoryShareInstallRosetta()` from inside the per-VM shim would block VM startup on a multi-hundred-megabyte download and is better handled out of band (see Rollout). The error string is plumbed back to the API server through the existing shim-failure path (`lib/hypervisor/vz/starter.go:248`).

`vmConfig.Validate()` (`cmd/vz-shim/vm.go:89`) already runs after device configuration and will reject a malformed directory-sharing setup before `NewVirtualMachine`.

### 3. Guest side: mount the share and register binfmt_misc automatically

This is the core divergence from Tart. The work is added to the guest `init` binary (`lib/system/init`), in a new file `lib/system/init/rosetta.go`. It runs only when `cfg.EnableRosetta` is true.

**Placement.** Registration must happen after the overlay rootfs and the bind-mounts of `/proc`/`/sys`/`/dev` into `/overlay/newroot` are in place (`bindMountsToNewRoot`, `lib/system/init/mount.go:145`) and before the workload runs. Concretely, add a phase in `main` between Phase 6 (bind mounts) and Phase 9 (mode-specific execution), so it applies to both exec and systemd modes (both chroot into `/overlay/newroot`: `mode_exec.go:33`, `mode_systemd.go:46`):

```go
// Phase 6.5: Register Rosetta x86-64 emulation if enabled. systemd mode needs
// a binfmt.d drop-in (see below), so the mode is passed in.
if cfg.EnableRosetta {
    if err := setupRosetta(log, cfg.InitMode == "systemd"); err != nil {
        log.Error("hypeman-init:rosetta", "failed to set up rosetta", err)
        // Non-fatal: arm64 workloads still run; only amd64 exec is affected.
    }
}
```

**Why the `binfmt_misc` "F" (fix-binary) flag is essential.** The guest will `chroot("/overlay/newroot")` (`mode_exec.go:33`). `binfmt_misc` resolves the registered interpreter path at `execve` time. Without care, the interpreter path registered before chroot would be unreachable after chroot. The kernel's `F` flag opens the interpreter file at *registration* time and holds the open file descriptor, so the binary remains usable regardless of the caller's later root or mount-namespace changes. This is exactly the case Apple's Rosetta-in-Linux guidance and the systemd `binfmt.d` Rosetta examples use.

`init` mounts the Rosetta share **inside the overlay rootfs** at `/opt/hypeman/rosetta` (under `newRoot`) rather than at an init-only path like `/run/rosetta`. Two reasons: the interpreter then has a stable guest-absolute path (`/opt/hypeman/rosetta/rosetta`) that is valid both before and after the chroot, and `/opt` is not shadowed by the `/run` tmpfs systemd remounts during boot. The live registration uses the F flag with the interpreter's current (pre-chroot) path under `newRoot`; the pinned fd then survives the chroot.

`lib/system/init/rosetta.go` implements `setupRosetta(log *Logger, systemdMode bool)`:

1. mkdir + `mount(RosettaMountTag, <newRoot>/opt/hypeman/rosetta, "virtiofs", "")` (reusing the existing `mount` helper, `lib/system/init/mount.go:189`), then `waitForDevice` (`:72`) on the interpreter.
2. `ensureBinfmtMounted()` mounts `binfmt_misc` if it is not already present.
3. register the live rule (`rosettaBinfmtRule(interp)` → `:rosetta:M::<magic>:<mask>:<interp>:OCF`) by writing to `/proc/sys/fs/binfmt_misc/register`. The `OCF` flags request `O` keep `argv[0]`, `C` apply the target's credentials, `F` pin the interpreter fd. A duplicate-name `EEXIST` is treated as success (an image-shipped `:rosetta:` rule is left in place).
4. in **systemd mode only**, also write `/usr/lib/binfmt.d/rosetta.conf` into the overlay rootfs (referencing the guest-absolute interpreter path) so `systemd-binfmt` re-registers Rosetta after its boot-time flush (see Risks).

The magic/mask are the standard x86-64 ELF signature used by distro `binfmt.d` Rosetta rules (byte-identical to lima's vz-rosetta rule). After this phase, `runExecMode`/`runSystemdMode` chroot as usual and amd64 `execve` transparently dispatches to Rosetta.

If `setupRosetta` fails, boot continues — an arm64 workload is unaffected; only amd64 execution is lost. This mirrors the non-fatal treatment of network/volume setup in `main` (`lib/system/init/main.go:63`, `:72`).

### 4. Guest kernel prerequisites

The guest kernel must provide three options for this to work:

- `CONFIG_VIRTIO_FS=y` (and its dependency `CONFIG_FUSE_FS`) — to mount the `rosetta` share.
- `CONFIG_BINFMT_MISC=y` (or `=m`, loaded) — to register the interpreter.

If `CONFIG_VIRTIO_FS` is missing, the `virtiofs` mount fails with `unknown filesystem type 'virtiofs'`; if `binfmt_misc` is unavailable, `ensureBinfmtMounted` fails. Both surface as the non-fatal Rosetta error above. The kernel hypeman ships for `vz` guests must enable these; the testing plan includes a boot-time assertion. (No kernel build target exists in the repo Makefile today; the kernel artifact is provisioned externally, so this is a packaging requirement on that artifact, documented in the milestone list.)

## Configuration / API changes

`EnableRosetta` is the internal mechanism the shim and guest act on; it is
*derived*, not user-supplied (see "What was implemented").

- `hypervisor.VMConfig` (`lib/hypervisor/config.go`): `EnableRosetta bool`.
- `shimconfig.ShimConfig` (`lib/hypervisor/vz/shimconfig/config.go`): `EnableRosetta bool json:"enable_rosetta,omitempty"` and `const RosettaMountTag = "rosetta"`.
- `vmconfig.Config` (`lib/vmconfig/config.go`): `EnableRosetta bool json:"enable_rosetta,omitempty"`.
- Instance: `instances.CreateInstanceRequest` carries `Platform string` (drives image resolution / auto-pull) and the *internal* `StoredMetadata.EnableRosetta`, derived in `createInstance`. There is no `enable_rosetta`/`rosetta` field in the API.
- Snapshot manifests (`shimconfig.SnapshotManifest`) automatically carry `EnableRosetta` because they embed the full `ShimConfig`; no manifest schema change is needed. The field is `omitempty`, so pre-existing manifests deserialize as `EnableRosetta=false`.

## Multi-platform image pull

Image and instance creation accept a Docker-style `platform` field
(`os/arch[/variant]`, e.g. `linux/amd64`) modeled on `docker --platform`. It is
added to `CreateImageRequest`, `CreateInstanceRequest`, and as a read-only field
on the `Image` response in `openapi.yaml` (regenerated into `lib/oapi`). Today
only `os` `linux` is supported and `arch` must be `amd64` or `arm64`; other
values are rejected up front with a clear error.

- `lib/images` defines `Platform{OS, Architecture, Variant}` with `ParsePlatform`/`Normalize`/`String`/`ToGCR`, host-default and `needsEmulation` helpers, and a single host-platform source of truth (`oci.go`'s `vmPlatform` routes through it).
- When a non-host platform is requested, `CreateImage` resolves the platform-specific manifest digest and pins the ref to it while **preserving the user's tag**, so e.g. `alpine:3.19` stays addressable by tag and dedups by tag.
- The pulled image **config** is the source of truth for the recorded platform: `buildImage` persists the manifest's real `os/arch[/variant]` (not the requested value) and fails the build if a non-empty request does not match the manifest. A platformless (locally built) image with no request falls back to the host platform.
- Boot-by-tag auto-pull threads the instance's `Platform` so it fetches the right architecture rather than defaulting to the host.

## What was implemented (deviations from the original design)

Two decisions were taken during implementation, agreed with the user:

- **No user-facing emulation flag.** The original design proposed an
  `enable_rosetta`/`--rosetta` API toggle. Instead, emulation is *automatic*:
  `createInstance` compares the resolved image architecture to the host and, when
  they differ, enables Rosetta on `vz`/Apple-silicon hosts (`deriveEnableRosetta`).
  On any other host an emulated image is rejected with an emulation-agnostic
  error (`running amd64 images requires an emulation-capable host; this host is
  <goos>/<goarch>`). This mirrors Docker Desktop, where Rosetta is a host setting
  and never a per-container flag. There is no runtime capability probe — if
  Rosetta is not installed the shim already fails with a clear, actionable error.
- **`platform`, not `architecture`.** The user-facing surface is Docker's
  `os/arch[/variant]` platform string rather than a bare architecture, for Docker
  parity and forward-compatibility with non-Linux guests.

## Platform constraints & edge cases

- **Apple Silicon only.** Rosetta-for-Linux is an arm64-host feature. The bindings are under `//go:build darwin && arm64` (`shared_directory_arm64.go:1`); on Intel macOS the no-op stub errors if Rosetta is requested, mirroring Tart's `#elseif arch(x86_64)` empty return (`Sources/tart/Commands/Run.swift:748-751`). hypeman's `vz` snapshot support is already arm64-gated (`lib/hypervisor/vz/client.go:87`), so this aligns with existing assumptions.
- **macOS 13.0 minimum; 14.0 for caching.** `NewLinuxRosettaDirectoryShare` requires macOS 13 (`shared_directory_arm64.go:66`); on macOS 11/12 `LinuxRosettaDirectoryShareAvailability()` returns `NotSupported` (`shared_directory_arm64.go:117-119`) and we error. The translation-cache options require macOS 14 (`shared_directory_arm64.go:88`, `:154`). hypeman's stated floor is macOS 11 (`README.md`), so Rosetta is a capability available on a *subset* of supported hosts; the availability check is the gate.
- **Rosetta must be installed.** `availability == NotInstalled` is a distinct, actionable error. We surface the `softwareupdate --install-rosetta` hint (matching Tart's help text, `Sources/tart/Commands/Run.swift:151`) rather than auto-installing inside the shim.
- **`binfmt_misc` interpreter resolution across chroot.** Covered above: the `F` flag pins the interpreter fd at registration, so the subsequent `chroot("/overlay/newroot")` (`mode_exec.go:33`) does not break dispatch. Without `F`, dispatch would fail post-chroot.
- **APFS / volume boundaries.** The Rosetta share is a synthetic, framework-managed directory, not a user filesystem path, so it is unaffected by APFS volume layout or the user's `data_dir` location. This is simpler than a general `--dir` virtio-fs share (Tart's `Sources/tart/Commands/Run.swift:162`), which must respect host path boundaries.
- **virtio-fs / vsock device budget.** The Rosetta device is one additional virtio-fs device. `vz` already attaches block, network, entropy, console, vsock, and balloon devices (`cmd/vz-shim/vm.go`); one virtio-fs device is within Virtualization.framework limits and does not collide with the existing devices.
- **Snapshot/restore device parity.** `vz` restore requires the restored config's devices to match the saved machine state (`lib/hypervisor/vz/fork.go:65-67`). Because `EnableRosetta` is persisted in the manifest and replayed verbatim on restore (`lib/hypervisor/vz/starter.go:148`), a Rosetta-enabled VM restores with the Rosetta device present and a non-Rosetta VM restores without it — parity holds. A VM snapshotted *without* Rosetta cannot be restored *with* it (and vice-versa) without breaking machine-state compatibility; this matches the existing prohibition on mutating device identity during fork.
- **Performance.** Rosetta translates ahead-of-time on first execution and caches per-process; it is materially faster than full userspace CPU emulation but slower than native arm64. amd64 binaries that hammer x86-specific syscalls or unusual instruction sequences may still hit unsupported paths and fault — Rosetta-for-Linux does not guarantee 100% ISA coverage.

## Testing plan

- **Unit (host config, no entitlements/hardware):** table tests for `buildShimConfigFromVMConfig` and `buildGuestConfig` asserting `EnableRosetta` propagates from `hypervisor.VMConfig` / `inst` into `ShimConfig` / `vmconfig.Config`. These run on Linux CI (the config packages are not `darwin`-gated except `shimconfig`, which already carries `//go:build darwin`; the propagation test for `ShimConfig` runs on the macOS job).
- **Unit (guest registration logic):** factor the `binfmt_misc` rule string into a pure function (`rosettaBinfmtRule(interp string) string`) and assert the exact `:rosetta:M::<magic>:<mask>:<interp>:OCF` output, including that the interpreter path is interpolated and the `F` flag is present. No mounts required.
- **Build-tag matrix:** `GOOS=darwin GOARCH=arm64 go build ./cmd/vz-shim/...` compiles the arm64 Rosetta file. `darwin/amd64` is intentionally not built: Code-Hex/vz v3.7.1 itself does not compile for `darwin/amd64` (the generated `virtualmachinestate_string.go` references arm64-only `VirtualMachineState*` consts), and `cmd/vz-shim` imports vz unconditionally, so the package is arm64-only regardless of the `!arm64` stub. The stub exists for type-checking parity with the existing `save_restore_unsupported.go`, not to produce a working Intel-macOS binary.
- **Host availability gating (macOS arm64 runner):** with Rosetta uninstalled, assert `configureDirectorySharing` returns the `NotInstalled` error; with it installed, assert the device is attached and `vmConfig.Validate()` (`cmd/vz-shim/vm.go:89`) passes.
- **Unit (auto-Rosetta derivation, Linux CI):** `deriveEnableRosetta` is a pure function of (image-needs-emulation, hypervisor); a table test asserts host-native images never enable Rosetta, an emulated image auto-enables on `vz`/Apple-silicon and is rejected (`ErrInvalidRequest`) elsewhere, and an emulated image on a non-vz hypervisor is always rejected. Paired with `lib/images` platform unit tests (`ParsePlatform`/`Normalize`/validation, the `imageMetadata`↔`Image` platform round-trip with the empty→host default, and `resolveManifestPlatform` recording the manifest platform).
- **End-to-end (macOS arm64 runner, Rosetta installed):** `TestVZRosettaImageX86` (`lib/instances/rosetta_image_darwin_test.go`) pulls a real **amd64** alpine image (`Platform: linux/amd64`) and creates the instance with **no** emulation flag; Rosetta auto-enables because the image is amd64 on the arm64 host. It asserts `img.Platform == "linux/amd64"`, then execs a real on-disk amd64 ELF from the image (`/bin/busybox cat /etc/alpine-release`) so a genuine x86-64 binary dispatches through Rosetta — `uname -m` is unreliable under Rosetta, so the assertion is on command output. Shim and guest serial logs are dumped on failure. Where Rosetta is not installed the test skips. The earlier injected-ELF probe test was removed: with auto-only Rosetta an arm64 image never gets Rosetta, so forcing Rosetta on a same-arch image no longer has a user path, and the real-image E2E covers the same ground.
- **Snapshot/restore E2E:** snapshot a Rosetta-enabled VM, restore it, and assert amd64 exec still works post-restore (manifest carries `EnableRosetta`, device parity preserved).
- **Kernel-config assertion:** boot-time check that `virtiofs` is a known filesystem and `binfmt_misc` is mountable; fail the image-validation test if the shipped `vz` guest kernel lacks `CONFIG_VIRTIO_FS` / `CONFIG_FUSE_FS` / `CONFIG_BINFMT_MISC`.

## Risks & alternatives considered

- **Risk: guest kernel lacks virtio-fs or binfmt_misc.** Mitigation: kernel-config assertion in CI plus non-fatal degradation at runtime. Without the kernel options the feature simply reports an error and arm64 workloads continue.
- **Risk: `binfmt_misc` rule collides with an existing handler.** If the image's userspace (e.g. systemd's `binfmt.d`) already registered a `:rosetta:` rule, the live registration write returns `EEXIST`. Mitigation (implemented): `registerBinfmt` treats `EEXIST` as already-registered and continues.
- **Risk: systemd flushes the live rule.** `systemd-binfmt.service` runs `disable_binfmt()` (writes `-1` to `/proc/sys/fs/binfmt_misc/status`), flushing *all* rules — including our live `:rosetta:` rule — before re-applying only the `binfmt.d` drop-ins. The `F` flag pins the interpreter fd but does not protect the registration from this explicit flush. Mitigation (implemented): in systemd mode `init` also writes `/usr/lib/binfmt.d/rosetta.conf` into the overlay rootfs, so `systemd-binfmt` re-registers the handler. The interpreter is mounted under `/opt/hypeman/rosetta` (inside the overlay rootfs, not `/run`) so its guest-absolute path survives the chroot and is not shadowed by the `/run` tmpfs that systemd remounts during boot; the drop-in references that path.
- **Risk: restore-compatibility regressions.** Adding any device interacts with `vz`'s machine-state parity requirement (`lib/hypervisor/vz/fork.go:65`). Mitigation: persist and replay `EnableRosetta` from the manifest so a restored VM rebuilds the identical device set; covered by a restore E2E test.
- **Alternative: userspace emulation (QEMU user-mode / box64) registered via binfmt.** Works on any host (not Apple-Silicon-gated) but is materially slower and adds a binary to ship and maintain inside guests. Rosetta is the right default on Apple Silicon; userspace emulation could be a separate, host-agnostic fallback later but is out of scope here.
- **Alternative: expose a Tart-style `--rosetta=TAG` with manual guest registration.** Rejected: contradicts hypeman's programmatic, ephemeral-instance model. Automating the guest side is the whole point of the divergence.
- **Alternative: bake the `binfmt_misc` rule into the image rootfs.** Rejected: it would require every image to carry a Rosetta-aware `binfmt.d` entry and assume the share is mounted, coupling image content to host capability. Keeping registration in `init` makes it host-driven and image-agnostic.
- **Alternative: auto-install Rosetta from the shim via `LinuxRosettaDirectoryShareInstallRosetta()`.** Rejected for the hot path: a large blocking download inside per-VM startup. Better surfaced as an operator action (Rollout).

## Rollout / milestones

1. **Config plumbing + host device (no guest changes).** Add the fields and `configureDirectorySharing`; behind `EnableRosetta`, default off. Verify a Rosetta-enabled VM boots (even before guest registration, the device attaches and validates). Land build-tag matrix tests.
2. **Guest registration.** Add `lib/system/init/rosetta.go` and the Phase 6.5 hook; land the rule-string unit test and the amd64 hello-binary E2E.
3. **Guest kernel packaging.** Ensure the shipped `vz` guest kernel enables `CONFIG_VIRTIO_FS` / `CONFIG_FUSE_FS` / `CONFIG_BINFMT_MISC`; add the CI config assertion.
4. **API + docs.** Surface the Docker-style `platform` field on the image/instance API (no user-facing emulation flag); derive Rosetta automatically from the image vs. host architecture; regenerate OpenAPI types; document the host prerequisite (`softwareupdate --install-rosetta`) and the Apple-Silicon-only constraint.
5. **Operator preflight (optional).** A one-time host check/installer that calls `LinuxRosettaDirectoryShareInstallRosetta()` out of band so per-VM startup never blocks on installation.
6. **Follow-up: shared translation cache.** On macOS 14+, configure the Rosetta share with `SetOptions(NewLinuxRosettaUnixSocketCachingOptions(path))` or `NewLinuxRosettaAbstractSocketCachingOptions(name)` (`shared_directory_arm64.go:87`, `:153`, `:201`) so first-run translation results are cached. Because hypeman forks VMs from a snapshot (`lib/hypervisor/vz/fork.go`), a cache directory placed under a stable host path (e.g. derived from `paths.DataDir()`, `lib/paths/paths.go:20`) and shared read-mostly across forks could let forked VMs reuse a warm translation cache, reducing first-exec latency for amd64 workloads. This needs careful design around cache invalidation, concurrent fork access, and the macOS 14 floor, and is intentionally deferred.

## Open questions

- **Cache sharing semantics across forks.** Is a single shared Rosetta cache directory safe for concurrent read/write across many forked VMs, or must each fork get a copy-on-write view? The macOS 14 caching API communicates over a Unix/abstract socket to a translation daemon (`shared_directory_arm64.go:137`, `:184`); how that interacts with hypeman's fork-from-snapshot model needs prototyping before milestone 6.
- **Auto-install policy.** Should a Rosetta-enabled instance request that hits `NotInstalled` fail hard (current proposal) or trigger a one-time background install at the server level? Failing hard is simpler and predictable; an operator preflight may be the better ergonomics.
- **Duplicate-registration handling.** Resolved: the live registration tolerates `EEXIST` (an image-shipped `:rosetta:` rule is left in place), and in systemd mode `init` writes its own `binfmt.d` drop-in so `systemd-binfmt` re-registers Rosetta after the boot-time flush. An image that ships a *different* x86-64 handler under another name is left untouched; the two coexist as distinct `binfmt_misc` entries.
- **Tag constant vs. configurable.** A fixed `rosetta` tag is proposed for safety. Is there any multi-share scenario (e.g. a future general `--dir` virtio-fs feature, cf. Tart `Sources/tart/Commands/Run.swift:162`) that would require the Rosetta tag to be configurable to avoid collisions?
- **Guest kernel ownership.** The repo has no in-tree kernel build target; confirming where the `vz` guest kernel `.config` is owned determines whether milestone 3 is a packaging change in this repo or an external artifact bump.
