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

The local importer needs APFS clonefile support. The data directory path must be
short enough that each guest's sockets fit the 103-byte macOS limit; a boot that
would exceed it is refused. Images are `darwin/arm64` machine images, not OCI
macOS containers. Registry pulls also support experimental complete-machine
bundles described below. Additional tags within the same
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
root filesystem. Machine layers are unpacked sparsely (zero chunks become holes),
accept only regular files (not GNU sparse tar entries) and directories, and are
checked against their diff IDs, so a mostly empty boot disk occupies only its
data. A bundle's layers may decompress to at most 1 TiB in total. The boot disk
is stored as `rootfs.raw`, as for a local import; machine images pulled before
that naming must be deleted and pulled again. The matching auxiliary storage and platform
identity are retained. Do not package live mutable guest storage.

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
Each instance gets its own writable complete boot disk (`boot.raw`) and auxiliary
storage; instances created before that naming must be deleted and recreated. No
Linux kernel, initrd, configuration disk, overlay filesystem or balloon is
attached. A networked guest gets its own vmnet network (macOS 26 and later):
hypeman assigns it a /29 from `network.macos_subnet_cidr` (default
`192.168.128.0/20`) and vmnet's DHCP serves the guest a reservation for its
preserved MAC. The guest's address is stored with the instance, kept through stop
and standby, and reported whether or not the guest is running. Delete releases it,
and a released address is not given to another guest for five minutes, longer than
ingress caches upstream addresses. Clones and forks get a new address. Instances
started before this change get an address at their next start; until then they
stay on vmnet's shared NAT and report none.

**Without the guest agent, `Running` means the VMM is running, not that SSH,
the desktop or Chrome is ready.** Unmanaged templates still need an external
readiness check. Templates provisioned with the shared Darwin GuestService can opt
in with `guest_agent: true` in their platform configuration; those instances stay
`Initializing` until the agent answers, as Linux guests do, so waiting for
`Running` waits for the agent. See [Darwin GuestService](macos-guest-agent.md).
Startup does not launch Chrome.
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

`POST /instances/{id}/stop` attempts orderly GuestService shutdown for opted-in
images, then falls back to VMM shutdown if necessary. Without an enabled Darwin
guest agent it does **not** guarantee an orderly guest OS/application shutdown. Use a human
or an authorized guest shutdown workflow before destructive operations when
application consistency matters. Start cold-boots the instance's existing disk.
Delete removes instance storage, not the imported image. Deleting the imported
image does not affect existing instances: each owns independent copies and
starts without the template.

Unsupported operations are rejected explicitly. The status of each operation is kept in
[macos-support.md](macos-support.md), not repeated here.

An opt-in `macos_only: true` config requires Darwin/arm64 and VZ, refuses Linux
instance creates/starts and skips Linux kernel/initrd downloads and builders.
Builds are rejected in that mode. Caddy and DNS start as on Linux.
The default is false. `listen_address: 127.0.0.1` can bind the HTTP API locally.
No host sudo, bridge creation or launchd installation is required for the spike.

For controlled Chrome testing after autologin, use `open -na "Google Chrome"`
with a dedicated `--user-data-dir` and guest-local `--remote-debugging-port=9222`.
macOS can reopen Chrome after a power-off without the original debugging flags;
`open -a` may just activate that existing process and ignore the flags. To reach
CDP from off the host, route it through ingress (below) or tunnel it via SSH.

## Ingress

Ingress routes to macOS guests as it does to Linux guests: Caddy on the host
resolves the target instance through hypeman's DNS server on each request and
proxies to the guest's IP, the address hypeman assigned it. A guest that is not
running has no network at that address, so Caddy returns 502; the address is never
another guest's. As on Linux, the guest service must listen on the guest's network
interface, not only on loopback.

Caddy is not embedded in macOS builds. `make build-caddy-darwin` builds the same
Caddy version and DNS modules as Linux into `bin/caddy`. Hypeman looks for `caddy`
next to its own binary, then on `PATH`, and fails to start without it.

Caddy needs local network access to reach guests. On macOS 15.5 and later, allow
the guest pool for every process once per host, then restart (Apple TN3179):

```sh
sudo defaults write com.apple.network.local-network AllowedEthernetLocalNetworkAddresses -array "192.168.128.0/20"
```

Without it, only root has access. On the QA host (macOS 27.0), a launchd daemon
running as a regular user was refused, and so was a hypeman left running after the
SSH session that started it ended: dials to guests failed with "no route to host"
and Caddy returned 502. With the subnet allowed, the same daemon reached guests.

Starting a macOS guest also needs the hypeman user logged in on the host. After a
host restart with nobody logged in, start failed with `VZErrorDomain Code=-9`
("Unable to access security information") and succeeded once the user logged in.

Saving a macOS guest's memory (standby and memory snapshots) also needs that
user's screen unlocked. VZ encrypts saved state with a Secure Enclave key that
macOS creates only while the user's data is unlocked, and a locked screen locks
it. On the QA host, standby failed with `VZErrorDomain Code=11` ("permission
denied"; the log shows `errSecInteractionNotAllowed` from the key creation) until
the screen was unlocked. Turn off the automatic screen lock for that user:

```sh
sysadminctl -screenLock off -password -
```

Each guest's network is separate, so guests cannot reach each other's addresses.
A guest can still reach anything the host listens on, including Caddy's listeners,
sshd and Screen Sharing, so hosts that run guests for different tenants should not
expose other host services on guest networks. Ingress hostnames reach a guest
through Caddy, as they do from outside.
