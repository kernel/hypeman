# Windows machine images

Hypeman accepts Windows desktop disks as OCI images for `windows/amd64`. Ordinary Windows container images are not bootable and are rejected.

A machine image uses these OCI config labels:

| Label | Base | Image |
|---|---|---|
| `io.hypeman.machine-image.version` | `1` | `1` |
| `io.hypeman.machine-image.kind` | `windows-base` | `windows-image` |
| `io.hypeman.machine-image.disk-path` | relative path to the source disk | relative path to a qcow2 delta |
| `io.hypeman.machine-image.disk-format` | `raw`, `qcow2`, `vhd`, or `vhdx` | `qcow2` |
| `io.hypeman.machine-image.base` | omitted | digest-pinned base reference |
| `io.hypeman.machine-image.tpm` | `2.0` | `2.0` |
| `io.hypeman.machine-image.secure-boot` | `required` | `required` |
| `io.hypeman.machine-image.bitlocker` | omitted | `disabled` for forkable personas; `reseal-required` otherwise |

The base must be pulled before its dependent Windows images. A base cannot be deleted while any cached image references its digest. Instance references are not tracked by the image cache, matching existing Linux behavior: do not delete a base while a dependent Windows instance exists.

Windows installation media, activation material, credentials, and generated disks belong in private registries and must not be committed to this repository.
