# RFC: Live memory resize for macOS (vz) VMs

## Summary

A running macOS (vz) VM cannot grow its usable RAM above the size it booted with; the only runtime lever Apple's Virtualization.framework exposes is the traditional memory balloon, which can *reclaim* memory down from boot size but never grow past it. This RFC proposes booting vz VMs at a configured **memory ceiling**, immediately ballooning them down to a smaller **baseline**, and then driving balloon deflate/inflate from the existing host-pressure controller so browser/workload VMs can absorb spikes up to the ceiling and give memory back under host pressure — all without a reboot and without statically pinning every VM at its peak footprint.

## Motivation

vz-backed VMs on macOS are commonly used for headless browser and CI-style workloads whose memory demand is spiky: idle most of the time, with short bursts that need several GB. Today hypeman has two unattractive options:

1. **Static over-provisioning** — size every VM at its worst-case peak. The peak is reserved (and, depending on guest page-init behavior, touched and resident) for the whole VM lifetime even while idle, so host RAM packing density is poor and admission rejects VMs that would have fit at their idle footprint.
2. **Balloon reclaim only** — the current `active_ballooning` controller (`lib/guestmemory/controller.go`) can take memory *away* from a guest under host pressure, but it cannot hand memory *back* above the boot size. A VM that booted at 2 GB is stuck at a 2 GB ceiling for life.

The goal is to make a vz VM's usable memory elastic between a baseline and a ceiling at runtime, so a VM can boot cheap (baseline), grow on demand toward the ceiling when its own workload needs it, and shrink back under host memory pressure. This helps anyone running many short-lived or bursty Linux guests on a fixed-RAM Apple Silicon host: higher density at idle, fewer admission rejections, and no per-VM reboot to change the working size.

This is explicitly scoped to vz. Cloud Hypervisor and Firecracker already expose true memory hotplug (`SupportsHotplugMemory`, `ResizeMemory`); vz does not, and the technique here is a vz-specific way to approximate it.

## Current behavior in hypeman

**Memory is fixed at VM-configuration time.** The shim builds the machine with a single memory size and never changes it:

- `cmd/vz-shim/vm.go:38` computes `memoryBytes := computeMemorySize(uint64(config.MemoryBytes))`.
- `cmd/vz-shim/vm.go:42` passes it to `vz.NewVirtualMachineConfiguration(bootLoader, vcpus, memoryBytes)`. This is the *only* place memory size is set; there is no runtime `SetMemorySize`.
- `cmd/vz-shim/vm.go:307-323` clamps the requested size into `[VirtualMachineConfigurationMinimumAllowedMemorySize(), VirtualMachineConfigurationMaximumAllowedMemorySize()]`.

**The boot size is the instance's `Size`.** `lib/instances/create.go:878` sets `MemoryBytes: inst.Size`, threaded into `ShimConfig.MemoryBytes` at `lib/hypervisor/vz/starter.go:165`. `HotplugBytes`/`HotplugSize` exist in the config (`lib/hypervisor/config.go:9`, `lib/instances/types.go:97`) but vz ignores them.

**The only runtime memory lever is the balloon, and it only reclaims.** When `EnableMemoryBalloon` is set, the shim attaches a `VZVirtioTraditionalMemoryBalloonDevice` (`cmd/vz-shim/vm.go:79-91`), gated on `EnableMemoryBalloon` / `RequireMemoryBalloon` (`lib/hypervisor/vz/shimconfig/config.go:36-37`), which are populated from the guest-memory policy features (`lib/hypervisor/vz/starter.go:170-171`, `lib/instances/create.go:925-932`, `lib/guestmemory/policy.go:79-92`).

The balloon control plane:

- `cmd/vz-shim/server.go:48-50` — `balloonRequest{ TargetGuestMemoryBytes int64 }`.
- `cmd/vz-shim/server.go:62-63` — `GET`/`PUT /api/v1/vm.balloon`.
- `cmd/vz-shim/server.go:212-250` — handlers call `device.GetTargetVirtualMachineMemorySize()` / `device.SetTargetVirtualMachineMemorySize(...)`.
- `cmd/vz-shim/server.go:268-275` — `getTraditionalBalloonDevice()` finds the device via `vz.AsVirtioTraditionalMemoryBalloonDevice`.
- Client side: `lib/hypervisor/vz/client.go:271-290` (`SetTargetGuestMemoryBytes` / `GetTargetGuestMemoryBytes`), with `SupportsBalloonControl: true` and `SupportsHotplugMemory: false`; `ResizeMemory`/`ResizeMemoryAndWait` return `hypervisor.ErrNotSupported` (`client.go:85-97, 263-269`).

