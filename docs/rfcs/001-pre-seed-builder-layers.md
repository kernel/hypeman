# RFC 001: Pre-seed BuildKit Layer Store in Builder VMs

**Status**: Proposal
**Author**: hiroTamada
**Date**: 2026-02-09

## Problem

Every code-change deployment takes ~27s of build time inside the builder VM, even though the base image hasn't changed. The breakdown:

| Step | Time | Notes |
|------|------|-------|
| Base image extract | ~10.6s | Decompress + unpack 16 gzipped tar layers (~100MB) |
| COPY + RUN setup.sh | ~5.4s | pnpm install + TypeScript + esbuild |
| Image export + push | ~7.3s | Push built image layers to registry |
| Cache export | ~0.1s | Write cache manifest |

The ~10.6s base image extraction is the single largest cost and is incurred on **every build where source code changes**, which is effectively every real deployment. This happens because builder VMs are ephemeral — BuildKit starts with an empty content store every time.

### Current architecture

```
Builder VM boots
  → BuildKit starts with empty content store
  → Imports cache manifest from registry (knows WHAT layers exist)
  → COPY step: cache miss (source files changed)
  → BuildKit needs filesystem to execute COPY
  → Downloads 16 compressed layers from local registry (~0.4s)
  → Decompresses + extracts each layer sequentially (~10.6s)  ← BOTTLENECK
  → Executes COPY, RUN, etc.
```

When all steps are cached (identical redeploy), BuildKit never needs the filesystem and the build completes in ~1s. But any cache miss — which happens on every code change — triggers the full extraction.

### Why the registry cache doesn't help

The registry stores **compressed tar archives** (gzipped blobs). BuildKit needs **unpacked filesystem trees** (actual directories and files on disk) to execute build steps. The registry cache tells BuildKit *what* the result of each step is (layer digests), but when a step needs to actually execute, BuildKit must reconstruct the filesystem from the compressed layers. The ~10s is the decompression + extraction cost.

## Proposal

Pre-seed the builder VM's rootfs with BuildKit's content store already populated for known base images. When a build runs, BuildKit finds the base image layers already extracted locally and skips the download + extraction entirely.

### How BuildKit's content store works

BuildKit uses containerd's content store at `/home/builder/.local/share/buildkit/`:

```
/home/builder/.local/share/buildkit/
├── containerd/
│   ├── content/
│   │   └── blobs/sha256/     ← compressed layer blobs (content-addressable)
│   └── metadata.db           ← bolt database with metadata
└── snapshots/
    └── overlayfs/            ← extracted filesystem snapshots
```

When BuildKit processes `FROM onkernel/nodejs22-base:0.1.1`, it:

1. Checks if the layer blobs exist in `content/blobs/sha256/`
2. Checks if extracted snapshots exist in `snapshots/`
3. If both exist, skips download + extraction entirely
4. If not, downloads from registry and extracts

By pre-populating both the blobs and snapshots, BuildKit can skip steps 3-4.

## Options Considered

### Option A: Pre-seed at builder image build time (Recommended)

Warm the content store during the builder Docker image build, then bake the result into the image.

**Build process:**

```bash
# 1. Build the base builder image as usual
docker build -t builder:base -f lib/builds/images/generic/Dockerfile .

# 2. Warm the content store
docker run -d --privileged --name warmup builder:base sleep infinity

docker exec warmup sh -c '
  buildkitd &
  sleep 2

  mkdir /tmp/warmup
  echo "FROM onkernel/nodejs22-base:0.1.1" > /tmp/warmup/Dockerfile
  echo "RUN true" >> /tmp/warmup/Dockerfile

  buildctl build \
    --frontend dockerfile.v0 \
    --local context=/tmp/warmup \
    --local dockerfile=/tmp/warmup \
    --output type=oci,dest=/dev/null

  kill %1 && wait
'

# 3. Commit with warmed content store
docker commit warmup onkernel/builder-generic:latest

# 4. Push with OCI mediatypes
# (need to re-tag and push via buildx for OCI compliance)
docker rm -f warmup
```

**Could also be automated** via a `Makefile` target:

