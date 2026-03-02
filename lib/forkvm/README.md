# VM Forking: Hypervisor Behavior

This document describes hypervisor-specific fork behavior and how fork is made
to work across implementations.

## Common fork model

- **Stopped source**: clone VM data and start a new VM from copied state.
- **Standby source**: clone data + snapshot artifacts, then adapt snapshot
  identity for the fork (paths, network, vsock behavior varies by hypervisor).
- **Running source**: transition source to standby, fork from that standby
  snapshot, then restore the source.

For networked forks, the fork gets a fresh host/guest identity (IP, MAC, TAP)
instead of reusing the source identity.

## Cloud Hypervisor

- Snapshot-based forks are supported by rewriting snapshot configuration before
  restore.
- Path rewrites are constrained to exact source-directory matches or source-dir
  path prefixes to avoid mutating unrelated values.
- Serial log path, vsock socket path, and network fields are updated for the
  fork.
- Vsock CID is intentionally kept stable for snapshot restore compatibility.
- Running-source fork works by standby -> fork -> restore source, with source
  and fork separated by rewritten runtime endpoints.

## QEMU

- Snapshot-based forks are supported by rewriting QEMU snapshot VM config.
- Rewrites are explicit and path-safe (source-dir exact/prefix replacement),
  applied to disk/kernel/initrd/serial/vsock socket paths.
- Kernel arguments are left unchanged (not blanket-rewritten), to avoid
  accidental mutation of non-path text.
- Network identity is updated in snapshot config for the fork.
- Vsock CID updates are supported for snapshot state, so running-source fork can
  rotate source CID when needed to avoid CID collision after restore.

## Firecracker

- Firecracker snapshot restore supports **network overrides** but does not
  expose a full snapshot-config rewrite surface for arbitrary embedded paths.
- To make standby/running fork work, fork preparation stores desired network
  override data and source->target data-directory mapping.
- During restore, the source data path is temporarily aliased to the fork data
  path so embedded snapshot paths resolve for the fork, then aliasing is
  cleaned up.
- Network override fields are supplied at snapshot load to bind the fork to its
  own TAP device.
- Vsock CID remains stable for snapshot-based flows.

## VZ (Virtualization.framework)

- Stopped-source fork is supported (directory clone, no snapshot rewrite).
- Standby and running-source fork are supported by rewriting the copied VZ
  snapshot manifest (`config.json`) for fork-local runtime paths.
- VZ fork preparation rewrites:
  - Instance-local paths in serialized shim config (disks, kernel/initrd,
    serial log, control/vsock sockets, shim log).
- VZ keeps snapshotted NIC identity unchanged during fork prep because
  save/restore validation can reject machine-state restore when device identity
  fields are mutated.
- Vsock CID rewrites are not required for VZ fork flows because VZ uses a
  per-VM Unix vsock proxy socket for routing.

## Operational constraints

- Writable attached volumes are rejected for fork to prevent concurrent
  cross-VM writes to the same backing data.
- If a post-fork target-state transition fails, the partially created fork is
  cleaned up rather than left orphaned.