**The pressure-driven controller already exists, but is reclaim-only and bounded by boot size.** `lib/guestmemory/controller.go`:

- `controller.go:136` lists eligible VMs via `Source.ListBalloonVMs`; `lib/providers/providers.go:303` reports `AssignedMemoryBytes: inst.Size + inst.HotplugSize` (so for vz, `inst.Size`, since `HotplugSize` is 0).
- `controller.go:168-186` clamps the current target into `[0, AssignedMemoryBytes]`, computes `protectedFloor = protectedFloorBytes(...)` (`planner.go:60-63`: `max(ProtectedFloorMinBytes, assigned * ProtectedFloorPercent/100)`), and sets `maxReclaimBytes = AssignedMemoryBytes - protectedFloor`.
- `controller.go:198` / `planner.go:84-97` compute `autoTarget` only under `HostPressureStatePressure`; under no pressure the target is 0 (no reclaim). The plan never raises a guest above `AssignedMemoryBytes` (`controller.go:260-261` clamp to `[protectedFloor, AssignedMemoryBytes]`).

Host pressure comes from `lib/guestmemory/pressure_darwin.go`: `vm_stat` + `sysctl hw.memsize` for total/available, and `memory_pressure -Q` for a stressed flag (`pressure_darwin.go:35-64`), reduced to a two-state machine `healthy ⇄ pressure` by watermarks (`planner.go:65-82`).

The config surface is already wired but disabled by default — `config.example.darwin.yaml:42-51` (`active_ballooning.enabled: false`, `poll_interval`, `pressure_high/low_watermark_available_percent`, `protected_floor_percent`, `protected_floor_min_bytes`, `min_adjustment_bytes`, `per_vm_max_step_bytes`, `per_vm_cooldown`), plus `kernel_page_init_mode`, `reclaim_enabled`, `vz_balloon_required` (`config.example.darwin.yaml:39-41`). The doc even states the current limitation outright: "CPU/Memory Hotplug — Resize operations not supported" (`config.example.darwin.yaml:163-164`).

**Net:** today a vz VM is elastic only *downward* from its boot size, and only under host pressure. There is no way to give it more than it booted with.

## Prior art: Tart

Tart is an open-source VM toolset built on the same Virtualization.framework. It is a useful comparison point because it has already settled the question of how to size vz memory through the framework's API — and it is deliberately conservative about it.

What Tart does:

- **Memory is a static config value with a minimum-resources floor.** `Sources/tart/VMConfig.swift:63-64` stores `memorySizeMin` and a `private(set) memorySize`; the initializer seeds `memorySize = memorySizeMin` (`VMConfig.swift:85`). `setMemory(memorySize:)` (`VMConfig.swift:178-190`) refuses any value below `memorySizeMin` (for darwin guests) and below `VZVirtualMachineConfiguration.minimumAllowedMemorySize`, throwing `LessThanMinimalResourcesError`. The same floor pattern guards CPU count (`VMConfig.swift:164-176`).
- **It is applied at the next boot, not live.** `setMemory` is reachable only from the `tart set` CLI command (`Sources/tart/Commands/Set.swift:48-50`), which mutates the persisted config; the value is consumed when a configuration is built: `Sources/tart/VM.swift:334` assigns `configuration.memorySize = vmConfig.memorySize` exactly once while constructing the `VZVirtualMachineConfiguration` (`VM.swift:326-334`). Tart does **not** attach a balloon device or call any runtime memory API. There is no live resize.
- **Per-OS defaults.** Linux VMs default to a 4 GiB minimum (`VM.swift:241`); macOS VMs derive the minimum from `requirements.minimumSupportedMemorySize` (`VM.swift:190`).

What hypeman should adopt:

- **The minimum-resources floor guard.** Tart's `memorySizeMin` is exactly hypeman's `protected_floor` concept (`active_ballooning.protected_floor_*` → `protectedFloorBytes`, `planner.go:60-63`). hypeman should keep enforcing a hard lower bound on usable guest memory so a guest is never ballooned below a size it can function at, and should likewise clamp the *boot ceiling* into the framework's `[minimumAllowedMemorySize, maximumAllowedMemorySize]` range exactly as `computeMemorySize` already does (`vm.go:307-323`) and as Tart does in `setMemory`.
- **Treating "the number you give the VZ configuration" as the hard ceiling.** Tart's `configuration.memorySize` is the immovable boot size; hypeman's `NewVirtualMachineConfiguration` argument is the same. This RFC's central move — choose that number deliberately as a *ceiling* rather than a *baseline* — only works because both projects agree this value is fixed for the VM's lifetime.

