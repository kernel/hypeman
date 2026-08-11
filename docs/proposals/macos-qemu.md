# RFC: Secure QEMU support on macOS

> **Status: proposed.** This document describes an Apple Silicon QEMU backend
> using HVF and QEMU's built-in isolated vmnet networking. The networking and
> privilege-drop design has been validated manually, but the production
> launcher, packaging, guest-agent transport, and lifecycle integration have
> not been implemented.

## Summary

Add production-grade support for Hypeman's existing `qemu` hypervisor type on
Apple Silicon. The backend uses QEMU's ARM `virt` board with HVF acceleration,
QEMU's built-in `vmnet-shared,isolated=on` network backend, and direct
host-routable guest IPs. QEMU starts with only the privilege required to create
the vmnet interface, then permanently drops to Hypeman's service UID/GID with
`-run-with` before remaining resident.

This is not a macOS port of `qemu-microvm`. QEMU's `microvm` board is x86-only,
and Hypeman intentionally registers `qemu-microvm` only on Linux/amd64. macOS
uses the existing standard `qemu` profile and its architecture-native ARM
`virt` machine.

The public API remains:

```json
{"hypervisor": "qemu"}
```

There is no `qemu-darwin` type and no public QEMU machine-type field. Host OS,
accelerator, network backend, guest-agent transport, packaged runtime, and
privilege boundary are internal implementation details.

## Motivation and user case

VZ remains the preferred default for ordinary macOS use. It is integrated with
Virtualization.framework, supports Rosetta for amd64 images, has native vsock,
and already supports pause, ballooning, snapshots, standby, and forks.

The QEMU backend serves narrower requirements:

1. **Enforceable peer isolation.** Hypeman's VZ backend creates
   `VZNATNetworkDeviceAttachment` devices but has no API to request or attest
   guest-to-guest isolation. QEMU exposes Apple's vmnet isolation option
   directly. Every macOS QEMU NIC will use `isolated=on`; disabling isolation is
   not representable in the launcher protocol.
2. **QEMU parity across development and deployment hosts.** ARM QEMU guests use
   the same machine family, QMP lifecycle, block layer, and private restore
   contract on Linux and macOS, making QEMU-specific behavior reproducible on a
   developer Mac.
3. **Disk I/O controls.** Standard QEMU already maps Hypeman disk limits to the
   QEMU block throttling layer. VZ reports `SupportsDiskIOLimit: false`. macOS
   QEMU will expose this only after runtime validation.
4. **Pinned, inspectable VMM behavior.** Hypeman can package and retain exact
   QEMU/runtime profiles rather than inheriting all VMM behavior from the host
   macOS release.
5. **QMP diagnostics.** QEMU provides richer process, migration, block, and
   device diagnostics than the current VZ shim API. User-visible diagnostics
   require a separate scoped API; arbitrary QMP access will not be exposed.

Before claiming isolation as a VZ differentiator, run the same two-guest peer
matrix against VZ. The current code does not explicitly configure isolation,
but its observed behavior has not been archived.

## Goals

- Apple Silicon (`darwin/arm64`) and native ARM64 Linux guests.
- HVF acceleration only; no TCG fallback.
- Standard QEMU ARM `virt` machine selected internally.
- Direct host-to-guest IP connectivity for ingress and health checks.
- Outbound NAT and DNS for every network-enabled guest.
- Mandatory bidirectional guest isolation at ARP, ICMP, TCP, and UDP layers.
- No long-lived root QEMU process.
- A narrow, auditable privileged boundary with sealed runtime artifacts.
- Preserve shared QEMU process, QMP, configuration, and future snapshot code.
- Coexist with VZ instances on the same host.

## Non-goals

- QEMU `microvm` on macOS.
- Intel Mac support.
- amd64 guest emulation or TCG.
- SLIRP, application traffic tunnels, ingress tunnels, `socket_vmnet`, or
  `vmnet-helper`.
- Permanent-root QEMU.
- Arbitrary QEMU arguments, machine types, devices, or QMP commands.
- GPU passthrough, network shaping, or memory hotplug in the MVP.
- Snapshot, standby, or warm-fork support in the MVP.
- Replacing VZ as the default macOS backend.

## Experimental evidence

A manual two-VM experiment ran on macOS 26.5.2/arm64 with QEMU 10.1.3 and HVF.
Both guests used:

