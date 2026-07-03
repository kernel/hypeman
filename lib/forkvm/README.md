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

## Fork data copy behavior

- Guest directory copy is **sparse-only** for regular files.
- Copy uses `SEEK_DATA`/`SEEK_HOLE` to preserve sparse holes and avoid
  de-sparsifying large overlay images.
- If sparse seeking is unsupported on the underlying filesystem, fork fails
  with an explicit sparse-capability error (no dense-copy fallback).
- Non-essential runtime artifacts are skipped during copy:
  `logs/` subtree and runtime socket files.

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
- Standby forks hardlink the source's snapshot mem-file instead of copying it:
  fanout costs no memory I/O and every fork of a snapshot faults against one
  inode, so the kernel page cache (and the UFFD pager cache, keyed by the
  inherited snapshot cache key) is shared across siblings. Deleting the source
  is still safe immediately — unlink only drops a name; forks keep the inode
  alive via its link count.
- Sharing an inode is safe because Firecracker mmaps the mem-file MAP_PRIVATE
  (guest writes never reach the file) and the only file writer — the in-place
  diff-snapshot merge on standby — first replaces any mem-file with `nlink > 1`
  with a private copy (reflink-cloned where the filesystem supports FICLONE,
  sparse-copied otherwise). A fork therefore becomes fully independent at its
  first standby, and a source that standbys while forks still share its base
  unshares the same way instead of mutating memory a fork reads.
- When the Firecracker snapshot memory backend is configured as UFFD, UFFD is
  used as a one-shot acceleration for the first restore of a newly forked
  standby snapshot.
- Subsequent direct restores of that same fork use Firecracker's normal
  file-backed memory backend. If that standby fork is itself forked again, the
  new child gets its own one-shot UFFD restore.
- This keeps UFFD on the high-fanout path where shared snapshot cache is most
  useful, while preserving the normal Firecracker diff-snapshot lifecycle for
  per-instance standby/resume cycles.

## VZ (Virtualization.framework)

- Stopped-source fork is supported (directory clone, no snapshot rewrite).
- Standby-source fork is supported (snapshot copy + VZ manifest rewrite +
  restore).
- Running-source fork is supported (standby source -> fork from standby ->
  restore source).
- VZ fork preparation rewrites instance-local paths in serialized shim config:
  disks, kernel/initrd, serial log, control socket, vsock socket, shim log.
- VZ keeps snapshotted NIC identity unchanged during fork prep because
  save/restore validation can reject machine-state restore when NIC identity
  fields are mutated.
- For forked standby restores with networking, a fresh network allocation is
  applied post-restore via the generic restore networking flow.
- Vsock socket naming is resolved generically through hypervisor registration
  (`vz.vsock` for VZ), so no instance-layer VZ-specific branching is required.
- Vsock CID rewrites are not required for VZ fork flows because VZ routing is
  socket-path based.

## Operational constraints

- Writable attached volumes are rejected for fork to prevent concurrent
  cross-VM writes to the same backing data.
- If a post-fork target-state transition fails, the partially created fork is
  cleaned up rather than left orphaned.