Where hypeman should diverge:

- **Tart sizes once and reboots to change it; hypeman wants live elasticity.** hypeman already runs a server + vz-shim model with a balloon device attached and a host-pressure controller (`lib/guestmemory/`). The differentiation is the live, pressure-driven resize loop: boot at the ceiling, balloon down to a baseline, and let the controller move the balloon target up (toward the ceiling) on guest demand and down (toward the floor) under host pressure. Tart has no equivalent because it targets interactive macOS guests driven by a human at the CLI, not a fleet of headless Linux guests packed onto one host.

## Proposed design

### The hard constraint, stated precisely

On vz there are exactly two memory quantities:

- **Boot size** — the `memorySize` passed to `NewVirtualMachineConfiguration` (`vm.go:42`). Fixed for the VM's lifetime. Bounded by `VirtualMachineConfigurationMaximumAllowedMemorySize()` (`Code-Hex/vz v3` `configuration.go:312`), which on Apple Silicon is a function of host RAM.
- **Balloon target** — `VZVirtioTraditionalMemoryBalloonDevice.SetTargetVirtualMachineMemorySize(uint64)` (vz binding `memory_balloon.go:129-149`). This is the guest-*visible* size after the balloon inflates/deflates. The binding's own doc comment is explicit: "The target memory size must be less than the total memory configured for the virtual machine."

So **usable guest memory = balloon target, and `floor ≤ target ≤ boot_size`**. Inflating the balloon (lowering the target) reclaims; deflating it (raising the target) returns memory — but only up to `boot_size`. There is no API to raise `boot_size` at runtime. I confirmed there is no `SetMemorySize`/resizable-memory/`maximumMemory` symbol in `Code-Hex/vz v3 v3.7.1`: the only memory mutators are the balloon get/set above and the config-time `NewVirtualMachineConfiguration(bootLoader, cpu, memorySize)`. The traditional balloon itself is gated on macOS 11+ (`memory_balloon.go:39-44`).

**Therefore "grow above the size the guest currently sees" is achievable on vz *only if the boot size was set high enough in advance.*** This RFC makes that the explicit mechanism: **boot at the ceiling, run at a baseline via the balloon.**

### Mechanism: boot-at-ceiling, balloon-to-baseline

At create time, a vz VM is given two numbers instead of one:

- `baseline` = the size the guest should normally run at (what `inst.Size` means today).
- `ceiling` = the maximum the guest may ever grow to live (≥ baseline, ≤ host-derived max).

The shim boots the machine at `ceiling`, then, before/at the moment the guest starts using memory, inflates the balloon so the guest sees `baseline`. From then on the controller can move the target anywhere in `[floor, ceiling]`:

- guest demand rises → deflate balloon → guest sees more, up to `ceiling`, no reboot;
- host under pressure → inflate balloon → guest gives memory back, down to `floor`.

Because vz/Linux memory backing is lazy (pages are host-resident only once touched, which is exactly why `config.example.darwin.yaml` ships `kernel_page_init_mode` and why `assertLowIdleVZHostMemoryFootprint` in `lib/instances/guestmemory_darwin_test.go:143-166` asserts a low idle RSS), booting at a larger ceiling does **not** make the host pay for the ceiling while the guest sits at baseline. The cost of a higher ceiling is address-space/bookkeeping, not resident RAM, as long as the balloon holds the guest down and the guest doesn't touch the ballooned pages. This is the property that makes the technique pay off.

### Config-time changes (vz-shim)

Add a `MemoryCeilingBytes` to `ShimConfig` and an initial balloon target to apply at boot. `MemoryBytes` keeps its current meaning (baseline). When the ceiling is unset or equals the baseline, behavior is identical to today.

```go
//go:build darwin

// lib/hypervisor/vz/shimconfig/config.go
type ShimConfig struct {
	// Compute resources
	VCPUs       int   `json:"vcpus"`
	MemoryBytes int64 `json:"memory_bytes"` // baseline: guest's normal running size

	// MemoryCeilingBytes is the boot-time memory size. The VM is created at this
	// size and immediately ballooned down to MemoryBytes so the guest can later
	// grow up to the ceiling without a reboot. Zero means "no ceiling" (boot at
	// MemoryBytes), preserving today's behavior.
	MemoryCeilingBytes int64 `json:"memory_ceiling_bytes,omitempty"`

	// ... existing fields unchanged ...
	EnableMemoryBalloon  bool `json:"enable_memory_balloon,omitempty"`
	RequireMemoryBalloon bool `json:"require_memory_balloon,omitempty"`
}
```

