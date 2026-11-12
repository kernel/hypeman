# Firecracker Hypervisor

This package implements Firecracker support behind the common `lib/hypervisor` interfaces:

- `Starter` (`process.go`): starts/restores a Firecracker process and waits for the API socket.
- `Firecracker` client (`firecracker.go`): configures boot, controls lifecycle, and manages snapshots.
- `VsockDialer` (`vsock.go`): host-initiated vsock connections via Firecracker's UDS handshake.
- Config translation (`config.go`): maps `hypervisor.VMConfig` to Firecracker API models.

## Binaries

Like Cloud Hypervisor, Firecracker binaries are embedded and extracted into `data_dir` at runtime.

- Embedded source path: `lib/hypervisor/firecracker/binaries/firecracker/<version>/<arch>/firecracker`
- Download helper: `make download-firecracker-binaries`
- Runtime override: `hypervisor.firecracker_binary_path` (uses external binary instead of embedded)

Multiple versions are embedded so snapshots can be restored with the Firecracker version recorded in their instance metadata. `defaultVersion` in `binaries.go` controls the version used for new instances; changing it does not migrate running or standby instances.

## Jailer (optional, prototype)

`hypervisor.firecracker_jailer.enabled` runs each VM's firecracker under the
`jailer` as `uid`/`gid`, in a chroot at `<instance>/jail/firecracker/vm/root`.

- Kernel, initrd, and disks are hardlinked into the chroot. Writable disks are
  chowned to the jail uid, so inputs must share a filesystem with the instance.
- The API socket, vsock socket, and serial log keep their host paths. Each is a
  symlink into the chroot.
- TAP devices are owned by the jail uid (`network.SetTAPOwner`).
- Snapshot, standby, restore, and fork work for jailed VMs. Snapshot directories
  record that they were taken jailed, and restore follows that record, not the flag.
  Restoring a jailed snapshot needs the flag on. If it is off, restore fails before
  anything starts, so keep the flag on while jailed standby VMs exist.
- AppArmor profiles do not cover the jailed launch yet. Keep the flag off where
  AppArmor is enforced.

`jailer_integration_test.go` (`-tags jailer_integration`) boots jailed guests and
runs a full/diff standby and restore cycle, jailed and unjailed. Setup is in
`scripts/jailer-smoke/README.md`.

## VM State Mapping

`mapVMState()` maps Firecracker `GET /` state strings to internal states:

- `Not started` -> `created`
- `Running` -> `running`
- `Paused` -> `paused`

These strings are validated against Firecracker's source/spec:

- `src/vmm/src/vmm_config/instance_info.rs`
- `src/firecracker/swagger/firecracker.yaml`

## Rate Limits

Instance bandwidth limits are still instance-level API inputs, but are propagated into
per-interface `hypervisor.NetworkConfig` so Firecracker can program device rate limiters.
Host-level traffic shaping remains handled by Hypeman's network manager.
