# Experimental macOS guests (local Apple silicon only)

This spike supports **offline local image import and the normal instance API's
create/list/get/start/stop/delete path**. It is not production macOS support.
Linux guests retain the existing defaults and boot path.

## Import

Use a stopped macOS bundle (`config.json`, `aux.img`, and `disk.asif` or a raw
`disk.img`) on Darwin/arm64. Configure its user, SSH, FileVault and desktop before
import. The importer verifies that storage is not open, clones an ASIF disk or
converts a raw one to ASIF (all-zero 1 MiB chunks stay unallocated; a 100 GB disk
took about 10 minutes on the QA host), clones auxiliary storage, hashes their
contents and identity/configuration into a digest, and publishes a local ready image. Import is an administrator CLI operation, not
an HTTP endpoint accepting arbitrary host paths.

```sh
go run -tags containers_image_openpgp ./cmd/import-macos \
  --data-dir /absolute/path/to/data \
  --source /absolute/path/to/stopped-bundle \
  --name localhost/macos:spike
```

The local importer needs APFS clonefile support, and ASIF disks need macOS 26 or
later; the VZ shim needs macOS 27. The data directory path must be
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
| `io.hypeman.machine-image.version` | `2` |
| `io.hypeman.machine-image.kind` | `macos-image` |
| `io.hypeman.machine-image.disk-format` | `asif` |
| `io.hypeman.machine-image.disk-path` | Relative ASIF disk path, in every layer |
| `io.hypeman.machine-image.aux-path` | Relative auxiliary storage path |
| `io.hypeman.machine-image.platform-path` | Relative macOS platform configuration JSON path |

Each layer carries one ASIF disk, and each layer's disk is an overlay on the
previous layer's, so the layers form the boot disk's chain, base first. The
auxiliary storage and platform come from the top layer; lower layers' copies are
ignored. A base image is one layer; an image built on it adds one layer holding
only the build's writes. A pull clones any lower layer whose diff ID a ready image
on the host already has instead of unpacking it, so images built on one base share
its disk. All files must be distinct, nonempty regular files inside the unpacked
image. Platform configuration uses the local bundle's `MacOSImage` schema and
is limited to 64 KiB. Absolute paths, traversal and symlink escapes are rejected.
The installed disk is materialized directly rather than converted to a Linux
root filesystem. Machine layers are unpacked sparsely (zero chunks become holes),
accept only regular files (not GNU sparse tar entries) and directories, and are
checked against their diff IDs. A bundle's layers may decompress to at most 1
TiB in total. The chain is stored as `disk.{n}.asif`, as for a local import, and
must open as an ASIF stack. Machine images from before ASIF (version `1`, `raw`)
are refused and must be rebuilt or reimported. The matching auxiliary storage and
platform identity are retained. Do not package live mutable guest storage.

This schema is experimental pending reconciliation with other machine-image
platforms. There is no identity rebinding on pull or concurrent-template guarantee. Ordinary OCI transport does not imply sparse
distribution efficiency, boot portability, or container execution semantics.

## Building images with Packer and Tart

hypeman does not build macOS images itself. Build them with Packer's Tart builder
on a macOS 27 host, as stacked Tart VMs, then publish the result with
`import-tart`. A stacked VM shares a read-only base disk and writes only to its own
`overlay.asif`, so a build on a base already in the registry uploads only that
overlay.

1. Publish the base once from a standalone Tart VM whose `disk.img` is ASIF
   (`diskFormat: asif`). Every layer is uploaded.
2. Get the base as a Tart image (`tart push` to any OCI registry) and create the
   build VM with `tart clone --stacked`. The Packer Tart plugin cannot clone
   stacked itself, so point its `vm_base_name` at that local clone.
3. Build with `images/macos/browser.pkr.hcl`. It installs Chrome (from Google's
   stable DMG, checked by its code signature and Gatekeeper), runs any
   `extra_scripts`, installs hypeman's agent launcher (hypeman supplies the agent
   on its own disk; see [macos-guest-agent.md](macos-guest-agent.md#agent-disk)),
   checks the image with `images/macos/smoke-test.sh` (launcher present, no baked
   agent, headless Chrome answers CDP), regenerates the SSH host keys and shuts the
   guest down from inside. It keeps the recovery partition; Packer's default
   deletes it and rewrites the whole disk. The base must have an SSH user with sudo
   for Packer.

   ```sh
   cd images/macos && packer init browser.pkr.hcl
   packer build -var vm_base_name=base-clone -var vm_name=my-build \
     -var ssh_username=admin -var ssh_password=... browser.pkr.hcl
   ```

4. Publish the stopped VM over the base:

```sh
HYPEMAN_JWT_SECRET=... go run ./cmd/import-tart \
  --vm ~/.tart/vms/my-build \
  --registry https://hypeman.example \
  --name my-build \
  --base hypeman.example/builds/my-base@sha256:... \
  --guest-agent
```

The `macOS image` workflow (`.github/workflows/macos-image.yml`, run by hand) does
steps 2 to 4 on a self-hosted macOS 27 runner labelled `tart`. It needs the
`HYPEMAN_REGISTRY_URL` and `MACOS_IMAGE_SSH_USERNAME` variables and the
`HYPEMAN_JWT_SECRET`, `MACOS_IMAGE_SSH_PASSWORD`, `TART_REGISTRY_USERNAME` and
`TART_REGISTRY_PASSWORD` secrets.

The image is published as `builds/<name>`. `import-tart` reads the VM's layers from
Tart's content store in the order its `manifest.json` lists them, refuses a
suspended or running VM, and checks that the overlay stacks on those layers. With
`--base`, the VM's layers must be that image's layers, and only the config and
overlay are uploaded. Each image gets a fresh machine identifier and MAC, since
Tart keeps its source's; the SSH host keys are the template's job. The registry
imports a pushed image on its host, as `builds/<name>@sha256:...`. Other hosts pull
it by digest with registry credentials.

Building on a build needs that build as a Tart image too: `tart clone --stacked`
only stacks on images pulled from a registry, and a local clone copies the
overlay instead of stacking on it.

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
Each instance gets clones of the image's ASIF layers (`disk.{n}.asif`), its own
auxiliary storage, and an empty writable ASIF overlay (`boot.asif`) above the
layers; the guest's writes only grow the overlay. Instances created before ASIF
must be deleted and recreated. No
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

The guest's vz-shim owns its vmnet network. If the shim is killed while the guest
runs (SIGKILL, a crash), macOS keeps the network and refuses to create that subnet
again until the host restarts, so that guest cannot start and its address cannot be
reused. Stop and delete therefore ask the shim to stop the VM and exit, and kill it
only if it does not.

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
