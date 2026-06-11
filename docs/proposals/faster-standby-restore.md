# RFC: Faster macOS VM standby/restore (snapshot save/restore)

## Summary

Standby/restore of `vz` (Apple Virtualization.framework) VMs on Apple silicon was slow mainly because the guest-disk copy on macOS was a full sparse copy. That copy is the win: **Section C — an APFS `clonefile(2)` copy-on-write disk copy in `lib/forkvm` — landed in #276** and makes a same-volume `overlay.raw` clone metadata-only and effectively size-independent.

Two further optimizations were proposed and **evaluated, then found to be no-ops on `vz`** once the disk copy is `clonefile`-instant, so **neither is implemented**:

- **A. Compress `machine-state.vzm`.** Apple's `SaveMachineStateToPath` already omits zero/free guest pages, so the file is the high-entropy residual of live RAM. Measured on `vz`: zstd shrinks it ~0% and lz4 comes out net-larger, while compression burns CPU in the latency-critical paused window. No space win, real cost — dropped.
- **B. Overlap the machine-state save with the disk copy.** Once the disk copy is a `clonefile` CoW clone (~1 ms, size-independent), there is essentially nothing to overlap. Measured on a 2 GiB-overlay VM: serial save-then-copy standby ~445 ms vs the overlapped variant ~482 ms — within noise. The overlap added concurrency machinery (errgroup, subtree-prune-then-recopy, cross-leg failure handling) for no measurable benefit — dropped.

The shipped `vz` standby/snapshot/fork path is therefore the simple serial one: pause, save `machine-state.vzm`, copy the guest disk (CoW via `clonefile`), resume/shutdown.

A and B would still matter elsewhere, and the code for the cases where they pay off is retained:

- **Firecracker / Cloud-Hypervisor.** Their raw memory files (`memory-ranges` / `memory`) include zero pages and compress well, so the existing async snapshot-compression subsystem (`lib/instances/snapshot_compression.go`) is kept intact for them. It is simply never applied to `machine-state.vzm`.
- **Cross-volume / non-APFS fallback.** When `clonefile` can't serve a copy (different APFS volume → `EXDEV`, or non-APFS → `ENOTSUP`), `lib/forkvm` falls back to a `SEEK_DATA`/`SEEK_HOLE` sparse copy whose cost scales with disk size. In that regime overlapping the save with the copy (B) would again matter; it is not implemented because the common deployment (instance and snapshot dirs under one data root on one APFS volume) gets the `clonefile` fast path.

## Motivation

Standby reclaims host CPU and memory from idle VMs and restores them on demand. `lib/autostandby` drives this automatically: when an instance's inbound TCP flow count stays at zero for `idle_timeout`, the controller calls `StandbyInstance` (`lib/autostandby/controller.go`). The faster standby and restore are, the more aggressively idle reclaim can run without users noticing latency on the next request, and the cheaper it is to pack many guests onto a host.

The dominant `vz` cost was the disk copy. Snapshot and fork copy the guest directory (which contains `overlay.raw`) via `lib/forkvm`. On Linux this uses `FICLONE` reflink and is effectively instantaneous on CoW filesystems; on macOS it previously fell through to `SEEK_DATA`/`SEEK_HOLE` extent copying, reading and rewriting every allocated block. #276 wired APFS `clonefile(2)` into `lib/forkvm`, so a same-volume copy is CoW-instant.

This RFC targets headless Linux guests running under hypeman's server + `vz-shim` subprocess model.

## What shipped: Section C — CoW-clone the disk on APFS (`clonefile`), #276

This was the highest-leverage change for snapshot/fork latency on macOS.

`lib/forkvm` has a real darwin `clonefile(2)` implementation (`copy_reflink_darwin.go`), and the previous `!linux` stub was narrowed to `!linux && !darwin` (`copy_reflink_other.go`). `copyRegularFileReflink` removes any stale destination (`clonefile` fails `EEXIST` if it exists), clones with `unix.CLONE_NOFOLLOW | unix.CLONE_NOOWNERCOPY` — the latter drops owner/SUID/SGID/ACL metadata that has no business entering a fork — then re-applies the caller's mode with `os.Chmod`. On APFS this is a metadata-only copy-on-write clone: it consumes no extra space until blocks diverge and completes in roughly constant time regardless of file size.

