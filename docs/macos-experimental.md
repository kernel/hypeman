# Experimental macOS guests (local Apple silicon only)

This spike supports **offline local image import and the normal instance API's
create/list/get/start/stop/delete path**. It is not production macOS support.
Linux guests retain the existing defaults and boot path.

## Import

Use a stopped `spike/macvm` bundle (`config.json`, `disk.img`, `aux.img`) on
Darwin/arm64. Configure its user, SSH, FileVault and desktop before import.
The importer verifies that storage is not open, clones disk and auxiliary
storage, hashes their contents and identity/configuration into a digest, and
publishes a local ready image. Import is an administrator CLI operation, not
an HTTP endpoint accepting arbitrary host paths.

```sh
go run -tags containers_image_openpgp ./cmd/import-macos \
  --data-dir /absolute/path/to/data \
  --source /absolute/path/to/stopped-bundle \
  --name localhost/macos:spike
```

The local importer needs APFS clonefile support. Images are `darwin/arm64`
machine images, not OCI macOS containers. Registry pulls also support experimental
complete-machine bundles described below. Additional tags within the same
repository are supported; cross-repository promotion remains unsupported.
Do not mutate the imported image files.

## Experimental OCI bundles

The normal image API accepts a `darwin/arm64` OCI manifest whose configuration
labels describe a complete installed machine:

| Label | Value |
|---|---|
| `io.hypeman.machine-image.version` | `1` |
| `io.hypeman.machine-image.kind` | `macos-image` |
| `io.hypeman.machine-image.disk-format` | `raw` |
| `io.hypeman.machine-image.disk-path` | Relative installed boot disk path |
| `io.hypeman.machine-image.aux-path` | Relative auxiliary storage path |
| `io.hypeman.machine-image.platform-path` | Relative macOS platform configuration JSON path |

All three files must be distinct, nonempty regular files inside the unpacked
image. Platform configuration uses the local bundle's `MacOSImage` schema and
is limited to 64 KiB. Absolute paths, traversal and symlink escapes are rejected.
The installed disk is materialized directly rather than converted to a Linux
root filesystem. The matching auxiliary storage and platform identity are
retained. Do not package live mutable guest storage.

This schema is experimental pending reconciliation with other machine-image
platforms. Complete bundles only: no base/delta chain, identity rebinding, or
concurrent-template guarantee. Ordinary OCI transport does not imply sparse
distribution efficiency, boot portability, or container execution semantics.

## API

Use the usual JWT-authenticated API with a VZ-capable, signed build:

```json
{
  "name": "macos-desktop",
  "image": "localhost/macos:spike",
  "platform": "darwin/arm64",
  "network": {"enabled": true}
}
```

CPU and memory default to the imported template (minimum 2 CPUs and 4GB).
Each instance gets its own writable complete boot disk and auxiliary storage;
no Linux kernel, initrd, configuration disk, overlay filesystem or balloon is
attached. NAT uses the preserved MAC. The IP is observed from the host's VZ
DHCP leases, which may retain an old lease before the guest is ready.

**`Running` means the VMM is running, not that SSH, the desktop or Chrome is
ready.** Unmanaged templates still need an external readiness check. Templates
provisioned with the shared Darwin GuestService can opt in with `guest_agent: true`
in their platform configuration; system readiness is observed separately through
`guest_agent_ready_at`. See [Darwin GuestService](macos-guest-agent.md). Startup
does not launch Chrome.
Guest identity, host keys, user secrets and MAC are preserved. Only one active
instance with a given Mac identifier is allowed. Do not run the source bundle
or an external clone concurrently. Production provisioning needs identity and
credential rekeying; this spike is not a multi-tenant image format.

`POST /instances/{id}/stop` attempts orderly GuestService shutdown for opted-in
images, then falls back to VMM shutdown if necessary. Without an enabled Darwin
guest agent it does **not** guarantee an orderly guest OS/application shutdown. Use a human
or an authorized guest shutdown workflow before destructive operations when
application consistency matters. Start cold-boots the instance's existing disk.
Delete removes instance storage, not the imported image. Deleting the imported
image does not affect existing instances: each owns independent copies and
starts without the template.

Unsupported instance operations reject requests: snapshot/fork/standby/restore,
updates, volumes, startup env/commands/credential brokering,
health/restart/auto-standby policies, passthrough and shaping. Exec/file-copy over
vsock require an opted-in shared agent; unmanaged images reject those requests.
The shim's standalone save/restore proof does not make API snapshot semantics
safe; consistent disk+aux+state bundle handling remains separate work.

An opt-in `macos_only: true` config requires Darwin/arm64 and VZ, refuses Linux
instance creates/starts and skips Linux kernel/initrd downloads, builders and
Caddy/DNS initialization. Ingress creation and builds are rejected in that mode.
The default is false. `listen_address: 127.0.0.1` can bind the HTTP API locally.
No host sudo, bridge creation or launchd installation is required for the spike.

For controlled Chrome testing after autologin, use `open -na "Google Chrome"`
with a dedicated `--user-data-dir` and guest-local `--remote-debugging-port=9222`.
macOS can reopen Chrome after a power-off without the original debugging flags;
`open -a` may just activate that existing process and ignore the flags. Tunnel
CDP via SSH rather than exposing it on the guest network.
