# Snapshot Feature

Snapshots are immutable point-in-time captures of a VM that can later be:
- restored back into the original VM
- forked into a new VM
- listed, inspected, and deleted independently

## Snapshot Kinds

### `Standby`
- Captures standby-style state, including memory/device snapshot state plus disks.
- Intended for fast resume-style recovery.
- Can be created from `Running` or `Standby`.
- Does **not** allow hypervisor switching on restore/fork.

### `Stopped`
- Captures disk-focused state from a stopped VM.
- Intended for cold-start style restore/fork.
- Can be created only from `Stopped`.
- Allows optional hypervisor switching on restore/fork because no memory state is carried across.

## Lifecycle

### Create
- `Standby` snapshot from `Running`:
  - source VM is put into standby
  - snapshot payload is copied
  - source VM is restored to running
- `Standby` snapshot from `Standby`:
  - snapshot payload is copied directly
- `Stopped` snapshot from `Stopped`:
  - snapshot payload is copied directly

## Snapshot Compression

Snapshot memory compression is optional and is **off by default**.

- Compression applies only to `Standby` snapshots, because only standby snapshots contain guest memory state.
- `Stopped` snapshots cannot use compression because they do not include resumable RAM state.
- Compression affects only the memory snapshot file, not the entire snapshot directory.

### When compression runs

- A standby operation can request compression explicitly.
- A snapshot create request can request compression explicitly.
- If the request does not specify compression, Hypeman falls back to:
  - the instance's `snapshot_policy`
  - then the server's global `snapshot.compression_default`
- Effective precedence is:
  - request override
  - instance default
  - server default
  - otherwise no compression

Compression runs **asynchronously after the snapshot is already durable on disk**.

- This keeps the standby path fast.
- Standby can return successfully while compression is still running in the background.
- That means a later restore can arrive before compression has finished.
- While compression is running, the snapshot remains valid and reports `compression_state=compressing`.
- Once finished, the snapshot reports `compression_state=compressed` and exposes compressed/uncompressed size metadata.
- If compression fails, the snapshot reports `compression_state=error` and keeps the error message for inspection.

### Restore behavior with compressed snapshots

Restore always uses the same flow across hypervisors.

- If a restore starts while compression is still running, Hypeman cancels the in-flight compression job first.
- Restore then synchronizes with that job so it is no longer mutating the snapshot files.
- If the raw memory file still exists after cancellation, restore uses the raw file directly and removes any compressed sibling artifacts.
- If the raw file is already gone, Hypeman expands the compressed file back to the raw memory file before asking the hypervisor to restore.
- This means restore never races a live compressor, while still preferring the raw snapshot whenever it is still available.

In practice, the tradeoff is:

- standby stays fast because compression is not on the hot path
- restore from a compressed standby snapshot pays decompression cost
- restore from an uncompressed standby snapshot avoids that cost entirely

### Supported algorithms

- `zstd`
  - default when compression is enabled
  - supports levels `1-19`
  - defaults to level `1` when no level is specified
- `lz4`
  - optimized for lower decompression overhead
  - supports levels `0-9`
  - defaults to level `0` (fastest) when no level is specified

### Native codec preference

Compression and decompression try native system codecs first:

- `zstd` snapshots prefer the `zstd` binary
- `lz4` snapshots prefer the `lz4` binary

If the native binary is missing or not executable, Hypeman falls back to the in-process Go implementation.

- This fallback is operational, not a behavior change: snapshot artifacts remain the same `.zst` and `.lz4` formats.
- A fallback emits a warning log with an install hint.
- A fallback also increments `hypeman_snapshot_codec_fallbacks_total`.
- If the native codec starts but fails for a real runtime reason, Hypeman does **not** silently fall back; it treats that as an error.

### Restore (in-place)
- Restore always applies to the original source VM.
- Source VM must not be `Running`.
- Default target states:
  - `Standby` snapshot -> `Running`
  - `Stopped` snapshot -> `Stopped`
- Allowed target states:
  - `Standby` snapshot -> `Running`, `Standby`, `Stopped`
  - `Stopped` snapshot -> `Stopped`, `Running`

### Fork (new VM)
- Creates a new instance from snapshot artifacts.
- Uses the same target-state rules as restore.
- `target_hypervisor` is allowed only for `Stopped` snapshots.

### Delete
- Removes snapshot metadata and payload.
- Does not modify source or forked instances.

## Scheduled Snapshots

Per-instance schedules can create snapshots automatically on an interval.

- Configure with:
  - `PUT /instances/{id}/snapshot-schedule`
- Inspect with:
  - `GET /instances/{id}/snapshot-schedule`
- Disable with:
  - `DELETE /instances/{id}/snapshot-schedule`

### Schedule Rules
- Schedules do not take a snapshot kind input.
- Each run auto-selects behavior from current source state:
  - `Running`/`Standby` source -> `Standby` snapshot
  - `Stopped` source -> `Stopped` snapshot
- `Standby` scheduled runs against a `Running` source include a brief pause/resume cycle during capture.
- `interval` uses Go duration format (for example `1h`, `24h`).
- `retention` is required and must set at least one of:
  - `max_count`: keep only the newest N scheduled snapshots (`0` disables count-based cleanup)
  - `max_age`: delete scheduled snapshots older than a duration
- Optional `name_prefix` (max 47 chars) and `metadata` are applied to each scheduled snapshot.

### Cleanup Scope
- Retention cleanup only targets snapshots created by the schedule for that same instance.
- Manually created snapshots are never deleted by schedule retention.
- If the source instance is deleted, the schedule remains until scheduled snapshots for that instance are gone, then self-deletes.

## Safety Rules

- Snapshot creation rejects writable volume attachments.
- Snapshot names must be unique per source instance.
- Snapshot IDs are immutable.
- Snapshot artifacts remain usable after source instance deletion.

## Stored Data

Each snapshot stores:
- immutable snapshot metadata (`id`, `name`, `kind`, source identity, timestamp, size)
- snapshot payload (`guest/`) used for restore/fork
- source metadata needed to reconstruct VM runtime settings during restore/fork

## Sparse Copy Behavior

Snapshot payload copy uses the same sparse-only guest copy path as fork.
- If sparse extent copy cannot be guaranteed, operations fail explicitly.
- No dense-copy fallback is used.