In `createVM` (`cmd/vz-shim/vm.go`), boot at the ceiling when one is set:

```go
//go:build darwin

// cmd/vz-shim/vm.go (createVM)
bootBytes := uint64(config.MemoryBytes)
if config.MemoryCeilingBytes > config.MemoryBytes {
	bootBytes = uint64(config.MemoryCeilingBytes)
}
memoryBytes := computeMemorySize(bootBytes) // existing min/max clamp, vm.go:307-323
// ...
vmConfig, err := vz.NewVirtualMachineConfiguration(bootLoader, vcpus, memoryBytes)
```

A ceiling requires the balloon. Boot-at-ceiling without a balloon would leave the guest permanently at the ceiling, defeating the point and wasting RAM the moment the guest touches it. So when `MemoryCeilingBytes > MemoryBytes`, the shim must treat the balloon as required regardless of `RequireMemoryBalloon`:

```go
//go:build darwin

// cmd/vz-shim/vm.go (balloon block, replacing vm.go:79-91)
ceilingActive := config.MemoryCeilingBytes > config.MemoryBytes
if config.EnableMemoryBalloon || ceilingActive {
	balloonConfig, err := vz.NewVirtioTraditionalMemoryBalloonDeviceConfiguration()
	if err != nil {
		if config.RequireMemoryBalloon || ceilingActive {
			return nil, nil, fmt.Errorf("create memory balloon device: %w", err)
		}
		slog.Warn("memory balloon unavailable, continuing without balloon", "error", err)
	} else {
		vmConfig.SetMemoryBalloonDevicesVirtualMachineConfiguration(
			[]vz.MemoryBalloonDeviceConfiguration{balloonConfig},
		)
	}
}
```

### Applying the baseline at boot

The balloon target must be set after the VM is running (the device only exists on the live machine — `s.vm.MemoryBalloonDevices()`, `server.go:269`). The shim's startup path (after `vm.Start`, where it begins serving the control API) should, when a ceiling is active, inflate the balloon to the baseline once the guest is up:

```go
//go:build darwin

// cmd/vz-shim: applied after the VM reaches Running, before/at first guest use.
func applyInitialBalloonTarget(vm *vz.VirtualMachine, cfg *shimconfig.ShimConfig) {
	if cfg.MemoryCeilingBytes <= cfg.MemoryBytes {
		return // no ceiling; nothing to do
	}
	for _, dev := range vm.MemoryBalloonDevices() {
		if t := vz.AsVirtioTraditionalMemoryBalloonDevice(dev); t != nil {
			t.SetTargetVirtualMachineMemorySize(uint64(cfg.MemoryBytes))
			slog.Info("ballooned guest to baseline",
				"boot_bytes", cfg.MemoryCeilingBytes, "baseline_bytes", cfg.MemoryBytes)
			return
		}
	}
}
```

There is an unavoidable window between `Running` and the balloon settling at `baseline` during which the guest could touch up to `ceiling` of pages. In practice the guest kernel comes up, sees the full ceiling briefly, and the balloon driver claims pages back as it inflates; the guest's own early-boot footprint is far below the ceiling. We accept this transient. (See Open questions for whether to gate "ready" on the balloon reaching baseline.)

### Threading ceiling through the control plane

`AssignedMemoryBytes` in the controller is the upper clamp for every balloon decision (`controller.go:168, 260-261`). To let the controller grow a guest above its baseline, `AssignedMemoryBytes` for a ceiling-enabled vz VM must be the **ceiling**, not `inst.Size`. The cleanest place is the `Source`:

```go
// lib/providers/providers.go (guestMemoryInstanceSource.ListBalloonVMs, ~line 298)
assigned := inst.Size + inst.HotplugSize
if inst.MemoryCeilingBytes > assigned {
	assigned = inst.MemoryCeilingBytes // vz boot-ceiling acts as the live max
}
vms = append(vms, guestmemory.BalloonVM{
	ID:                  inst.Id,
	Name:                inst.Name,
	HypervisorType:      inst.HypervisorType,
	SocketPath:          inst.SocketPath,
	AssignedMemoryBytes: assigned,
})
```

