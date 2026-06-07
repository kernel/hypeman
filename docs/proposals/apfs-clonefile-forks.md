# RFC: APFS `clonefile(2)` fast path for macOS VM disk forks

## Summary

On macOS, hypeman's `forkvm` disk-copy path has no reflink fast path: the darwin build of `copyRegularFileReflink` is a no-op stub that always reports `ErrReflinkUnsupported`, so every fork falls through to a full `SEEK_DATA`/`SEEK_HOLE` byte copy. This RFC wires Apple's `clonefile(2)` into that reflink slot so that forking a VM's guest directory on Apple Silicon APFS becomes an instantaneous, copy-on-write, space-shared operation. The change is confined to the existing reflink->sparse fallback ladder in `lib/forkvm` and, as verified below, requires no new dependency.

## Motivation

Forking is hypeman's primitive for creating a new instance from a snapshot or an existing instance. The disk side of a fork is a recursive copy of a guest directory containing the rootfs, the config disk, overlay disks, and (for VZ) a serialized machine-state file. These payloads are multi-hundred-megabyte to multi-gigabyte files. On Linux, hypeman already clones them at the block layer via `FICLONE` (`unix.IoctlFileClone`) when the destination filesystem is btrfs/xfs-with-reflink/zfs/bcachefs, making forks effectively free in both time and space until the guest writes diverging pages.

On macOS the same code path degrades to a dense, chunked, 1 MiB-at-a-time `pread`/`pwrite` loop (`copyFileExtent`, `lib/forkvm/copy_sparse_unix.go:103`). For a multi-gigabyte rootfs this is the difference between a fork that completes in milliseconds and one that takes seconds and consumes the full logical size on disk. macOS ships APFS, which supports copy-on-write file clones through `clonefile(2)`; not using it leaves the platform's headline capability on the table.

This helps anyone running hypeman on Apple Silicon developer machines or macOS CI: faster instance creation from snapshots, lower disk pressure when many instances share a base image, and parity with the Linux fast path so fork latency and disk-usage characteristics are no longer wildly platform-dependent.

## Current behavior in hypeman

The disk copy for all forks funnels through `forkvm.CopyGuestDirectory` / `CopyRegularFile`. Call sites:

- `lib/instances/snapshot.go:552` — `forkvm.CopyGuestDirectory(m.paths.SnapshotGuestDir(snapshotID), instanceDir)` materializes a snapshot's guest payload into a new instance directory.
- `lib/instances/snapshot_alias_lock.go:30`, `:45`, `:64` — alias/lock-protected snapshot copies via `CopyGuestDirectory` / `CopyGuestDirectoryWithOptions`.
- `lib/hypervisor/firecracker/firecracker.go:145` — `forkvm.CopyRegularFile` for deferred snapshot memory (a Firecracker/Linux path, not darwin, but it shares the same helper).

For VZ specifically, `lib/hypervisor/vz/fork.go` does **not** copy any disk bytes. `PrepareFork` (`fork.go:18`) and `rewriteSnapshotManifestForFork` (`fork.go:33`) only rewrite the snapshot manifest — patching paths inside the serialized shim config (`rewriteShimConfigPaths`, `fork.go:82`) and clearing runtime-only fields. The actual disk materialization for a VZ fork happens earlier, through `forkvm.CopyGuestDirectory`. So improving the copy primitive improves VZ fork performance without touching the VZ-specific code.

The copy dispatch (`lib/forkvm/copy.go:158`):

```go
func copyRegularFile(state *copyState, srcPath, dstPath string, perms fs.FileMode) error {
	if state == nil || !state.reflinkDead {
		err := copyRegularFileReflink(srcPath, dstPath, perms)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrReflinkUnsupported) {
			return err
		}
		if state != nil {
			state.reflinkDead = true
		}
	}
	return copyRegularFileSparse(srcPath, dstPath, perms)
}
```

Two behaviors matter for this RFC:

