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
