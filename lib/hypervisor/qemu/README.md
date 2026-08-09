# QEMU hypervisor

The `qemu` backend uses `q35` on amd64 and `virt` on arm64. These architecture-native standard boards are selected internally and are not public API options.

## `qemu-microvm`

The `qemu-microvm` backend uses QEMU's Linux amd64-only `microvm` board. Hypeman uses direct kernel boot, `ttyS0` serial logs, and virtio-mmio transport for disks, networking, vsock, and the optional balloon.

It cannot use PCI/VFIO/vGPU devices or hotplug memory. QEMU limits it to eight virtio-mmio devices; Hypeman counts rootfs, overlay, config disk, attached-volume disks (an overlay volume consumes two), network, vsock, and the optional balloon before starting QEMU.

A `qemu-microvm` standby snapshot or warm fork may restore only with the exact QEMU version that wrote its memory image. Hypeman records the running binary's version in `qemu-config.json` and checks it before restore. If QEMU changes, restore a stopped snapshot with `target_state: Stopped` and start it normally, or recreate the instance; an instance already in `Stopped` state can always cold-start. A stopped snapshot may switch between `qemu`, `qemu-microvm`, and other hypervisors; the target backend determines the internal QEMU board.

## Boot comparison

`./scripts/benchmark-qemu-machine-types.sh [samples]` is an opt-in, non-gating Linux/KVM benchmark. It reports p50/p95 `StartedAt` → `ProgramStartedAt` latency and QEMU RSS for equivalent q35 and microvm nginx guests.
