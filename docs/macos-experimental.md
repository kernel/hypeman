# Experimental macOS guests (local Apple silicon only)

This spike supports **offline local image import and the normal instance API's
create/list/get/start/stop/delete path**. It is not production macOS support.
Linux guests retain the existing defaults and boot path.

## Import

Use a stopped macOS bundle (`config.json`, `disk.img`, `aux.img`) on
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

The data directory needs APFS clonefile support. Images are `darwin/arm64`, not
OCI macOS containers. Registry pulls, manifest resolution and tagging/promotion
of macOS images are unsupported. Do not mutate the imported image files.

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
ready.** Readiness is currently tested over SSH. Startup does not launch Chrome.
A shim killed without cleanup (for example, SIGKILL) leaves its control socket
behind; when nothing accepts on it and the recorded shim is gone, the instance
reads as Stopped and its resources are released for admission. If the API dies
mid-boot or mid-restore, the shim it started may still be loading the VM before
it opens its socket. Start and restore return 409 while such a shim runs, and
stop and delete find it by its socket path even when its PID was never recorded.
Guest identity, host keys, user secrets and MAC are preserved. Only one active
instance with a given Mac identifier is allowed. Do not run the source bundle
or an external clone concurrently. Production provisioning needs identity and
credential rekeying; this spike is not a multi-tenant image format.

`POST /instances/{id}/stop` shuts down the VMM; without a Darwin guest agent,
it does **not** guarantee an orderly guest OS/application shutdown. Use a human
or an authorized guest shutdown workflow before destructive operations when
application consistency matters. Start cold-boots the instance's existing disk.
Delete removes instance storage, not the imported image. Deleting the imported
image does not affect existing instances: each owns independent copies and
starts without the template.

Unsupported operations are rejected explicitly. The status of each operation is kept in
[macos-support.md](macos-support.md), not repeated here.

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
