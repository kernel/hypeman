# Fork VM Helpers

This package contains low-level helpers used by instance forking.

## Why this package exists

Forking is mostly filesystem and snapshot identity rewriting work. Keeping that logic outside `lib/instances` keeps lifecycle orchestration focused on state transitions and locking.

## What it provides

- `CopyGuestDirectory(srcDir, dstDir)`
  - Recursively copies a guest directory.
  - Skips runtime sockets (for example `ch.sock`) because they are process-local artifacts.
- `RewriteSnapshotConfig(configPath, opts)`
  - Rewrites `snapshots/snapshot-latest/config.json` for a forked cloud-hypervisor instance.
  - Supports:
    - source data-dir -> target data-dir path remap
    - vsock CID/socket rewrite
    - serial log path rewrite
    - network fields (`tap`, `ip`, `mac`, `mask`) rewrite

## How it is used

`lib/instances/fork.go` calls this package to clone guest data and prepare copied snapshot state. `lib/instances/restore.go` uses it when a standby fork restores with a fresh network identity.

## Safety notes

- This package does not perform lifecycle locking by itself.
- Locking and state validation must be handled by callers (`instances.Manager`), which already serializes per-instance lifecycle operations.