With that one change, the existing controller already does the right thing in both directions, because nothing in it assumes the guest started at `AssignedMemoryBytes`:

- **Grow on guest demand.** This is the one genuinely new policy input. The controller today only acts under *host* pressure (`planner.go:84-86` returns 0 target when healthy). To grow a guest toward its ceiling we need a *guest*-demand signal. Two options, in preference order:
  - (a) **Balloon-stat-driven** (preferred if available): read the guest's actual memory usage and deflate when the guest is near its current target. vz's traditional balloon does not surface guest free-memory stats through `Code-Hex/vz v3 v3.7.1` (only get/set target — `memory_balloon.go`), so this requires either a future vz stat API or an in-guest agent reporting `MemAvailable` over vsock. hypeman already has a vsock exec/agent channel (`cmd/vz-shim/server.go:327` `ServeVsock`).
  - (b) **Headroom policy** (no new guest signal): keep a configured slack between target and the guest's working set by deflating in steps while the host is healthy and the guest is above a utilization threshold, capped by `per_vm_max_step_bytes` and `per_vm_cooldown` (already enforced — `controller.go:243-258`). Until a guest signal exists, deflate-to-ceiling is gated behind an opt-in and rate-limited.
- **Shrink under host pressure.** Unchanged and already correct: under `pressure`, `automaticTargetBytes` computes the reclaim needed to reach the low watermark (`planner.go:84-97`), `planGuestTargets` distributes it across VMs proportional to `maxReclaimBytes` (`planner.go:13-58`), and per-VM clamps keep each guest ≥ `protectedFloor` (`controller.go:260-261`). A ceiling VM simply has more reclaimable headroom (`maxReclaimBytes = ceiling - floor`).

The grow path is the only new controller logic. It belongs in `planner.go` as a healthy-state target, kept conservative and opt-in:

```go
// lib/guestmemory/planner.go — new: target when host is healthy.
// Returns a per-VM "grow toward ceiling" target only when explicitly enabled
// and the guest demand signal (if any) warrants it. Steps and cooldown are
// still applied by the controller (controller.go:243-258).
func growthTargetBytes(cfg ActiveBallooningConfig, c candidateState, demand guestDemand) int64 {
	if !cfg.GrowOnDemandEnabled {
		return c.currentTargetGuestBytes // no change: today's behavior
	}
	// Grow toward AssignedMemoryBytes (the ceiling) while the guest is using a
	// high fraction of its current target and the host is healthy.
	if demand.utilizationPercent < cfg.GrowUtilizationPercent {
		return c.currentTargetGuestBytes
	}
	return c.vm.AssignedMemoryBytes
}
```

`hypervisor.Capabilities` gets a flag so callers can tell a ceiling VM apart from a fixed one without inspecting config:

```go
// lib/hypervisor/hypervisor.go (Capabilities)
// SupportsLiveMemoryCeiling indicates the VM was booted at a ceiling above its
// baseline and its usable memory can grow above baseline at runtime via the balloon.
SupportsLiveMemoryCeiling bool
```

For vz this is `true` exactly when a ceiling is configured; `SupportsHotplugMemory` stays `false` (we are not hotplugging — we are deflating a pre-sized balloon). No other hypervisor sets it. Like the merged `EnableRosetta` flag, this is derived internally from config rather than surfaced as a user-facing request knob — `EnableRosetta` states that contract directly ("Derived internally … not a user-facing field", `lib/instances/types.go:163-166`), and the ceiling-implies-balloon requirement below follows the same derive-from-config approach.

### Why this reuses the existing machinery rather than adding knobs

Every quantity the design needs already has a config knob and a code path:

- floor → `protected_floor_percent` / `protected_floor_min_bytes` → `protectedFloorBytes` (`planner.go:60-63`)
- shrink trigger → `pressure_high/low_watermark_available_percent` + `memory_pressure -Q` (`pressure_darwin.go`, `planner.go:65-97`)
- step/cooldown → `per_vm_max_step_bytes` / `per_vm_cooldown` (`controller.go:243-258`)
- min change → `min_adjustment_bytes` (`controller.go:243-245`)

The only genuinely new config is the ceiling itself and the optional grow-on-demand policy.

## Configuration / API changes

### Instance creation

Add an optional per-instance memory ceiling. It defaults to the baseline (no ceiling), so existing callers are unchanged.