```text
-netdev vmnet-shared,id=net0,isolated=on
-run-with user=501:20
```

Observed results:

- Guests received distinct addresses (`192.168.2.2` and `192.168.2.3`).
- The host reached both guests over ICMP, TCP/22, and a UDP echo service.
- Both guests reached external TCP endpoints and resolved DNS.
- Guest-to-guest ICMP had 100% loss in both directions.
- Guest neighbor entries became `FAILED`, showing ARP isolation.
- Guest-to-guest TCP and UDP timed out bidirectionally while host controls
  continued succeeding.
- Both QEMU processes had real/effective/saved UID 501 and GID 20 after startup.

These results establish feasibility, not a release artifact. The complete test
must be reproduced by a checked-in harness on a dedicated Apple Silicon host,
with raw logs and checksums retained in CI.

## Relationship to the shared QEMU backend

The `qemu-microvm` work in #376 established the correct abstraction:

```text
Hypeman hypervisor type -> QEMU profile -> shared QEMU implementation
qemu                  -> StandardProfile -> q35/virt
qemu-microvm          -> MicroVMProfile  -> microvm
```

macOS adds a second internal dimension:

```text
QEMU profile + host runtime -> launch behavior
StandardProfile + Linux     -> KVM, TAP, vhost-vsock, direct exec
MicroVMProfile  + Linux     -> KVM, TAP, vhost-vsock, direct exec
StandardProfile + Darwin    -> HVF, isolated vmnet, virtio-serial, launcher
```

`profile.go` continues to own guest-visible board and device-policy differences.
A new internal platform-runtime abstraction owns acceleration, network backend,
guest-agent transport, process execution, packaging, and platform capability
constraints. This avoids forking the QMP, snapshot-config, pool, and process
startup logic into a separate macOS QEMU implementation.

Only profiles supported by the current platform should be registered. Darwin
must not register `qemu-microvm` capabilities or client factories.

## QEMU configuration

Refactor `lib/hypervisor/qemu/config.go` so independent dimensions are not
encoded as Darwin conditionals:

- The QEMU profile selects the board and virtio transport: PCI for the standard
  profile, MMIO for `microvm`.
- The platform runtime selects KVM versus HVF.
- The platform runtime renders TAP versus isolated vmnet networking.
- The platform runtime selects vhost-vsock versus virtio-serial guest control.
- The platform runtime owns process launch and privilege-drop requirements.

The Darwin launcher renders an argv equivalent to:

```text
-no-user-config
-nodefaults
-machine <versioned-virt-profile>,accel=hvf
-cpu host
-smp <validated>
-m <validated>
-kernel <sealed-kernel>
-initrd <sealed-initrd>
-append <fixed-kernel-arguments including console=ttyAMA0>
-blockdev <validated raw disk FDs>
-device virtio-blk-pci,...
-netdev vmnet-shared,id=net0,isolated=on,<sealed-domain-options>
-device virtio-net-pci,netdev=net0,mac=<validated-MAC>
-device virtio-rng-pci
-device virtio-serial-pci
-chardev socket,...
-device virtserialport,...
-mon chardev=qmp,mode=control
-display none
-run-with user=<policy-uid>:<policy-gid>
```

The exact versioned `virt-*` alias is private and persisted in
`qemu-config.json`. It is selected by the installed runtime profile, never by an
API request. Linux QEMU behavior remains unchanged.

## Privileged launcher and trust boundary

Running a user-owned Homebrew QEMU with passwordless sudo would grant effective
root access. Production therefore installs a signed root LaunchDaemon and a
root-owned QEMU bundle with all required dylibs.

The launcher accepts a bounded, versioned semantic request containing only:

- Instance identity and requested runtime profile.
- VCPU and memory limits.
- Opaque kernel/initrd artifact IDs.
- Relative raw-disk identities and access modes.
- A validated locally administered MAC.

The caller cannot select UID/GID, executable paths, dylibs, environment
variables, QEMU arguments, machine type, accelerator, network backend, subnet,
isolation, socket paths, kernel command line, or shell commands.

The launcher:

- Authenticates the Unix-socket peer with `getpeereid` against a root-owned
  service-user policy.
- Verifies root ownership, signatures, hashes, and non-writable ancestors for
  QEMU, dylibs, boot artifacts, and runtime manifests.
