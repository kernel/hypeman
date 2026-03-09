# Cross-Server Snapshot Transfer (v1)

This feature lets one Hypeman server push a snapshot to another Hypeman server.
It is async, resumable, and preserves sparse guest files.

## What v1 does

- Push-only transfer model (source-initiated).
- Destination import is metadata+payload only (no auto-restore, no fork side effects).
- Supports both `Stopped` and `Standby` snapshots.
- Enforces strict standby compatibility checks before transfer starts.
- Supports cancel/resume across process restarts on both source and destination.

## End-to-end flow

1. Source receives `POST /snapshots/{snapshotId}/transfers`.
1. Source performs destination preflight (`/snapshot-import-sessions/preflight`).
1. If preflight fails (conflict/incompatible), request fails immediately and no async job is created.
1. If preflight passes, source creates an async transfer job.
1. Source creates or resumes a destination import session.
1. Source uploads ordered chunks with per-chunk checksum verification.
1. Destination durably records committed chunk indexes.
1. Source can resume by reading destination committed progress and skipping already committed chunks.
1. Source calls destination complete endpoint to finalize import into snapshot storage.

## Resumability and idempotency

- Source transfer jobs are persisted on disk and resumed on startup.
- Destination import sessions are persisted on disk and survive restart.
- Chunk uploads are idempotent by chunk index + checksum.
- Out-of-order chunk writes are rejected to keep deterministic assembly.
- Resume works even if either side restarts mid-transfer.

## Sparse-file preservation

- Source builds a deterministic manifest of guest tree entries and sparse extents.
- Payload contains only data extents; holes are represented implicitly by offsets.
- Destination reconstructs files by truncating to final size and writing only data extents.
- Result preserves sparse holes instead of inflating to dense files.

## Compatibility and conflict policy

- Preflight is required before job acceptance.
- Destination rejects incompatible `Standby` snapshots when standby-critical metadata does not match.
- Destination rejects conflicting snapshot/session state.
- Source snapshot identity is preserved for transfer intent; conflicts fail fast instead of auto-overwrite.

## Cancellation behavior

- Source cancel marks the transfer cancelled and stops worker progress.
- Source best-effort calls destination cancel endpoint.
- Destination cancel removes partial data and marks session cancelled.

## Security and auth contract

- Caller provides destination token per start request.
- Source uses this token as `Authorization: Bearer ...` for destination session calls.
- Tokens are not returned in API responses and should not be logged.
- For auto-resume, source persists the destination token with restrictive file permissions.

## Concurrency

- Source transfer workers are bounded by `snapshot_transfer.max_concurrent` (default `2`).
- Jobs beyond that limit queue until workers are available.