1. **The `reflinkDead` short-circuit** (`copy.go:159`, `:167`). The first file that returns `ErrReflinkUnsupported` flips `state.reflinkDead = true`, so the rest of the directory walk skips the reflink attempt and goes straight to sparse copy. This is a per-`CopyGuestDirectory`-call cache: once we learn the destination filesystem can't reflink, we stop re-paying the rejection on every subsequent file. A darwin `clonefile` path must map "unsupported" failures (including the cross-volume `EXDEV` case) to `ErrReflinkUnsupported` so this short-circuit fires correctly, and must map real failures to themselves so they propagate.

2. **The Linux reflink impl pre-creates the destination** (`lib/forkvm/copy_reflink_linux.go:28`): it opens `dst` with `os.O_CREATE|os.O_TRUNC|os.O_WRONLY` before issuing the `FICLONE` ioctl, because `FICLONE` clones *into* an existing, open destination fd. On failure it removes `dstPath` in the deferred cleanup (`copy_reflink_linux.go:37`). This is exactly the opposite of what `clonefile(2)` wants — see "Proposed design".

The darwin stub today (`lib/forkvm/copy_reflink_other.go:13`):

```go
//go:build !linux

func copyRegularFileReflink(srcPath, dstPath string, perms fs.FileMode) error {
	_ = dstPath
	_ = perms
	return fmt.Errorf("%w: reflink unsupported on this platform: %s", ErrReflinkUnsupported, srcPath)
}
```

Its own comment (`copy_reflink_other.go:10-12`) already notes that "On macOS APFS supports clonefile(2) and could be wired up here, but we currently only rely on the sparse-copy fallback off-Linux." This RFC does exactly that.

The Linux unsupported-error mapping that the darwin path must mirror in spirit (`copy_reflink_linux.go:53`):

```go
func isReflinkUnsupportedError(err error) bool {
	switch {
	case errors.Is(err, unix.EINVAL),
		errors.Is(err, unix.ENOTSUP),
		errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.EXDEV),
		errors.Is(err, unix.ETXTBSY),
		errors.Is(err, unix.EISDIR),
		errors.Is(err, unix.ENOTTY):
		return true
	}
	return false
}
```

## Prior art: Tart

Tart is an open-source VM toolset built on Apple's Virtualization.framework. It already relies on APFS copy-on-write for VM cloning and is useful prior art for the same problem on the same platform.

What Tart documents and does:

- `Sources/tart/Commands/Clone.swift:11-14` — the command's own discussion text states the design intent plainly: "Due to copy-on-write magic in Apple File System, a cloned VM won't actually claim all the space right away. Only changes to a cloned disk will be written and claim new space. This also speeds up clones enormously." This is the exact property hypeman wants on darwin.
- `Sources/tart/VMDirectory.swift:119-123` — `clone(to:generateMAC:)` copies the config, NVRAM, and disk with `FileManager.default.copyItem(at:to:)`. On APFS, `copyItem` performs a `clonefile` CoW transparently; Tart does not call `clonefile(2)` directly.
- `Sources/tart/Commands/Clone.swift:77-82` — after cloning, Tart deliberately reclaims/pre-allocates space: "APFS is doing copy-on-write, so the above cloning operation ... is not actually claiming new space until the VM is started and it writes something to disk. So, once we clone the VM let's try to claim the rest of space for the VM to run without errors." It computes `sourceVM.sizeBytes() - sourceVM.allocatedSizeBytes()` and calls `Prune.reclaimIfNeeded(...)` (`Sources/tart/Commands/Prune.swift:107`), capped by a prune limit. `allocatedSizeBytes` is read from the file's `totalFileAllocatedSize` resource value (`Sources/tart/URL+Prunable.swift:13`), and `reclaimIfNeeded` compares required bytes against `volumeAvailableCapacity` (`Prune.swift:118-145`).