- Resolves disks beneath the configured data root with descriptor-relative
  traversal and `O_NOFOLLOW`.
- Accepts only regular raw disk files and passes validated descriptors to QEMU.
- Creates QMP, agent, supervisor, and log endpoints under a root-owned
  per-launch runtime directory.
- Sanitizes the environment and renders the complete argv itself.
- Requires `-run-with` and verifies real/effective/saved UID/GID after QMP
  readiness.
- Identifies processes by an opaque launch generation, not an arbitrary PID.

A per-VM supervisor remains QEMU's parent, reaps the exact child, records exit
status, and survives API or LaunchDaemon restart. Only the LaunchDaemon remains
root after launch.

The baseline must work without Apple's restricted `com.apple.vm.networking`
entitlement. Obtaining it may later remove the root bootstrap, but cannot be a
release dependency.

## Capabilities and preflight

#376 added `VMStarter.ValidateConfig` and complete planned-device validation
before image, network, and filesystem side effects. Preserve that path and
compose profile validation with Darwin runtime validation.

Initial Darwin `qemu` capabilities:

| Capability | MVP |
| --- | --- |
| HVF / ARM64 | yes |
| Direct guest IP | yes |
| Peer isolation | required |
| Outbound NAT/DNS | yes |
| QMP pause/resume | yes |
| Graceful VMM shutdown | yes |
| Guest agent | virtio-serial |
| Vsock | no |
| Disk I/O limit | validation-gated |
| Balloon control | validation-gated |
| GPU/PCI passthrough | no |
| Memory hotplug | no |
| Snapshot/standby/warm fork | no |

Capabilities must be composed from QEMU profile and host runtime. The current
profile-only `qemuCapabilities` reports Linux assumptions such as vsock and PCI
passthrough and cannot be reused unchanged on Darwin.

## Guest-agent transport

QEMU's existing vsock dialer is Linux-only because it uses host AF_VSOCK.
Darwin uses a dedicated virtio-serial port backed by a root-runtime Unix socket.

Refactor the host abstraction from `VsockDialer` to a transport-neutral
`GuestAgentDialer`. Linux QEMU and `qemu-microvm` keep the existing vsock
factory. Darwin `qemu` registers a virtio-serial factory. The guest agent opens:

```text
/dev/virtio-ports/org.kernel.hypeman.agent.0
```

The stream requires an explicit magic/version/nonce handshake before gRPC/HTTP2
starts, plus reconnect and stale-stream resynchronization. This channel carries
Hypeman control operations only; application ingress continues to use the
direct guest IP.

## Networking and IP lifecycle

The current Darwin network manager describes VZ's shared NAT domain as
`192.168.64.0/24` and allocates static guest identities from it. The QEMU vmnet
experiment received addresses from a different vmnet DHCP domain. A single
global Darwin default network is therefore insufficient when VZ and QEMU
instances coexist.

Add backend-aware persistent network domains. Each instance records its domain.
For Darwin QEMU:

1. Reserve a unique MAC; leave IP pending.
2. Launch QEMU with mandatory isolated vmnet networking.
3. Boot the guest with DHCP.
4. Obtain the active IPv4 address from the guest agent.
5. Cross-check it through the root launcher against the vmnet lease data.
6. Verify it belongs to the configured QEMU domain and is not assigned to
   another Hypeman instance.
7. Persist MAC, IP, and network domain atomically.
8. Mark the instance Running only after address and guest-agent readiness.

`StoredMetadata.IP` remains authoritative for Caddy ingress, DNS, and health
checks. No traffic tunnel is introduced. Stop/delete release active address
state only after launcher-confirmed QEMU exit.

A separate archived spike should compare DHCP with static addressing. DHCP is
the default plan because vmnet owns address allocation and exposes no supported
reservation API.

## Process lifecycle

Reuse QEMU's shared QMP client and the generation-safe pool behavior added in
#376. Replace only direct process execution on Darwin.

Evolve startup results to persist a launcher process reference containing an
opaque launch generation and diagnostic PID. Darwin stop, delete, liveness,
startup reconciliation, and fallback kill operations go through the launcher.
The API must never signal a Darwin QEMU PID directly.

On API restart, list launcher-owned generations and reconcile them with stored
instance metadata. Unexpected exit invalidates pooled QMP and guest-agent
connections and records the exact exit status. Root-runtime sockets are removed
only after the supervisor reaps QEMU.

