# Windows machine images

Hypeman accepts Windows desktop disks as OCI images for `windows/amd64`. Ordinary Windows container images are not bootable and are rejected.

A machine image uses these OCI config labels:

| Label | Base | Persona |
|---|---|---|
| `io.hypeman.machine-image.version` | `1` | `1` |
| `io.hypeman.machine-image.kind` | `windows-base` | `windows-persona` |
| `io.hypeman.machine-image.disk-path` | relative path to the source disk | relative path to a qcow2 delta |
| `io.hypeman.machine-image.disk-format` | `raw`, `qcow2`, `vhd`, or `vhdx` | `qcow2` |
| `io.hypeman.machine-image.base` | omitted | digest-pinned base reference |
| `io.hypeman.machine-image.tpm` | `2.0` | `2.0` |
| `io.hypeman.machine-image.secure-boot` | `required` | `required` |

Hypeman materializes the base as immutable sparse raw. It rewrites the persona's qcow2 backing header to the cache-owned base path, ignoring any artifact-supplied backing path. At instance creation, Hypeman reflink-clones the immutable persona into a writable `windows.qcow2`; the clone remains backed directly by the raw base.

The base must be pulled before its personas. A base cannot be deleted while any cached persona references its digest.

Windows installation media, activation material, credentials, and generated disks belong in private registries and must not be committed to this repository.