**What to adopt.** The core insight is identical and worth following: on APFS, the right primitive for forking a VM disk is a `clonefile` CoW clone, because it is near-instant and shares blocks until first write. hypeman should treat APFS clone as the darwin equivalent of the Linux `FICLONE` fast path.

**Where to diverge.**

1. *Implicit vs. explicit clone.* Tart leans on `FileManager.copyItem` doing `clonefile` implicitly. hypeman already has an explicit, typed reflink->sparse fallback ladder (`copy.go:158`) with a tested "unsupported -> fall back" contract and a `reflinkDead` short-circuit. We keep that structure and call `clonefile(2)` directly rather than going through a higher-level copy that hides whether the clone happened. An explicit syscall lets us distinguish "filesystem can't clone, fall back" (`EXDEV`, `ENOTSUP`) from "real I/O error, propagate" (`EIO`, `ENOSPC`) precisely, which the directory-walk short-circuit depends on. `FileManager.copyItem` would silently dense-copy on `EXDEV`, defeating both the fallback bookkeeping and our ability to observe it in tests.

2. *Eager reclaim vs. CoW-as-intended.* Tart's reclaim step trades away the space savings to avoid a runtime `ENOSPC` when the guest later writes into shared blocks. hypeman's fork model is different: guest writes land in per-instance overlay files (`lib/paths` `InstanceOverlay`, `InstanceVolumeOverlay`), and the cloned rootfs is generally read-mostly, so the CoW-`ENOSPC`-at-runtime risk is lower and the space savings are the whole point. This RFC therefore does **not** add eager reclaim to the default fork path; it is called out in "Risks & alternatives" and "Open questions" as an optional, opt-in follow-up rather than default behavior.

## Proposed design

### Dependency reality check (verified)

The task framing assumed `golang.org/x/sys` v0.38.0 exports no `clonefile` wrapper and that a raw `SYS_CLONEFILEAT` syscall would be needed. That assumption comes from running `go doc` with the default `GOOS=linux`, where the darwin-only symbols are not visible. Built for darwin they are present:

```
$ go doc golang.org/x/sys/unix Clonefile            # GOOS=linux (default)
doc: no symbol Clonefile in package golang.org/x/sys/unix

$ GOOS=darwin GOARCH=arm64 go doc golang.org/x/sys/unix Clonefile
func Clonefile(src string, dst string, flags int) (err error)

$ GOOS=darwin GOARCH=arm64 go doc golang.org/x/sys/unix Clonefileat
func Clonefileat(srcDirfd int, src string, dstDirfd int, dst string, flags int) (err error)

$ GOOS=darwin GOARCH=arm64 go doc golang.org/x/sys/unix Fclonefileat
func Fclonefileat(srcDirfd int, dstDirfd int, dst string, flags int) (err error)
```

These are generated wrappers in the module cache at
`golang.org/x/sys@v0.38.0/unix/zsyscall_darwin_arm64.go:1093` (`Clonefile`),
`:1117` (`Clonefileat`), declared via `//sys` directives in
`unix/syscall_darwin.go:714-727`, and gated by `//go:build darwin && arm64`. They dynamically import `clonefile`/`clonefileat` from `/usr/lib/libSystem.B.dylib`. The flag constants are also present:
`CLONE_NOFOLLOW = 0x1` and `CLONE_NOOWNERCOPY = 0x2`
(`unix/zerrors_darwin_arm64.go:238-239`), and the raw `SYS_CLONEFILEAT = 462`
(`unix/zsysnum_darwin_arm64.go:372`) if we ever wanted it.

**Conclusion:** the fast path can be built on `unix.Clonefile` (or `unix.Clonefileat`) with the pinned `x/sys` v0.38.0 — no dependency bump, no cgo shim, no raw `SYS_CLONEFILEAT` syscall. The raw-syscall and dependency-bump options are still discussed in "Risks & alternatives" for completeness, but they are not the recommendation.