## Snapshots, restore, and forks

Snapshots, standby, and warm forks remain disabled for the MVP. Cold/stopped
disk forks continue using APFS `clonefile`.

When enabled, build on #376 rather than creating a parallel restore format:

- Keep private `MachineType` and `QEMUVersion` in `qemu-config.json`.
- Add the sealed runtime-profile digest, boot-artifact IDs, device-schema
  version, agent transport, and network domain.
- Retain exact runtime bundles side by side so historical snapshots can launch
  with their writer version.
- Restore QEMU through inherited migration FDs, not shell-based `exec:cat`.
- Recreate isolated vmnet, reconnect virtio-serial, renew DHCP, and update IP.
- Warm forks receive new MAC, IP, runtime sockets, launch generation, and guest
  identity.

No snapshot capability is advertised until repeated restore/fork tests pass
across API restart and supported package upgrades.

## Packaging and updates

Install versioned, root-owned runtime profiles containing:

- A minimized `qemu-system-aarch64` build with HVF, vmnet, QMP/migration, raw
  block, ARM `virt`, and required virtio devices.
- All required dylibs with bundle-relative install names.
- Signed kernel/initrd artifacts and compatibility metadata.
- SHA-256 hashes, code-signing requirements, Team ID, entitlements, machine and
  device schema, and launcher protocol compatibility.

Sign dylibs inside-out, then QEMU and the launcher with Developer ID and hardened
runtime. Sign and notarize the installer package. Install versions side by side,
activate through an atomic root-owned manifest, retain versions referenced by
snapshots, and provide an audited rollback command.

Do not execute Homebrew QEMU during a privileged window.

## Validation and release gates

Unit and security tests cover:

- Profile/runtime composition without Linux QEMU or `qemu-microvm` regressions.
- Darwin capability and preflight constraints.
- Launcher schema, peer UID, argument non-representability, and environment
  sanitization.
- Runtime/artifact ownership, signature, hash, traversal, symlink, hard-link,
  and path-swap rejection.
- Supervisor generation, wait, signal, reaping, and API-restart reconciliation.
- Virtio-serial handshake and 100 reconnect cycles.
- DHCP validation, duplicate detection, VZ coexistence, and lease renewal.

The dedicated Apple Silicon integration matrix must prove:

| Source | Destination | Expected |
| --- | --- | --- |
| Host | Guest A/B ICMP, TCP, UDP | success |
| Guest A/B | external TCP and DNS | success |
| A -> B and B -> A ARP | failure |
| A -> B and B -> A ICMP | 100% loss |
| A -> B and B -> A TCP/UDP | timeout |

Repeat host controls after peer-denial tests. Archive QEMU argv, binary and dylib
hashes, signatures, raw network logs, DHCP leases, process credentials, and
checksums. Verify QEMU real/effective/saved UID/GID equal the configured service
identity and supplementary groups exclude root/admin.

Also run the same peer matrix against VZ before documenting isolation as a
backend differentiator.

## Delivery sequence

1. Refactor shared QEMU into profile plus platform runtime with no Linux behavior
   change; keep all `qemu` and `qemu-microvm` tests green.
2. Add platform-composed capabilities, Darwin preflight constraints, and
   transport-neutral guest-agent dialing.
3. Add the root launcher, supervisor, sealed runtime/artifact store, and a
   network-disabled Darwin boot.
4. Add HVF, isolated vmnet, DHCP identity, direct ingress, and archive the
   two-guest security matrix.
5. Add virtio-serial guest control, complete lifecycle reconciliation,
   diagnostics, signed installer, stress testing, upgrade, and rollback.
6. Separately gate disk throttling, ballooning, snapshots, standby, and warm
   forks as evidence supports them.

## References

- [Hypeman #376: QEMU microvm backend](https://github.com/kernel/hypeman/pull/376)
- [QEMU vmnet schema](https://github.com/qemu/qemu/blob/master/qapi/net.json)
- [QEMU invocation and `-run-with`](https://www.qemu.org/docs/master/system/invocation.html)
- [UTM](https://github.com/utmapp/UTM) and its [QEMU argument generation](https://github.com/utmapp/UTM/blob/main/Configuration/UTMQemuConfiguration%2BArguments.swift)
- [Apple `com.apple.vm.networking` entitlement](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.vm.networking)