```makefile
build-builder-warmed:
    docker build -t builder:base -f lib/builds/images/generic/Dockerfile .
    ./scripts/warm-builder-cache.sh builder:base onkernel/builder-generic:latest
```

**Pros:**
- Every VM boots with the base image already extracted
- Zero per-tenant storage overhead
- No changes to the build pipeline — just a bigger builder image
- Eliminates ~10.6s extraction on every code-change build

**Cons:**
- Builder image grows by ~100-150MB (uncompressed base image layers)
- Must rebuild the builder image when base images change
- `docker commit` + re-push workflow is somewhat manual
- Only helps for base images known at builder image build time

### Option B: Persistent volume per tenant

Attach a persistent block device to each builder VM that survives across builds.

**Architecture:**

```
First build:
  VM boots with persistent volume mounted at /home/builder/.local/share/buildkit/
  → BuildKit extracts base image layers → written to persistent volume
  → Build completes
  → VM shuts down, volume persists

Second build:
  VM boots with same persistent volume
  → BuildKit finds layers already extracted
  → Skips download + extraction (0.0s)
```

**Implementation:**
- Create a persistent ext4 volume per tenant (or per org)
- Mount it into the builder VM at BuildKit's content store path
- Manage lifecycle: create on first build, garbage collect after idle period

**Pros:**
- Works for ANY base image (not just pre-known ones)
- Gets faster over time as more layers accumulate
- Tenant cache and layer store in one place

**Cons:**
- Per-tenant storage cost (~200-500MB per tenant, grows over time)
- Needs volume lifecycle management (creation, cleanup, GC)
- Potential stale data issues (old layers accumulating)
- More complex VM setup (attach volume before boot)
- Cloud Hypervisor needs block device attachment support

### Option C: Shared read-only layer cache

Maintain a shared, read-only layer cache volume that contains extracted layers for common base images. Mount it into every builder VM.

**Architecture:**

```
Periodic job:
  Extracts layers for known base images into a shared volume
  → onkernel/nodejs22-base:0.1.1
  → onkernel/python311-base:0.1.0
  → etc.

Every build:
  VM boots with shared volume mounted read-only
  → BuildKit finds common layers already extracted
  → Uses copy-on-write for any new layers
```

**Pros:**
- One volume serves all tenants
- Minimal storage overhead
- No per-tenant state to manage

**Cons:**
- Only helps for pre-known base images (same as Option A)
- Needs overlay/copy-on-write filesystem support
- Read-only mount needs BuildKit configuration changes
- More complex than Option A for similar benefit

## Recommendation

**Start with Option A** (pre-seed at build time). It's the simplest to implement, requires no infrastructure changes, and addresses the primary bottleneck. The only cost is a larger builder image (~100-150MB), which is negligible given the ~10s savings on every deploy.

### Expected impact

| Scenario | Current | With pre-seeded layers |
|----------|---------|----------------------|
| Code change deploy (first for tenant) | ~27s build | ~17s build (-37%) |
| Code change deploy (subsequent) | ~27s build | ~17s build (-37%) |
| No code change (cached) | ~1s build | ~1s build (unchanged) |
| Total deploy time (code change) | ~50s | ~40s |

The ~10s savings applies to every single code-change deployment across all tenants using `nodejs22-base`.

### Future work

If builder image size becomes a concern (multiple base images), consider:

1. **Option B** for tenants with high deploy frequency — persistent volumes amortize the extraction cost over many builds
2. **Lazy pulling** (eStargz/zstd:chunked) — BuildKit can pull and extract only the layers it actually needs, on demand. Requires base images published in eStargz format.
3. **Dockerfile restructuring** — splitting `COPY` into dependency-only and source-only steps to maximize cache hits on the `RUN` step, reducing the impact of cache misses

## Open Questions

1. Should we pre-seed multiple base images (nodejs, python, etc.) or just the most common one?
2. What's the acceptable builder image size increase? Each base image adds ~100-150MB.
3. Should the warm-up script be part of CI/CD, or a manual step when base images change?
4. Does Cloud Hypervisor's block device support make Option B viable for later?