`unix.IoctlFileClone` (the Linux `FICLONE` wrapper) is, as expected, not available on darwin (`GOOS=darwin GOARCH=arm64 go doc ... IoctlFileClone` -> "no symbol"), which is why the build-tag split exists in the first place.

### Build-tag layout

Today the reflink implementation splits on `linux` vs `!linux`:

- `copy_reflink_linux.go` (`//go:build linux`) — real `FICLONE`.
- `copy_reflink_other.go` (`//go:build !linux`) — stub.

We introduce a darwin implementation and narrow the stub:

- `copy_reflink_linux.go` (`//go:build linux`) — unchanged.
- `copy_reflink_darwin.go` (**new**, `//go:build darwin`) — `clonefile(2)` implementation.
- `copy_reflink_other.go` — retag from `//go:build !linux` to `//go:build !linux && !darwin` so the stub covers only platforms with neither fast path.

This mirrors the precedent set by `copy_sparse_unix.go` (`//go:build darwin || linux`), which already shares one Unix file across both OSes; the reflink mechanism genuinely differs per-OS (`ioctl` vs `clonefile`), so it gets per-OS files instead.

### `copy_reflink_darwin.go`

```go
//go:build darwin

package forkvm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// copyRegularFileReflink clones srcPath to dstPath via clonefile(2) (APFS
// copy-on-write). On APFS this is effectively instantaneous and consumes no
// additional space until the cloned file's pages diverge.
//
// Unlike the Linux FICLONE path, clonefile(2) requires the destination to NOT
// exist: it creates the destination as part of the clone. We therefore remove
// any stale destination first and must not pre-create or O_TRUNC it.
//
// Returns ErrReflinkUnsupported when APFS/the volume cannot serve the clone
// (e.g. cross-volume EXDEV, non-APFS ENOTSUP); callers fall back to a sparse
// copy. Real errors (EIO, ENOSPC, EACCES) propagate as-is.
func copyRegularFileReflink(srcPath, dstPath string, perms fs.FileMode) error {
	// clonefile fails with EEXIST if the destination is present. The walk may
	// be re-running over a partially populated destination, so clear it first.
	if err := os.Remove(dstPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale clone destination %s: %w", dstPath, err)
	}

	// CLONE_NOFOLLOW: clone the symlink/file at srcPath itself, never its
	// target. CLONE_NOOWNERCOPY: do not copy owner/SUID/SGID/ACL metadata
	// (we are not necessarily privileged, and the fork should not inherit
	// source ownership); we set the mode explicitly below to honor perms.
	flags := unix.CLONE_NOFOLLOW | unix.CLONE_NOOWNERCOPY
	if err := unix.Clonefile(srcPath, dstPath, flags); err != nil {
		if isReflinkUnsupportedError(err) {
			return fmt.Errorf("%w: clonefile rejected for %s: %v", ErrReflinkUnsupported, srcPath, err)
		}
		return fmt.Errorf("clonefile %s -> %s: %w", srcPath, dstPath, err)
	}

	// clonefile copies the source mode bits; CLONE_NOOWNERCOPY drops owner
	// metadata but the file mode still derives from the source. Enforce the
	// perms contract used by the rest of the copy package (caller passes
	// info.Mode().Perm()).
	if err := os.Chmod(dstPath, perms); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("chmod cloned file %s: %w", dstPath, err)
	}
	return nil
}

// isReflinkUnsupportedError returns true when a clonefile failure indicates the
// clone cannot be served and the caller should fall back to a sparse copy.
// Real errors (EIO, ENOSPC, EACCES) return false and propagate.
func isReflinkUnsupportedError(err error) bool {
	switch {
	case errors.Is(err, unix.ENOTSUP), // volume is not APFS / clone unsupported
		errors.Is(err, unix.EOPNOTSUPP),
		errors.Is(err, unix.EXDEV),  // src and dst on different volumes
		errors.Is(err, unix.EEXIST), // dst already exists (defensive; we remove first)
		errors.Is(err, unix.EINVAL), // bad flags / unsupported combination
		errors.Is(err, unix.ENOTDIR):
		return true
	}
	return false
}
```

