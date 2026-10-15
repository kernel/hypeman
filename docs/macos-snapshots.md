# macOS snapshots, schedules and restore (PR 4, draft)

Supported experimentally: stopped disk snapshots, same-instance memory checkpoints,
snapshot schedules, and cold forks from gracefully stopped instances or snapshots.
Independent warm memory forks remain unsupported. Native validation of the new
memory-snapshot and schedule paths is still required.

## Stopped snapshots and cold forks

- `kind: Stopped` requires a stopped source and a graceful shutdown receipt:
  acknowledged guest shutdown with confirmed owned VMM exit. A forced stop is
  refused with 409.
- Restore targets are Stopped or Running. The source instance, hardware model,
  machine identifier and MAC must match the snapshot.
- A stopped-snapshot fork requires a graceful receipt and an enabled guest agent.
  Disk and auxiliary storage are copied, the clone gets a fresh machine identifier
  and MAC, and SSH host keys are rekeyed on first boot. The default target is
  Stopped; Running cold-boots and completes rekeying before returning.
- Forking a stopped instance uses the same identity/rekey contract. Hostnames,
  account credentials and application-specific identities are not rekeyed.
- Failed rekeying stops the clone and leaves rekey pending for a later retry.

## Same-instance memory snapshots

Use `POST /instances/{id}/snapshots` with `{"kind":"Standby"}`.

- A Running source goes through pause, VZ machine-state save and confirmed VMM
  shutdown. The disk, auxiliary storage and machine state are copied together
  while the VMM is down; then the source resumes from its own checkpoint.
- A Standby source is copied without being resumed. Stopped and other source
  states are rejected with 409.
- Before pausing, the shim validates the exact VM configuration using Apple's
  `validateSaveRestoreSupport`. Host/architecture capability checks alone are
  not proof that the guest's configured devices support save/restore.
- Pause waits for raw Paused state, not transitional Pausing state, before save.
  Error-path resume and source recovery use bounded cleanup contexts that survive
  cancellation of the snapshot request. If recovery fails, the API reports the
  failure; the source may need explicit recovery.
- VZ machine-state files stay uncompressed. Explicit enabled compression is
  rejected before capture; Linux server compression defaults do not apply.

Restore using `POST /instances/{id}/snapshots/{snapshot_id}/restore`.

- This is a same-instance operation, not a clone. Restore requires an inactive
  Stopped or Standby source; Running, Paused and other live states are refused.
- The default target for a memory snapshot is Running. Explicit Standby installs
  the checkpoint without resuming it. Stopped discards memory and does not create
  a graceful-shutdown receipt; a subsequent clean boot/stop is required before
  making a stopped snapshot or cold fork.
- Preflight checks saved CPU/RAM, hardware model, machine identifier, NIC MAC,
  disk/auxiliary paths and the complete memory/disk/auxiliary bundle before
  replacing source storage. Invalid payloads leave current storage untouched.
- Restored metadata is staged with the payload, including the current instance
  name and expiration. Directory-swap recovery therefore commits or rolls back
  the matching metadata and storage together. Host phase accounting is preserved,
  rather than replayed from the checkpoint; memory restore retains its boot time.
  An explicit Stopped target suppresses automatic restart.
- Restoring never mutates the persistent snapshot. Framework restore failure
  after installation leaves the restored checkpoint available in Standby.
- If the API dies during a restore, the shim it started keeps loading the VM.
  Until it opens its socket the instance reads Standby, and start or restore
  return 409 while that shim runs. Once loaded it reads Paused: `POST /restore`
  resumes it and completes the restore (records the shim, removes the consumed
  checkpoint), and DELETE kills it. As on Linux, stop does not accept Paused.
- Saved state is not portable: use the originating Mac and OS account, with a
  compatible framework version and VM configuration. API availability on macOS
  14+ does not guarantee support for a particular macOS guest configuration.

## Schedules

The existing snapshot-schedule API, interval handling and retention policies are
shared with Linux. macOS schedules require host save/restore support; admission
also probes the exact configuration when a live VM is available.

- A gracefully Stopped source produces a stopped snapshot.
- A Running source produces a memory snapshot and is resumed afterwards.
- A Standby source produces a memory snapshot and remains in Standby.
- Ineligible state, absent graceful receipt, configuration or capture failure is
  recorded in `last_error`; no successful snapshot ID is reported for that run.
- Failed runs consume the interval, matching existing scheduler behavior. The
  next run follows the configured cadence rather than tight retries.
- Retention and schedule deletion use the existing controller. Scheduling does
  not stop a running guest to manufacture a graceful disk snapshot.

## Warm memory forks are not supported

`ForkSnapshot` rejects macOS memory snapshots with 501, and `ForkInstance`
requires a gracefully stopped macOS source. VZ restores saved state only into an
identical configuration: on Apple silicon, changing the MAC, the machine
identifier, the NIC or the auxiliary storage fails with "invalid argument", while
relocating paths works. Apple documents that two VMs running concurrently with
the same machine identifier have undefined guest behavior, so copies of one saved
state cannot run side by side. Repeated same-instance restore is the supported
warm start.

References:
- [Apple: Mac platform configuration](https://developer.apple.com/documentation/virtualization/vzmacplatformconfiguration)
- [Apple: restoring machine state](https://developer.apple.com/documentation/virtualization/vzvirtualmachine/restoremachinestatefrom(url:completionhandler:))

## Validation

Linux-runnable tests cover the filesystem and metadata paths (capture, restore,
isolation, manifest checks, schedules, retention and crash-swap recovery); mock
HTTP fixtures cover VZ pause, cancellation and configuration probing. Native
Apple-silicon runs have exercised Running capture/resume, same-instance memory
rollback, repeated restore, schedules with retention, cancellation after pause,
API crash during restore, and stopped clones with fresh identities.
