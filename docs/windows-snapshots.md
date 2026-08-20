# Windows snapshots and forks

Windows 11 QEMU instances support standby, restore, stopped snapshots, and forks. Snapshot payloads treat the writable qcow2 disk, Secure Boot NVRAM, software TPM state, saved QEMU configuration, and memory image as one machine.

## Same-instance standby and restore

Standby pauses QEMU, captures memory and device state, stops QEMU and swtpm, and retains the instance disk, NVRAM, and TPM directory. Restore starts swtpm from that same state before loading QEMU memory. The Windows machine identity and TPM remain unchanged.

## Fork identity

A fork receives independent disk and NVRAM files. Hypeman removes the copied TPM state before the child starts, so swtpm initializes a new TPM rather than cloning the parent's identity. The Windows guest agent then writes a new `MachineGuid` and records the child instance ID before the child is returned.

Fork admission requires the persona OCI label:

```text
io.hypeman.machine-image.bitlocker=disabled
```

Personas marked `reseal-required`, unlabeled personas, and unknown policies can still use same-instance snapshots, but cannot be forked. Hypeman does not expose a child whose encrypted disk was cloned without resealing it to the child's TPM.

Stopped forks cold-boot with a unique vsock CID and can run concurrently. A standby snapshot contains the Windows VioSock driver's current CID in guest memory, so a memory-restored child initially retains that CID. The source and child must not be restored concurrently until the child has been stopped and cold-started; Hypeman reports a state error instead of allowing QEMU to fail with a CID collision. Creating a running fork directly from a running Windows source therefore requires `target_state=Stopped`.

## Integration gates

- `TestWindowsStandbyRestoreIntegration` verifies identity-preserving standby/restore and the stopped snapshot restore/fork APIs.
- `TestWindowsStandbyForkIntegration` verifies a captured desktop memory fork, inherited guest state, fresh machine identity and TPM state, and independent NVRAM/disk files.
- `TestWindowsForkIntegration` verifies independent guest writes and cold-booted stopped forks.

The private Windows fixture and its license are not stored in this repository.