Notes on the design decisions, each tied to a concrete difference from the Linux path:

- **Destination must not exist.** This is the central semantic divergence from `copy_reflink_linux.go:28`. The Linux path opens `dst` with `O_CREATE|O_TRUNC` and clones into it; `clonefile(2)` instead creates the destination atomically and fails with `EEXIST` if it is already there. So the darwin impl (a) `os.Remove`s any stale `dst` before the call and (b) does **not** open or pre-create `dst`. The deferred-`os.Remove`-on-error cleanup from the Linux path is unnecessary on the clone itself (a failed `clonefile` creates nothing), but we still remove on a post-clone `chmod` failure.
- **Mode handling.** We pass `CLONE_NOFOLLOW|CLONE_NOOWNERCOPY` then `os.Chmod(dstPath, perms)`. The `perms` argument is what the caller already computes (`info.Mode().Perm()` in `copy.go:100`/`:109`; `info.Mode().Perm()` in `CopyRegularFile`, `copy.go:151`). `CLONE_NOOWNERCOPY` avoids copying owner/SUID/SGID/extended-attribute ownership we have no business propagating into a fork; the explicit `Chmod` then re-establishes exactly the permission bits the rest of the copy package guarantees. This matches the Linux path's effective behavior (the Linux open passes `perms` as the create mode), so callers see identical destination permissions on both platforms.
- **Errno mapping mirrors the Linux ladder by intent, not by literal set.** The Linux `isReflinkUnsupportedError` includes ioctl-specific codes (`ENOTTY`, `EISDIR`, `ETXTBSY`) that don't apply to `clonefile`. The darwin set maps the "this volume/fs can't clone" outcomes to `ErrReflinkUnsupported`:
  - `EXDEV` (`0x12`) — source and destination on different APFS volumes; clone impossible, fall back. This is the key edge case (see "Platform constraints").
  - `ENOTSUP` (`0x2d`) / `EOPNOTSUPP` (`0x66`) — non-APFS volume or clone unsupported.
  - `EEXIST` (`0x11`) — destination present; defensive, since we remove first, but mapping it to fallback rather than a hard error keeps a racing re-walk safe.
  - `EINVAL` / `ENOTDIR` — bad-arg / path-shape failures we'd rather degrade than abort on.

  Everything else propagates unchanged, importantly `EIO` (`0x5`), `ENOSPC` (`0x1c`), and `EACCES` (`0xd`) — these are real failures the operator needs to see, not silent fallbacks. (Constants confirmed in `unix/zerrors_darwin_arm64.go:1626-1732`.)

`copy.go` and the `copyState.reflinkDead` short-circuit need **no change**: `copyRegularFile` (`copy.go:158`) already calls `copyRegularFileReflink`, treats `ErrReflinkUnsupported` as the signal to flip `reflinkDead` and fall back, and propagates any other error. The new darwin impl plugs into that contract unchanged. `SetReflinkDisabled` (`copy.go:24`) continues to force the sparse path for tests on both platforms.

### Stub retag

```go
//go:build !linux && !darwin

package forkvm

// copyRegularFileReflink is unavailable on platforms without a copy-on-write
// clone primitive; callers fall back to the sparse copy.
func copyRegularFileReflink(srcPath, dstPath string, perms fs.FileMode) error {
	_ = dstPath
	_ = perms
	return fmt.Errorf("%w: reflink unsupported on this platform: %s", ErrReflinkUnsupported, srcPath)
}
```

## Configuration / API changes

None to the public API or on-disk layout. `CopyGuestDirectory`, `CopyGuestDirectoryWithOptions`, `CopyRegularFile`, `CopyOptions`, and `SetReflinkDisabled` keep their signatures. The change is internal to which `copyRegularFileReflink` is linked on darwin.

