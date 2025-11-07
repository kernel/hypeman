# Image Manager

Converts OCI container images into bootable ext4 disk images for Cloud Hypervisor VMs.

## Architecture

```
OCI Registry → containers/image → OCI Layout → umoci → rootfs/ → mkfs.ext4 → disk.ext4
```

## Design Decisions

### Why containers/image? (oci.go)

**What:** Pull OCI images from any registry (Docker Hub, ghcr.io, etc.)

**Why:** 
- Standard library used by Podman, Skopeo, Buildah
- Works directly with registries (no daemon required)
- Supports all registry authentication methods

**Alternative:** Docker API - requires Docker daemon running

### Why umoci? (oci.go)

**What:** Unpack OCI image layers in userspace

**Why:**
- Purpose-built for rootless OCI manipulation (official OpenContainers project)
- Handles OCI layer semantics (whiteouts, layer ordering) correctly
- Designed to work without root privileges

**Alternative:** With Docker API, the daemon (running as root) mounts image layers using overlayfs, then exports the merged filesystem. Users get the result without needing root themselves but it still has the dependency on Docker and does actually mount the overlays to get the merged filesystem. With umoci, layers are merged in userspace by extracting each tar layer sequentially and applying changes (including whiteouts for deletions). No kernel mount needed, fully rootless. Umoci was chosen because it's purpose-built for this use case and embeddable with the go program.

### Why mkfs.ext4? (disk.go)

**What:** Shell command to create ext4 filesystem

**Why:**
- Battle-tested, fast, reliable
- Works without root when creating filesystem in a regular file (not block device)
- `-d` flag copies directory contents into filesystem

**Options:**
- `-b 4096` - 4KB blocks (standard, matches VM page size)
- `-O ^has_journal` - No journal (disks mounted read-only in VMs, saves ~32MB)
- Minimum 10MB size covers ext4 metadata (~5MB for superblock, inodes, bitmaps)

**Alternative tried:** go-diskfs pure Go ext4 - has bugs

**Tradeoff:** Shell command vs pure Go, but mkfs.ext4 is universally available and robust

## Filesystem Persistence (storage.go)

**Metadata:** JSON files with atomic writes (tmp + rename)
```
/var/lib/hypeman/images/{id}/
  rootfs.ext4
  metadata.json
```

**Why filesystem vs database?**
- Disk images must be on filesystem anyway
- No sync issues between DB and actual artifacts
- Simple recovery (scan directory to rebuild state)

## Build Tags

Requires `-tags containers_image_openpgp` to avoid C dependency on gpgme. This is a build-time option of the containers/image project to select between gpgme C library with go bindings or the pure Go OpenPGP implementation (slightly slower but doesn't need external system dependency).

## Testing

Integration tests pull alpine:latest (~7MB) and verify full pipeline. No special permissions required.