```go
// lib/instances/types.go (CreateInstanceRequest and stored instance)
// MemoryCeilingBytes, when greater than Size, boots a vz VM at this size and
// balloons it down to Size, allowing usable memory to grow up to the ceiling
// at runtime. vz only. Ignored by hypervisors with real hotplug.
MemoryCeilingBytes int64 // 0 = no ceiling (boot at Size)
```

Threaded: `CreateInstanceRequest.MemoryCeilingBytes` → stored on the instance → `hypervisor.VMConfig` (new field) → `buildShimConfigFromVMConfig` (`starter.go:161-188`) → `ShimConfig.MemoryCeilingBytes`. This is the same path the `Platform`/`EnableRosetta` fields already take, so it is proven plumbing rather than new machinery: the user-facing `Platform` rides `CreateInstanceRequest` → instance (`lib/instances/types.go:252`, `:93`), and the *derived* `EnableRosetta` flag is computed during create (`deriveEnableRosetta`, `lib/instances/rosetta.go:17`, called `create.go:119`), carried on `hypervisor.VMConfig` (`lib/hypervisor/config.go:36`), copied by `buildShimConfigFromVMConfig` (`starter.go:172`), and consumed as `ShimConfig.EnableRosetta` (`shimconfig/config.go:42`). `MemoryCeilingBytes` adds one field at each of those same hops. The ceiling is persisted on the instance so it survives standby/restore (the shim config is already round-tripped through the snapshot manifest — `shimconfig.SnapshotManifest`, `server.go:186-205`; the restore path rebuilds `ShimConfig` at `starter.go:148-156`).

Validation, mirroring Tart's `setMemory` floor guard (`VMConfig.swift:178-190`) and the existing `computeMemorySize` clamp (`vm.go:307-323`):

- `MemoryCeilingBytes == 0` → no ceiling.
- `0 < MemoryCeilingBytes ≤ Size` → reject (ceiling below baseline is meaningless).
- `MemoryCeilingBytes > VirtualMachineConfigurationMaximumAllowedMemorySize()` → reject (cannot boot that large; this is host-RAM dependent — see below).
- Ceiling implies balloon: creation fails fast if the balloon can't be attached (mirrors `RequireMemoryBalloon`).
- `Size` must still satisfy the protected floor.

### Server config (`config.example.darwin.yaml`)

Extend the existing `active_ballooning` block; do not introduce a new top-level section.

```yaml
hypervisor:
  memory:
    active_ballooning:
      enabled: false
      # ... existing watermarks / protected_floor / per_vm_* ...

      # Grow a guest's usable memory above its baseline toward its configured
      # boot ceiling while the host is healthy. Off by default. Requires the
      # instance to have been created with a memory ceiling above its size.
      grow_on_demand_enabled: false
      # Deflate toward the ceiling only when the guest is using at least this
      # percent of its current target. Ignored unless grow_on_demand_enabled.
      grow_utilization_percent: 85
```

Corresponding fields on `ActiveBallooningConfig` (`active_ballooning.go:20-34`) with defaults and `Normalize` clamps in the same style as the existing fields (`active_ballooning.go:37-88`):

```go
GrowOnDemandEnabled    bool
GrowUtilizationPercent int // clamp to (0,100); default 85
```

The doc comment in `config.example.darwin.yaml:163-164` ("CPU/Memory Hotplug — Resize operations not supported") should be updated to describe the actual situation: true hotplug is unsupported, but usable memory can be made elastic between a baseline and a boot ceiling via the balloon.

## Platform constraints & edge cases