The only escape hatch worth considering is an environment variable to force the sparse path at runtime in production (today `reflinkDisabled` is test-only, set via `SetReflinkDisabled`, `copy.go:24`). If we want operators to disable the clone path without a rebuild, we add a one-time init read, e.g.:

```go
// in copy.go, package-level
func init() {
	if os.Getenv("HYPEMAN_FORKVM_DISABLE_REFLINK") != "" {
		reflinkDisabled.Store(true)
	}
}
```

This is optional and called out as an open question rather than a hard requirement; the `reflinkDead` short-circuit already provides automatic, per-call degradation when cloning isn't available.

## Platform constraints & edge cases

- **`clonefile(2)` availability.** `clonefile` has existed since macOS 10.12 (APFS's introduction). Every Apple Silicon machine ships APFS and a macOS version far newer than that, so on the supported target (hypeman's darwin/VZ host is Apple Silicon) the syscall is always present. The `x/sys` wrappers we depend on are gated `//go:build darwin && arm64` (and an identical `darwin && amd64` variant exists), so an Intel-mac build also links them; the implementation is not arm64-specific in any way that matters.

- **Cross-volume `EXDEV` is the central constraint.** `clonefile(2)` only works when source and destination live on the **same APFS volume** (the same logical volume within an APFS container — block sharing cannot cross volume boundaries). hypeman keeps both the source and destination of a fork under a single `dataDir`:
  - source: `SnapshotGuestDir(snapshotID)` = `dataDir/snapshots/<id>/guest` (`lib/paths/paths.go:284`).
  - destination: `InstanceDir(id)` = `dataDir/guests/<id>` (`lib/paths/paths.go:172`).

  When `dataDir` is a single APFS volume (the normal case), every clone is intra-volume and succeeds. The clone will fail with `EXDEV` only if an operator has mounted a separate volume under part of the `dataDir` tree (e.g. `dataDir/guests` on one APFS volume and `dataDir/snapshots` on another, or `dataDir` on an exFAT/SMB share). The `EXDEV` mapping to `ErrReflinkUnsupported` makes this degrade cleanly to the sparse copy rather than erroring, exactly as the Linux path degrades on a cross-`mount` `FICLONE`. This matches the Linux contract, which already treats `EXDEV` as fall-back (`copy_reflink_linux.go:58`).

- **Non-APFS data directories.** If `dataDir` is on a non-APFS filesystem (exFAT external disk, network mount), `clonefile` returns `ENOTSUP`/`EOPNOTSUPP` and we fall back. The `reflinkDead` short-circuit means we pay this rejection once per `CopyGuestDirectory` call, not once per file.

