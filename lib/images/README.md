# Image Manager

Converts OCI images to bootable erofs disks for Cloud Hypervisor VMs.

## Architecture

```
OCI Registry → go-containerregistry → OCI Layout → umoci → rootfs/ → mkfs.erofs → disk.erofs
```

## Design Decisions

### Why go-containerregistry? (oci.go)

**What:** Pull OCI images from any registry (Docker Hub, ghcr.io, etc.)

**Why:** 
- Lightweight library from Google (used by ko, crane, etc.)
- Works directly with registries (no daemon required)
- Can propagate errors from registry (like 429)
- Supports all registry authentication methods

**Alternative:** containers/image - has automatic retry logic that delays error reporting, can't fail fast for registry rate limits. Heavier, supporting more use cases in comparison to go-containerregistry.

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
- `-zlz4` - Fast compression

**Alternative:** ext4 without journal works but erofs is optimized for this exact use case

## Filesystem Layout (storage.go, oci.go)

Content-addressable storage with tag symlinks (similar to Docker/Unikraft):

```
/var/lib/hypeman/
  images/
    docker.io/library/alpine/
      abc123def456.../      # Digest (sha256:abc123def456...)
        metadata.json       # Status, entrypoint, cmd, env
        rootfs.erofs        # Compressed read-only disk
      def456abc123.../      # Another version (digest)
        metadata.json
        rootfs.erofs
      latest -> abc123def456...   # Tag symlink to digest
      3.18 -> def456abc123...     # Another tag
  system/
    oci-cache/              # Shared OCI layout for all images
      index.json            # Manifest index with digest-based tags
      blobs/sha256/
        2d35eb...           # Layer blobs (shared across all images!)
        44cf07...           # Another layer
        706db5...           # Config blob for alpine
        abc123def456...     # Manifest for alpine:latest
```

**Benefits:**
- Content-addressable: Digests are immutable, same content stored once
- Tag mutability: Tags (symlinks) can point to different digests over time
- Deduplication: Multiple tags can point to same digest
- Natural hierarchy: All versions of an image grouped under repository
- Easy inspection: Clear which digest belongs to which image
- Layer caching: All images share the same blob storage, layers deduplicated automatically

**Design:**
- Images stored by manifest digest (content hash)
- Tags are filesystem symlinks pointing to digest directories
- Manifest always inspected upfront to discover digest (validates existence)
- Pulling same tag twice updates the symlink if digest changed
- OCI cache uses digest hex as layout tag for true content-addressable caching
- Shared blob storage enables automatic layer deduplication across all images
- Old digests remain until explicitly garbage collected
- Symlinks only created after successful build (status: ready)

## Reference Handling (reference.go)

Two types for type-safe image reference handling:

**`NormalizedRef`** - Validated format (parsing only):
```go
normalized, err := ParseNormalizedRef("alpine")
// Normalizes to "docker.io/library/alpine:latest"
```

**`ResolvedRef`** - Normalized + manifest digest (network call):
```go
resolved, err := normalized.Resolve(ctx, ociClient)
// Now has digest from registry inspection

resolved.Repository()  // "docker.io/library/alpine"
resolved.Tag()         // "latest"
resolved.Digest()      // "sha256:abc123..." (always present)
```

Validation via `github.com/distribution/reference`:
- `alpine` → `docker.io/library/alpine:latest`
- `alpine:3.18` → `docker.io/library/alpine:3.18`
- `alpine@sha256:abc123...` → digest validated against registry
- Rejects invalid formats (returns 400)

## Build Tags

Requires `-tags containers_image_openpgp` for umoci dependency compatibility.

## Registry Authentication

go-containerregistry automatically uses `~/.docker/config.json` via `authn.DefaultKeychain`.

```bash
# Login to Docker Hub (avoid rate limits)
docker login

# Works for any registry
docker login ghcr.io
```

No code changes needed - credentials are automatically discovered.