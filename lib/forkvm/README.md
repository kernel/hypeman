# Fork VM Helpers

This package contains low-level helpers used by instance forking.

## Why this package exists

Forking includes filesystem cloning work that is independent of any specific hypervisor. Keeping that logic outside `lib/instances` keeps lifecycle orchestration focused on state transitions and locking.

## What it provides

- `CopyGuestDirectory(srcDir, dstDir)`
  - Recursively copies a guest directory.
  - Skips runtime sockets because they are process-local artifacts.

## How it is used

`lib/instances/fork.go` calls this package to clone guest data before metadata and hypervisor-specific fork preparation.

## Safety notes

- This package does not perform lifecycle locking by itself.
- Locking and state validation must be handled by callers (`instances.Manager`), which already serializes per-instance lifecycle operations.