- **Sparse files.** APFS clones preserve sparseness inherently (it's a CoW reference, not a re-densify), so the destination of a clone is at least as sparse as the source. The existing sparse-preservation guarantee that `TestCopyGuestDirectory_PreservesSparseFiles` (`copy_sparse_unix_test.go:19`) checks for the fallback continues to hold; the clone path is strictly better on sparsity.

- **CoW-then-`ENOSPC` at runtime.** Because a clone shares blocks until first write, a volume that is nearly full at fork time can hit `ENOSPC` later when the guest writes into shared regions. This is the tradeoff Tart addresses with its reclaim step (`Clone.swift:77-82`). For hypeman it is mitigated structurally: guest writes go to per-instance overlays, and the cloned rootfs is read-mostly. We do not add eager reclaim by default (see "Risks & alternatives").

- **Permissions / SUID.** `CLONE_NOOWNERCOPY` plus the explicit `Chmod` means the fork never inherits the source's owner or set-user/group-ID bits beyond the permission mode the caller requested. This is intentional and matches what the Linux open-with-`perms` path produces.

- **Symlinks and non-regular files.** `copyRegularFileReflink` is only ever called for regular files (`copy.go:108`); symlinks, sockets, and directories are handled separately in the walk (`copy.go:114-130`). `CLONE_NOFOLLOW` is belt-and-suspenders for the regular-file case and costs nothing.

## Testing plan

Existing tests already constrain the contract and must keep passing:

- `TestCopyGuestDirectory_ReflinkFallback` (`copy_reflink_test.go:15`) — forces `SetReflinkDisabled(true)` and asserts correctness via the sparse path. Unchanged; still exercises the fallback on darwin.
- `TestCopyGuestDirectory_ReflinkAttempted` (`copy_reflink_test.go:42`) — runs with reflink enabled and asserts a correct destination. On darwin/APFS this now actually exercises `clonefile`; on a non-APFS CI volume it falls back. Either way the assertion (bytes match) holds.
- `TestCopyGuestDirectory_PreservesSparseFiles` (`copy_sparse_unix_test.go:19`), `TestCopyGuestDirectory_SkipsSocketRuntimeArtifacts` (`copy_sparse_unix_test.go:75`) — unchanged.

New darwin-only tests (`copy_reflink_darwin_test.go`, `//go:build darwin`):

1. **Clone correctness over a guest-shaped tree.** Build a source dir with a multi-MiB `rootfs.ext4`, a `config.ext4`, a sparse `overlay.raw`, a symlink, and a `.sock`; run `CopyGuestDirectory` with reflink enabled; assert byte-for-byte equality of regular files, that the symlink and skips behave, and that destination perms equal source perms (the `Chmod` contract).
2. **Stale destination is handled.** Pre-create `dst` as a regular file with different contents, then clone; assert the clone succeeds (the `os.Remove` pre-step fired) and the destination matches the source. This is the explicit guard for the "destination must not exist" divergence.
3. **CoW block sharing (best-effort, skip if unavailable).** Clone a dense N-MiB file within `t.TempDir()`; if `t.TempDir()` is on APFS, assert that immediately after the clone the combined allocated size has not grown by ~N MiB (blocks are shared), using `unix.Stat` + `Blocks*512` as in `allocatedBytes` (`copy_sparse_unix_test.go:117`). Detect non-APFS by observing `ErrReflinkUnsupported` from a direct `copyRegularFileReflink` call and `t.Skip`. This makes the test meaningful on developer machines without being flaky on CI runners whose temp volume isn't APFS.
4. **`EXDEV` maps to fallback (unit).** Unit-test `isReflinkUnsupportedError` directly with `unix.EXDEV`, `unix.ENOTSUP`, `unix.EOPNOTSUPP`, `unix.EEXIST`, `unix.EINVAL`, `unix.ENOTDIR` (expect `true`) and `unix.EIO`, `unix.ENOSPC`, `unix.EACCES` (expect `false`). No special filesystem needed.

`go vet`/build verification: build with `GOOS=darwin GOARCH=arm64` and `GOOS=darwin GOARCH=amd64` to confirm the new file and the retagged stub compile under both darwin arches, and build a non-darwin/non-linux target (e.g. `GOOS=freebsd`) to confirm the narrowed stub still satisfies the linker.

## Risks & alternatives considered

**Risk: `clonefile` mode/owner surprises.** If `CLONE_NOOWNERCOPY` interacts unexpectedly with extended attributes on rootfs images, the explicit `Chmod` only fixes mode bits, not xattrs. Mitigation: hypeman's guest-dir payloads are disk images and JSON, not xattr-bearing trees; if xattr fidelity ever matters we can drop `CLONE_NOOWNERCOPY` and run privileged, or strip xattrs explicitly. Called out rather than silently assumed.

**Risk: CoW-`ENOSPC` at runtime (the Tart tradeoff).** Discussed above. We accept it by default because hypeman's overlay model contains guest writes. *Alternative considered:* port Tart's reclaim step (`Prune.reclaimIfNeeded`, `Prune.swift:107`) — after cloning, check `volumeAvailableCapacity` and pre-allocate the unshared remainder. Rejected as default behavior because it discards the space savings that motivate this RFC and adds a disk-capacity policy hypeman doesn't otherwise have. Left as an opt-in follow-up (see Open questions).

**Alternative: raw `SYS_CLONEFILEAT` syscall.** Viable — `unix.SYS_CLONEFILEAT = 462` and the `CLONE_*` flags are present in v0.38.0 (`zsysnum_darwin_arm64.go:372`, `zerrors_darwin_arm64.go:238-239`), so a `unix.Syscall6(unix.SYS_CLONEFILEAT, AT_FDCWD, srcPtr, AT_FDCWD, dstPtr, flags, 0)` would work without any wrapper. Rejected because the typed `unix.Clonefile` wrapper already exists for darwin in the pinned version (verified via `GOOS=darwin go doc`), so hand-rolling `unsafe.Pointer` string marshaling and trampoline plumbing would be strictly more error-prone for zero benefit. The raw path remains a fallback if a future `x/sys` ever drops the wrapper.

**Alternative: bump `golang.org/x/sys`.** Unnecessary. The wrappers and constants we need are all in the currently pinned v0.38.0. A bump would be churn with no functional gain for this feature and is explicitly not part of this RFC.

**Alternative: cgo shim calling `clonefile(2)` from C.** Rejected. It introduces cgo to a package that is pure Go today, complicating cross-compilation and the build, for a syscall the standard `x/sys` wrapper already exposes.

**Alternative: `FileManager`-style implicit clone (Tart's approach).** Rejected for hypeman because there is no Go `copyItem` equivalent that both clones on APFS and reports whether the clone happened; we'd lose the precise `EXDEV`/`ENOSPC` discrimination the `reflinkDead` short-circuit and tests depend on.

## Rollout / milestones

1. **Add `copy_reflink_darwin.go` + retag `copy_reflink_other.go`.** Land the implementation and the stub narrowing. No behavior change on Linux; darwin gains the fast path. Verify both darwin arches and a non-darwin/non-linux build compile.
2. **Add darwin tests.** The four test groups above. CI on an Apple Silicon runner exercises the real clone path; the CoW-sharing assertion self-skips on non-APFS volumes.
3. **Observe in a darwin/VZ environment.** Confirm forks via `CopyGuestDirectory` (`snapshot.go:552`, `snapshot_alias_lock.go`) complete near-instantly and that disk usage reflects block sharing. Confirm the `reflinkDead` short-circuit logs/behaves correctly when `dataDir` is non-APFS.
4. **(Optional follow-up)** Add the `HYPEMAN_FORKVM_DISABLE_REFLINK` runtime escape hatch and/or an opt-in reclaim step if a real CoW-`ENOSPC` scenario is observed. Separate PR, gated by demand.

## Open questions

1. **Runtime escape hatch.** Do we want `HYPEMAN_FORKVM_DISABLE_REFLINK` in this change, or is the automatic `reflinkDead` degradation enough? Leaning "not needed initially."
2. **Eager reclaim.** Should hypeman ever pre-allocate the unshared remainder after a clone (Tart's `reclaimIfNeeded` pattern, `Prune.swift:107`)? Only if a concrete CoW-`ENOSPC`-at-runtime case shows up; if so, it should be opt-in and capacity-aware, not default.
3. **`dataDir` volume validation.** Should hypeman warn at startup when `dataDir/guests` and `dataDir/snapshots` resolve to different APFS volumes (so the clone path would silently fall back to dense copies)? A `statfs`-based check could surface the misconfiguration instead of letting forks quietly run slow.
4. **Intel-mac coverage.** The wrappers are present for `darwin && amd64` too, but hypeman's VZ host target is Apple Silicon. Do we want an Intel CI lane, or is build-only verification of the amd64 darwin target sufficient?
