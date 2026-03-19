---
name: hypeman-remote-linux-tests
description: Run Hypeman tests on a remote Linux host that supports the Linux hypervisors. Use when validating cloud-hypervisor, Firecracker, QEMU, embedded guest artifacts, or Linux-only integration behavior on a server over SSH.
---

# Hypeman Remote Linux Tests

Use this skill to run Hypeman tests on a remote Linux server in a way that is repeatable and low-friction for any developer.

## Quick Start

1. Pick a short remote working directory under the remote user's home directory.
2. Ensure the remote test shell includes `/usr/sbin` and `/sbin` on `PATH`.
3. Bootstrap embedded artifacts and bundled binaries before running `go test`.
4. Run the smallest relevant test target first.
5. Treat host-environment failures separately from product failures.

## Remote Workspace

Prefer a short path such as `~/hm`, `~/hypeman-test`, or another short directory name.

Cloud Hypervisor uses UNIX sockets with strict path-length limits. Long paths from default test temp directories or deeply nested clones can make the VMM fail before the actual test starts.

If a new test creates temporary directories for guest data, prefer a short root like:

```bash
mktemp -d /tmp/hmcmp-XXXXXX
```

## Remote Bootstrap

Before running Linux hypervisor tests, make sure the remote clone has the generated guest binaries and embedded VMM assets:

```bash
export PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH
make ensure-ch-binaries ensure-firecracker-binaries ensure-caddy-binaries build-embedded
```

Why:

- `go:embed` patterns in the repo require the bundled binaries to exist at build time.
- `mkfs.ext4` is commonly installed in `/usr/sbin` or `/sbin`, not always on the default non-login shell `PATH`.
- Running `go test` before this bootstrap often fails during package setup, not during test execution.

QEMU is usually provided by the host OS, so there is no matching bundled `ensure-qemu-binaries` target. If QEMU tests fail before boot, verify the system `qemu-system-*` binary separately.

## Test Execution Pattern

Start with one focused test:

```bash
go test ./lib/instances -run TestCloudHypervisorStandbyRestoreCompressionScenarios -count=1 -v
```

Then run the sibling hypervisor tests one at a time:

```bash
go test ./lib/instances -run TestFirecrackerStandbyRestoreCompressionScenarios -count=1 -v
go test ./lib/instances -run TestQEMUStandbyRestoreCompressionScenarios -count=1 -v
```

For unit-only validation:

```bash
go test ./lib/instances -run 'TestNormalizeCompressionConfig|TestResolveSnapshotCompressionPolicyPrecedence' -count=1
```

Run one hypervisor at a time when the tests boot real VMs. This avoids noisy failures from competing KVM guests and makes logs much easier to interpret.

## Syncing Local Changes

If the main development is happening locally and the Linux host is only for execution, sync only the files you changed rather than recloning every time.

Typical pattern:

```bash
rsync -az lib/instances/... remote-host:~/hypeman-test/lib/instances/
```

After syncing, re-run the bootstrap command if the changes affect embedded binaries, generated assets, or files referenced by `go:embed`.

## Common Host Failures

Classify failures quickly:

- Missing embedded assets:
  - symptom: package setup fails with `pattern ... no matching files found`
  - fix: run `make ensure-ch-binaries ensure-firecracker-binaries ensure-caddy-binaries build-embedded`
- `mkfs.ext4` not found:
  - symptom: overlay or disk creation fails with `exec: "mkfs.ext4": executable file not found`
  - fix: add `/usr/sbin:/sbin` to `PATH`, then retry
- Cloud Hypervisor socket path too long:
  - symptom: `path must be shorter than SUN_LEN`
  - fix: shorten the remote repo path and any temp/data directories used by the test
- `/tmp` or shared test-lock issues:
  - symptom: test-specific lock or temp-file permission failures
  - fix: prefer no-network test setup when the test does not require networking; avoid unnecessary shared lock files

## Reporting Guidance

When reporting results, separate:

1. Code failures
2. Test harness issues
3. Remote host environment issues

This matters in Hypeman because Linux integration tests often fail before product code executes if the remote machine is missing bundled binaries, system tools, or short enough paths.