Fallback is explicit and narrow. `isReflinkUnsupportedError` maps only volume/filesystem-capability signals — `ENOTSUP`, `EOPNOTSUPP`, `EXDEV` — to `ErrReflinkUnsupported`, so the reflink-first-then-sparse dispatcher (`lib/forkvm/copy.go`) flips its sticky `reflinkDead` flag and routes through `copyRegularFileSparse`. Everything else propagates. This preserves the invariant that fork copies are sparse-aware and never blindly desparsify (`lib/forkvm/README.md`). Coverage landed with the change (`copy_reflink_darwin_test.go`, plus `BenchmarkForkGuestDisk` and the end-to-end `TestVZForkSpeed`).

A multi-GiB `overlay.raw` clone is now sub-millisecond and metadata-only on a same-volume APFS layout, which removes the macOS disk-copy cost from snapshot/fork outright.

## Evaluated and not implemented

### A. Compress `machine-state.vzm` — no-op on `vz`

The vz Save/Restore API is path-based: `SaveMachineStateToPath` / `RestoreMachineStateFromURL` (Code-Hex/vz v3). The framework writes the file itself given a path — there is no `io.Writer` to interpose a compressor on, so compression would be a post-write pass over the complete file (and a pre-read pass on restore).

It does not pay off. `SaveMachineStateToPath` already omits zero/free guest pages, so `machine-state.vzm` is the high-entropy residual of live RAM. Measured on `vz`: **zstd ~0%, lz4 net-larger**, while the compress pass burns CPU in the paused window. Not implemented on `vz`: the compression candidate set does not list `machine-state.vzm`, and the shim's `vz` snapshot request carries only `destination_path`.

This differs for Firecracker/Cloud-Hypervisor, whose raw memory files include zero pages and compress well — the async compression subsystem (`lib/instances/snapshot_compression.go`) is retained intact for them and supports native `zstd`/`lz4` with a pure-Go fallback, decompressing on restore via `ensureSnapshotMemoryReady`.

### B. Overlap the machine-state save with the disk copy — no-op on `vz`

The idea: the guest disk is quiescent the moment the VM is paused, so the disk copy could run concurrently with `SaveMachineStateToPath` instead of after it. With Section C landed, the disk copy is a `clonefile` CoW clone — ~1 ms and size-independent — so there is essentially nothing to overlap.

Measured standby of a 2 GiB-overlay VM: **serial ~445 ms vs overlap ~482 ms — within noise.** The overlap also added real complexity (an errgroup over save+copy legs, pruning the in-flight `snapshots/` subtree from the disk copy and recopying it after the save, and cross-leg failure/cleanup handling). Not worth it: the `vz` path keeps the simple serial save-then-copy.

This would matter again only when the disk copy is *not* `clonefile`-instant — the cross-volume / non-APFS sparse-copy fallback — which is not the common deployment.

## Current shipped behavior in hypeman

### Standby orchestration (serial)

`standbyInstance` (`lib/instances/standby.go`) performs a serial multi-hop transition:

1. Pause the VM (`hv.Pause`).
2. Create the snapshot into `InstanceSnapshotLatest(id)` via `createSnapshot` → `hv.Snapshot`. For `vz` this reaches the shim's `handleSnapshot`, which requires the VM be paused, validates save/restore support, then calls `vm.SaveMachineStateToPath(<dir>/machine-state.vzm)` and writes the `SnapshotManifest` as `config.json` (`cmd/vz-shim/server.go`). The HTTP call uses a long-running client because duration scales with guest RAM (`lib/hypervisor/vz/client.go`).
3. Shut down the VMM process.
4. Release network, persist metadata.

A disk copy happens when a standby is promoted to a stored snapshot or forked (`copySnapshotPayload`, `lib/instances/snapshot.go`), serially with respect to the rest of the operation, using the `clonefile` fast path on APFS.

### Restore orchestration

`restoreInstance` (`lib/instances/restore.go`) materializes any compressed memory (Firecracker/CH) via `ensureSnapshotMemoryReady`, then `restoreFromSnapshot` → `starter.RestoreVM`. For `vz`, `RestoreVM` reads the manifest, sets `RestoreMachineStatePath`, and spawns the shim. The shim restores instead of cold-booting; the VM lands paused, and the caller resumes.

