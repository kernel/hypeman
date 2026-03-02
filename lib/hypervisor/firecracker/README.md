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