- **macOS version.** The traditional memory balloon device is macOS 11+ (`memory_balloon.go:39-44`); the whole vz backend already requires it. No new minimum is introduced by ballooning itself. Snapshots remain macOS 14+ on Apple Silicon (`config.example.darwin.yaml:169-172`), which matters only for ceiling VMs that are also snapshotted (the manifest already carries the shim config, so the ceiling is preserved across restore).
- **Apple Silicon only.** vz Linux guests under hypeman target arm64 (`lib/instances/guestmemory_darwin_test.go:30-32`; `SupportsSnapshot = runtime.GOARCH == "arm64"`, `client.go:87`). No change.
- **Ceiling is bounded by host RAM, not by APFS or disk.** `VirtualMachineConfigurationMaximumAllowedMemorySize()` (`configuration.go:312`) returns a value derived from physical host memory; the framework rejects configurations above it at `Validate()` time (`vm.go:93`). Memory here is RAM-backed, so there is **no** disk-image or APFS-volume-boundary concern for the memory feature specifically — unlike disk resizing, which Tart explicitly documents as one-directional to avoid data loss (`Set.swift:34-37`). The ceiling math is purely "sum of VM ceilings vs. host RAM," and admission control should reason about the **baseline** for packing (since that's the resident cost at idle) while treating the **ceiling** as the worst case the balloon controller must be able to claw back under pressure.
- **Oversubscription risk.** Booting at the ceiling makes a guest *capable* of touching ceiling-many pages. If many guests simultaneously grow toward their ceilings while the host is healthy, then the host swings into pressure, the controller must reclaim fast enough. This is bounded by `per_vm_max_step_bytes` (reclaim is incremental) and the protected floor (a guest is never squeezed below a usable size). The honest failure mode: if aggregate demand exceeds host RAM faster than the balloon can inflate, the host swaps. Grow-on-demand is therefore off by default and rate-limited; the safe default deployment is "boot at ceiling, hold at baseline, only ever deflate toward ceiling under explicit opt-in."
- **Balloon refusal / partial inflation.** `SetTargetVirtualMachineMemorySize` is a request; the guest's balloon driver fulfills it asynchronously and may lag or partially comply (e.g. under guest memory pressure with `deflate-on-OOM` semantics). The controller already reads back the *target* (`GetTargetVirtualMachineMemorySize`, `server.go:224`) — note this is the target, not the achieved size, so accounting is target-based, same as today. No new guarantee is claimed about instantaneous compliance.
- **Lazy backing assumption.** The density win depends on the guest not touching ballooned pages. A guest configured with `kernel_page_init_mode: hardened` (`init_on_alloc=1 init_on_free=1`, `policy.go:64-76`) touches more pages on alloc/free; the `performance` mode preserves lazy host allocation. Ceiling VMs that care about density should run `performance` page-init, exactly the tradeoff the existing knob encodes.
- **Interaction with `inst.Size + inst.HotplugSize`.** For vz, `HotplugSize` is always 0 today; the `Source` change uses `max(Size+HotplugSize, MemoryCeilingBytes)` so it stays correct if hotplug is ever populated on another backend without affecting vz.

## Testing plan

Extend the existing darwin manual integration tests (gated by `requireGuestMemoryManualRun`, darwin, arm64 — `lib/instances/guestmemory_darwin_test.go:25-32`) and the unit tests for the controller/planner.

Unit (host-independent, run everywhere):

- `planner_test.go` (new cases) / extend `policy_test.go`: `growthTargetBytes` returns no-change when `GrowOnDemandEnabled` is false; grows to `AssignedMemoryBytes` only above `GrowUtilizationPercent`; never exceeds `AssignedMemoryBytes`; never goes below `protectedFloor`.
- `controller_test.go`: with `AssignedMemoryBytes` = ceiling and current target = baseline, a healthy host with grow enabled raises the target by at most `per_vm_max_step_bytes` per reconcile and respects `per_vm_cooldown` (the clamps at `controller.go:243-258` already exist; assert they bound the grow path too).
- `ActiveBallooningConfig.Normalize`: `GrowUtilizationPercent` clamps to `(0,100)` and defaults to 85 when unset/invalid.

Integration (darwin/arm64, manual), extending `lib/instances/guestmemory_darwin_test.go`:

- **Boot-at-ceiling.** Create a vz instance with `Size = 1 GiB`, `MemoryCeilingBytes = 4 GiB`. Assert `getVZVMInfo` reports a balloon device (`lib/instances/guestmemory_darwin_test.go:64-66`). Read `/proc/meminfo` `MemTotal` over the exec agent (`vzExecCommand`, used at `lib/instances/guestmemory_darwin_test.go:58-60`) and assert it reflects the *boot* size (~4 GiB) — the guest kernel sees the ceiling.
- **Balloon-to-baseline.** After startup, assert `GET /api/v1/vm.balloon` target ≈ 1 GiB and that guest `MemAvailable` shrinks accordingly, while host RSS of the shim stays low (reuse `assertLowIdleVZHostMemoryFootprint`, `lib/instances/guestmemory_darwin_test.go:143-166`) — proving the ceiling didn't cost resident host RAM at baseline.
- **Live grow.** `PUT /api/v1/vm.balloon` with target 4 GiB (or drive the controller with `GrowOnDemandEnabled` and a synthetic high-utilization signal); assert the guest's usable memory climbs toward 4 GiB *without a reboot* (no change in `getVZVMInfo` state transitions, instance not recreated).
- **Live shrink under pressure.** Reuse `assertActiveBallooningLifecycle` (`lib/instances/guestmemory_darwin_test.go:72`) with an injected `PressureSampler` (the controller already supports injection — `NewControllerWithSampler`, `active_ballooning.go:198`) reporting `Stressed: true`; assert the target drops toward the floor and never below `protectedFloor`.
- **Ceiling validation.** Unit-level: `CreateInstanceRequest` with ceiling ≤ size is rejected; ceiling above `VirtualMachineConfigurationMaximumAllowedMemorySize()` is rejected.

## Risks & alternatives considered

- **Risk: host swap under correlated growth.** Covered above; mitigated by off-by-default grow, per-VM step/cooldown, and protected floor. The conservative default (boot-at-ceiling + hold-at-baseline) carries the same host-RAM risk profile as today plus the address-space cost of the larger boot size.
- **Risk: relying on balloon honesty.** The target is a request, not a guarantee. We account on target (as today) and document that achieved size may lag. If a future `Code-Hex/vz` exposes balloon stats, the grow path should switch from the headroom heuristic to a measured signal.
- **Alternative: wait for real vz memory hotplug.** Cleanest if it ever ships, but `Code-Hex/vz v3 v3.7.1` has no resizable-memory API and Apple has not exposed one; this RFC needs no new framework support. If Apple adds hotplug, this design degrades gracefully — `SupportsHotplugMemory` flips to `true` and `ResizeMemory` (`client.go:263`) gets a real implementation, with the ceiling becoming the hotplug max.
- **Alternative: static over-provision + reclaim only (today).** Rejected: cannot grow above boot size; forces peak-sizing.
- **Alternative: stop-resize-restart (Tart's model).** Tart changes memory by editing config and rebooting (`Set.swift` → `VM.swift:334`). Correct and simple, but a reboot defeats "absorb a spike without losing the running workload," which is the whole motivation. We adopt Tart's floor guard but not its reboot-to-resize model.
- **Alternative: snapshot/restore at a larger size.** vz restore must use the same memory size as the saved machine state; you cannot restore a snapshot into a differently-sized VM. So snapshot/restore can't grow a live guest either. Ceiling-at-boot is the only live option.

## Rollout / milestones

1. **Plumb the ceiling, behave identically by default.** Add `MemoryCeilingBytes` to `ShimConfig`, `hypervisor.VMConfig`, `CreateInstanceRequest`, and the instance record; boot-at-ceiling + balloon-to-baseline in the shim; `Source` reports ceiling as `AssignedMemoryBytes`. With no ceiling set, output is byte-identical to today. Ship behind validation only; no controller behavior change yet.
2. **Shrink-aware ceiling.** Verify the existing pressure-driven reclaim correctly uses the larger headroom of ceiling VMs (it should, unchanged) and add the `SupportsLiveMemoryCeiling` capability + metrics labels distinguishing baseline/ceiling/target.
3. **Grow-on-demand (opt-in).** Add `growthTargetBytes`, `GrowOnDemandEnabled`, `GrowUtilizationPercent`; start with the headroom heuristic, rate-limited by existing step/cooldown. Default off.
4. **Measured grow signal (optional, follow-up).** If/when a guest memory signal is available (vsock agent reporting `MemAvailable`, or a future vz balloon-stat API), replace the heuristic with a measured trigger.

Each milestone is independently shippable; milestone 1 is inert by default and safe to land first.

## Open questions

- **Readiness gating.** Should instance "ready" wait until the balloon has actually settled at baseline (bounding the boot-to-baseline window where the guest could touch ceiling pages), or is best-effort inflate-after-Running sufficient? Gating adds startup latency; not gating accepts a brief high-watermark window.
- **Guest demand signal.** Is an in-guest agent reporting `MemAvailable` over the existing vsock channel acceptable, or should grow-on-demand stay purely host-driven (headroom heuristic) until Apple/`Code-Hex/vz` exposes balloon stats? This determines whether milestone 4 is ever needed.
- **Default ceiling policy.** Should there be a server-wide default ceiling multiple (e.g. ceiling = N × baseline) for vz instances that don't specify one, or must the ceiling always be explicit per instance? A default makes the feature usable without per-call changes but raises the oversubscription surface.
- **Admission accounting.** Should admission control reserve against baseline (best density) or some fraction between baseline and ceiling (safety margin for correlated growth)? This is a policy choice that trades density against swap risk and should be configurable.
- **Metrics.** Beyond target/baseline/ceiling per VM, do we want a host-level "aggregate ceiling vs. host RAM" oversubscription gauge to make the swap risk observable before it bites?
