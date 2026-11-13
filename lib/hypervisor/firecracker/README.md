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
`jailer` as `uid`/`gid`, in a chroot at `<instance>/jail/<firecracker binary>/vm/root`.

- The jailer is embedded with each firecracker version, from the same release tarball.
  `binary_path` overrides it and is required with `hypervisor.firecracker_binary_path`.
- Kernel, initrd, and disks are hardlinked into the chroot, so they must share a
  filesystem with the instance. Writable disks and TAP devices are owned by the jail uid.
- The API socket, vsock socket, and serial log keep their host paths, as symlinks
  into the chroot.
- Each boot records the jail inputs in the restore metadata. Snapshot, restore and
  fork follow that record, not the flag. Restoring a jailed snapshot with the flag
  off fails before anything starts.
- AppArmor profiles do not cover the jailed launch yet.

`jailer_integration_test.go` (`-tags jailer_integration`) boots real guests; setup is
in `scripts/jailer-smoke/README.md`.

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