On `vz`, `machine-state.vzm` is never compressed, so it is always present as written by the save — no decompress step before restore.

### vz Resume is synchronous from the caller's view

`vz`'s `vm.resume` is a fire-and-forget PUT to the shim: it returns before the guest has left the Paused/Resuming transition, so an immediate `vm.info` read-back or guest exec could observe a transient Paused. `Client.Resume` (`lib/hypervisor/vz/client.go`) therefore issues the resume PUT and then polls the shim's raw `vm.info` state until it reports `Running`, bounded by a short timeout. This makes `Resume`'s post-condition "the guest is Running", so generic restore needs no hypervisor-specific state priming.

## Platform constraints & edge cases

- **macOS 14+, Apple silicon only.** Save/restore is restricted to darwin/arm64 by build tags and gated at runtime by `validateSaveRestoreSupport` (`cmd/vz-shim`), which calls the framework's `ValidateSaveRestoreSupport()` (requires macOS 14+); `Capabilities().SupportsSnapshot` is `runtime.GOARCH == "arm64"`. `clonefile` has been available since the introduction of APFS.
- **APFS volume boundaries / non-APFS.** `clonefile` only clones within a single APFS volume; cross-volume returns `EXDEV` and non-APFS returns `ENOTSUP`, and we fall back to a sparse copy whose cost scales with disk size. No correctness risk, only a performance cliff. The common deployment keeps instance and snapshot dirs on one APFS volume and gets CoW.
- **Compatibility / device-config stability.** Restore fails if the saved state is incompatible with the current configuration. The disk clone copies the disk exactly, so it does not affect restore compatibility; the constraint that NIC identity must not be rewritten in the serialized config (`lib/hypervisor/vz/fork.go`) is unaffected.

## Testing

- **`lib/forkvm` (landed with #276):** `copy_reflink_darwin_test.go` covers clone correctness over a guest-shaped tree, the stale-destination contract, the forced sparse fallback, and the `isReflinkUnsupportedError` table. Fork latency: `BenchmarkForkGuestDisk` and the end-to-end `TestVZForkSpeed`.
- **Running-source snapshot round-trip (darwin/arm64):** `TestVZRunningSnapshotRoundTrip` (`lib/instances/snapshot_running_source_darwin_test.go`) drives a standby snapshot from a *running* source → fork → restore → resume → guest reachable with state intact. This exercises the running-source save + disk-copy path the other darwin tests don't (they snapshot from standby).
- **Firecracker/CH compression** tests stay; there are no `machine-state.vzm` compression tests (not applicable on `vz`).

## Prior art: Tart

Tart is an open-source VM toolset on the same Virtualization.framework. Its suspend/resume uses the same path-based save/restore contract (`state.vzvmsave`), detects state by file existence, and clones via `FileManager.copyItem` — which on APFS transparently performs a `clonefile` CoW clone. hypeman gets the same CoW behavior via an explicit `unix.Clonefile` call in `lib/forkvm`, so the path is observable, testable, and has a defined fallback. Tart constrains suspend to interactive GUI macOS guests; hypeman's guests are headless Linux, gated only by `validateSaveRestoreSupport`.

## Risks & alternatives considered

- **Compressing `machine-state.vzm`** burns host CPU during reclaim and extends the paused window with no offsetting space win on `vz` (zero pages already omitted). Dropped. The async compression subsystem still helps Firecracker/Cloud-Hypervisor.
- **Overlapping save and disk copy** adds concurrency and cleanup complexity for no measurable benefit once the disk copy is `clonefile`-instant (445 ms serial vs 482 ms overlap on a 2 GiB-overlay VM). Dropped on `vz`; would matter for the cross-volume/non-APFS sparse-copy fallback.
- **`clonefile` vs `FICLONE` semantics (resolved in #276).** `clonefile` creates the destination and copies metadata; the darwin path removes the destination first, drops owner/SUID/SGID/ACL via `CLONE_NOOWNERCOPY`, and re-applies perms with `Chmod`.
