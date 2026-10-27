# macOS snapshots, schedules and restore (PR 4, draft)

Experimental: stopped disk snapshots, same-instance memory checkpoints, snapshot
schedules, and cold forks from gracefully stopped instances or snapshots.
Independent warm memory forks are unsupported. Native validation of the memory
snapshot and schedule paths is still required.

## Stopped snapshots and cold forks

- `kind: Stopped` requires a stopped source and a graceful shutdown receipt: after
  the shutdown request, the vz-shim recorded that the guest powered the VM off
  itself, and the owned VMM exited. VMM exit alone does not count, and a forced
  stop is refused with 409. A shim started before this receipt existed never
  writes one, so its guest needs a fresh boot and stop.
- Restore targets are Stopped or Running. The source instance, hardware model,
  machine identifier and MAC must match the snapshot.
- A stopped-snapshot fork requires a graceful receipt and an enabled guest agent.
  Disk and auxiliary storage are copied, the clone gets a fresh machine identifier
  and MAC, and SSH host keys are rekeyed on first boot. The default target is
  Stopped; Running cold-boots and completes rekeying before returning.
- Forking a stopped instance follows the same identity and rekey contract. Hostnames,
  account credentials and application identities are not rekeyed.
- A failed rekey stops the clone and leaves rekey pending for a later retry.

## Same-instance memory snapshots

`POST /instances/{id}/snapshots` with `{"kind":"Standby"}`.

- A Running source is paused, its VZ machine state saved, and the VMM shut down
  with confirmed exit. The disk, auxiliary storage and machine state are copied
  while the VMM is down, then the source resumes from its own checkpoint.
- A Standby source is copied without being resumed. Stopped and other states are
  rejected with 409.
- Before pausing, the shim validates the exact VM configuration with Apple's
  `validateSaveRestoreSupport`. Host and architecture capability checks alone do
  not prove that the guest's devices support save and restore.
- Pause waits for raw Paused state, not transitional Pausing, before save. Error-path
  resume and source recovery use bounded cleanup contexts that survive cancellation
  of the request. If recovery fails, the API reports it and the source may need
  explicit recovery.
- VZ machine-state files are uncompressed. Explicit enabled compression is rejected
  before capture; Linux compression defaults do not apply.

Restore with `POST /instances/{id}/snapshots/{snapshot_id}/restore`.

- Same-instance only, not a clone. The source must be inactive (Stopped or Standby);
  Running, Paused and other live states are refused.
- The default target for a memory snapshot is Running. Explicit Standby installs the
  checkpoint without resuming it. Stopped discards memory and creates no graceful
  receipt, so a clean boot and stop are needed before a stopped snapshot or cold fork.
- Preflight checks CPU/RAM, hardware model, machine identifier, NIC MAC, disk and
  auxiliary paths, and the full memory/disk/auxiliary bundle before storage is
  replaced. An invalid payload leaves current storage untouched.
- Restore stages the payload and restored metadata (including the current name and
  expiration) in `restore-staging/` inside the instance directory, then swaps the
  instance's files in place. Deleting the instance always removes its staging. A
  crash before the swap commits rolls back to the previous files; after it, server
  start or the next restore finishes the install. Host phase accounting is preserved rather than replayed from the
  checkpoint; a memory restore keeps its boot time. An explicit Stopped target
  suppresses automatic restart.
- A restore never mutates the persistent snapshot. If the framework restore fails
  after installation, the restored checkpoint remains available in Standby.
- Once the shim has loaded the VM, the checkpoint is retired before the guest
  resumes, so a later crash cannot restore old memory onto a newer disk. If retiring
  it fails, the VM is shut down and the checkpoint is kept. A resume that fails after
  it was retired leaves the instance Stopped with no graceful receipt.
- If the API dies during a restore, the shim it started keeps loading the VM. Until
  it opens its socket the instance reads Initializing. Restore returns 409 while
  that shim has not resumed the VM, and keeps the checkpoint. Once loaded it reads
  Paused: `POST /restore` retires the checkpoint, resumes the VM and records the
  shim, and DELETE kills it. As on Linux, stop does not accept Paused.
- Saved state is not portable. Use the originating Mac and OS account, a compatible
  framework version and the same VM configuration. API availability on macOS 14+
  does not guarantee support for a particular guest configuration.

## Schedules

Schedules reuse the existing snapshot-schedule API, interval handling and retention.
They require host save/restore support, and admission probes the exact configuration
when a live VM is available.

- A gracefully Stopped source produces a stopped snapshot. A Running source produces
  a memory snapshot and is resumed afterwards. A Standby source produces a memory
  snapshot and stays in Standby.
- An ineligible state, a missing graceful receipt, or a configuration or capture
  failure is recorded in `last_error`, and no snapshot ID is reported for that run.
- Failed runs consume the interval, matching the existing scheduler. The next run
  follows the configured cadence rather than retrying tightly.
- Scheduling never stops a running guest to manufacture a graceful disk snapshot.

## Warm memory forks are not supported

`ForkSnapshot` rejects macOS memory snapshots with 501, and `ForkInstance` requires a
gracefully stopped macOS source. VZ restores saved state only into an identical
configuration: on Apple silicon, changing the MAC, machine identifier, NIC or
auxiliary storage fails with "invalid argument", while relocating paths works.
Apple documents that concurrent VMs sharing a machine identifier have undefined guest
behavior, so copies of one saved state cannot run side by side. Repeated same-instance
restore is the supported warm start.

References:
- [Apple: Mac platform configuration](https://developer.apple.com/documentation/virtualization/vzmacplatformconfiguration)
- [Apple: restoring machine state](https://developer.apple.com/documentation/virtualization/vzvirtualmachine/restoremachinestatefrom(url:completionhandler:))

## Validation

Linux-runnable tests cover capture, restore, isolation, manifest checks, schedules,
retention and crash-swap recovery. Mock HTTP fixtures cover VZ pause, cancellation
and configuration probing. Native Apple-silicon runs have exercised Running capture
and resume, same-instance rollback, repeated restore, schedules with retention,
cancellation after pause, API crash during restore, and stopped clones with fresh
identities.
