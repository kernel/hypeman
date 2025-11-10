# Image Manager

Converts OCI images to bootable erofs disks for Cloud Hypervisor VMs.

## Architecture

```
OCI Registry → containers/image → OCI Layout → umoci → rootfs/ → mkfs.erofs → disk.erofs
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

### Why erofs? (disk.go)

**What:** erofs (Enhanced Read-Only File System) with LZ4 compression

**Why:**
- Purpose-built for read-only overlay lowerdir
- Fast compression (~20-25% space savings)
- Fast decompression at VM boot
- Lower memory footprint than ext4
- No journal/inode overhead

**Options:**
- `-zlz4` - Fast compression (good balance for development)

**Alternative:** ext4 without journal works but erofs is optimized for this exact use case

## Filesystem Layout (storage.go)


```
/var/lib/hypeman/
  images/
    docker.io/library/alpine/
      latest/
        metadata.json  # Status, entrypoint, cmd, env
        rootfs.erofs   # Compressed read-only disk
      3.18/            # Different version
  system/oci-cache/
    docker.io/library/alpine/latest/
      blobs/sha256/... # Shared layers, persistent
```

**Benefits:**
- Natural hierarchy (versions grouped)
- Layer caching (alpine:latest and alpine:3.18 share base layers)

## Input Validation

Uses `github.com/distribution/reference` to validate and normalize names:
- `alpine` → `docker.io/library/alpine:latest`
- Rejects invalid formats (returns 400)

## Build Tags

Requires `-tags containers_image_openpgp` to avoid C dependency on gpgme. This is a build-time option of the containers/image project to select between gpgme C library with go bindings or the pure Go OpenPGP implementation (slightly slower but doesn't need external system dependency).

## Registry Authentication

containers/image automatically uses `~/.docker/config.json` for registry authentication.

```bash
# Login to Docker Hub (avoid rate limits)
docker login

# Works for any registry
docker login ghcr.io
```

No code changes needed - credentials are automatically discovered.