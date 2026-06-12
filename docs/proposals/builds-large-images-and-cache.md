# RFC: Build system — large images, persistent cache, and the build API surface

> Status: draft. Written after an empirical attempt to build the kernel-images
> `chromium-headful` production browser image with `hypeman build` on
> 2026-06-11 (main @ 1b153f84). All file:line references are to that commit.

## Summary

`hypeman build` runs BuildKit inside an ephemeral microVM and pushes the
result to the embedded registry. For small user-supplied Dockerfiles this
works well (a trivial alpine image builds in ~4s). For large from-scratch
images it fails, and the failure is architectural: **BuildKit's entire data
root — every base-image layer, every intermediate snapshot, every
`--mount=type=cache` directory — lives on a hardcoded 3GB RAM-backed tmpfs
inside the builder VM** (`lib/builds/builder_agent/main.go:869`). The
practical ceiling on buildable image size is therefore "what fits in builder
RAM", there is no user-facing knob for it, and the memory caps that would let
RAM substitute for disk are themselves stacked at 16GB
(`lib/builds/types.go:227`) and 32GB (`limits.max_memory_per_instance`).

The same design decision means **no build cache can survive between builds**:
the tmpfs dies with the VM, and the opt-in registry cache
(`--cache-scope`/`--global-cache-key`) only covers layer results — never
cache mounts — and is off by default. Rebuilding an identical context with no
cache flags re-executes every step (verified: 0 `CACHED` steps on an
immediate identical rebuild).

This RFC proposes replacing the tmpfs with a **dedicated ext4 volume mounted
at BuildKit's root**, which simultaneously removes the size ceiling, removes
the need for oversized builder RAM, and creates the natural attachment point
for depot.dev/Docker-Build-Cloud-style persistent per-tenant build caches. It
also catalogs the gaps between our build API and `docker build`/buildx, and
four bugs found during the experiment.

## Motivation: the chromium-headful case study

The kernel-images `images/chromium-headful/Dockerfile` (the production
browser image) is a worst-case but real workload:

- 7 stages across 4 registries (`docker.io/golang`, `node:22`,
  `ubuntu:22.04`, `ghcr.io/kernel/neko/base`)
- network-heavy `RUN` steps: apt + a PPA, `npm install`, `go mod download`,
  raw `curl` of Chrome-for-Testing (~150MB), FFmpeg static builds, websocat,
  a loose `.deb` from ftp.de.debian.org
- BuildKit-specific features: `RUN --mount=type=cache` on every expensive
  step, heredoc `RUN <<-'EOT'` syntax, `TARGETARCH`/`TARGETOS` args
- compiles Xorg drivers from source (autoreconf/configure/make)
- build context is the repo root (~62MB), `-f images/chromium-headful/Dockerfile`

What worked **unmodified** — worth calling out because none of it was
guaranteed:

| Feature | Result |
|---|---|
| Multi-registry `FROM`s incl. ghcr.io | ✅ `mirrorBaseImagesForBuild` (`lib/builds/mirror.go:19`) mirrors non-docker.io refs into the local registry too |
| Heredoc syntax, no `# syntax=` directive | ✅ builtin `dockerfile.v0` frontend on BuildKit v0.30.0 |
| `--mount=type=cache` | ✅ (functional within a build; never persists — see Caching) |
| Egress for apt/npm/curl | ✅ `NetworkMode` defaults to `"egress"` (`lib/builds/types.go:236`) |
| Repo-root context + `-f` subpath | ✅ |

What failed was purely capacity. Timeline of the experiment (all on a host
with 1TB RAM, stock config except where noted):

1. **Build 1** (`--cpus 8 --memory 8192 --timeout 3600`): died at ~60s,
   `ResourceExhausted: ... /var/lib/buildkit/...: no space left on device`,
   while parallel stages were extracting golang/node/ubuntu bases. Cause:
   the 3GB tmpfs. (Initially misdiagnosed as the 10GB instance overlay
   default, `lib/instances/create.go:169` — patching that had no effect,
   because BuildKit never writes to the overlay.)
2. **Build 2** (overlay patched to 64GB): identical no-space failure at ~17s
   (faster only because the mirror was warm). Confirmed the tmpfs at
   `builder_agent/main.go:869` is the binding constraint.
3. **Build 3** (tmpfs patched to `size=85%` of guest RAM, `--memory 96000`):
   rejected 400 — `MaxBuildMemoryMB = 16384` (`lib/builds/types.go:227`).
4. **Build 4** (cap raised to 128GB): rejected 500 — instance admission:
   `total memory ... exceeds maximum allowed 34359738368` —
   `limits.max_memory_per_instance` (32GB default).
5. **Build 5** (config limit raised to 256GB, `--memory 65536` → ~54GB
   tmpfs): **success in 2m42s.** 1.5GB image, boots, all artifacts verified
   in the guest (compiled Go binaries, chromedriver 148, neko, ffmpeg, the
   from-source Xorg drivers, 602 fonts).

So the system is *capable* of building our production browser image — three
stacked limits away, all of which exist because the snapshot store is RAM.
The experiment patches were reverted; this RFC is the durable artifact.

To be precise about why the tmpfs exists (comment at
`builder_agent/main.go:859-864`): the builder VM's rootfs is an overlayfs
(read-only ext4 + writable upper), and BuildKit's native `overlayfs`
snapshotter creates `mknod` char-0:0 whiteouts, which the kernel rejects on
an overlayfs mount. tmpfs sidesteps the nesting. The fix below sidesteps it
differently.

## Caching today

Mechanism (all in `runBuildKit`, `builder_agent/main.go:789-846`):

- **Import**: `--import-cache type=registry,ref={registry}/cache/global/{key}`
  if `global_cache_key` set; plus `.../cache/{scope}` if `cache_scope` set.
- **Export**: `mode=max,image-manifest=true` to `cache/{scope}` (regular) or
  `cache/global/{key}` (admin builds only), into the embedded registry.
  Scoped registry tokens enforce tenant isolation
  (`lib/builds/manager.go:419-466`).

Properties that follow:

1. **Off by default.** No flags → no import, no export. Verified: an
   immediate identical rebuild of chromium-headful produced **0 `CACHED`
   steps** and began re-executing every `RUN`.
2. **Cache mounts never persist.** `--mount=type=cache` dirs (go-build,
   go-mod, npm, apt — exactly what this Dockerfile leans on) live in
   BuildKit's root, i.e. the tmpfs. Registry cache `mode=max` exports layer
   *results*, not mount contents. Even a fully cache-flagged rebuild re-runs
   `go mod download` and `npm install` from the network whenever any input
   to those layers changes.
3. **Import is network-shaped.** A fresh VM re-pulls cache blobs from the
   registry every build. Fine at our image sizes, but strictly worse than a
   local store that's simply still there.
4. The only always-on warmth is **base-image mirroring** — `FROM` pulls
   resolve from the local registry's OCI cache. That's image caching, not
   step caching.

For comparison, the remote-builder products this competes with
(depot.dev, Docker Build Cloud, Namespace) are at core "BuildKit + a
persistent NVMe volume per project": their entire pitch is that the layer
cache and cache mounts survive across builds, turning rebuilds into seconds.
We have the harder part (instant isolated VMs, scoped registry auth) and
lack the easy part (a disk that survives).

## The API surface vs comparables

End-to-end (CLI flag → multipart field → `BuildConfig`), we expose: context
tarball, `dockerfile`, `timeout_seconds`, `memory_mb`, `cpus`, `secrets`,
`cache_scope`, `global_cache_key`, `is_admin_build`, `image_name`,
`base_image_digest`, `tags`. Missing or broken vs `docker build`/buildx:

| Capability | docker/buildx | hypeman | Notes |
|---|---|---|---|
| `--build-arg` | ✅ | **dead wiring** | `CreateBuildRequest.BuildArgs` exists (`types.go:58`), manager forwards it (`manager.go:506`), agent applies it (`main.go:851`) — but `CreateBuild` parses no `build_args` multipart field (`cmd/api/api/builds.go`), so it can never be set. One field away from working. |
| `--target` (stage) | ✅ | ❌ | Not in CLI or server. (The `dockerfile.v0` frontend supports it as `--opt target=`.) |
| `--platform` | ✅ | ❌ | Host arch only; no multi-arch manifests. |
| `--no-cache`, `--pull` | ✅ | ❌ | No way to force-refresh; matters more once caching defaults on. |
| `--ssh` | ✅ | ❌ | Blocks private-git deps. Secrets exist; agent forwarding doesn't. |
| Network selection | ✅ | ❌ | `NetworkMode` and `AllowedDomains` exist in `BuildPolicy` (`types.go:91-96`) but no CLI/API field sets them; egress-always is the de facto policy. |
| Builder disk size | n/a (host disk) | ❌ | The limit this whole RFC is about. |
| Cache import/export targets | registry/local/gha/s3/inline | fixed tenant/global repos | Opinionated is fine; opt-in default is not (see below). |
| Output targets | image/registry/oci tar/local | fixed: internal `builds/{id}` | No way to export an OCI tar for use outside hypeman. |
| Provenance/SBOM | ✅ controllable | computed, not exposed | `BuildProvenance` is returned but not attestation-formatted. |
| `.dockerignore` | ✅ | **silently ignored** | CLI strips it and applies a hardcoded exclude list (`hypeman-cli pkg/cmd/build.go:308-310`). Correct results for Dockerfiles that `COPY` specific paths; silently different for `COPY . .` projects. |

The resource caps deserve their own row: `MaxBuildCPUs = 8`,
`MaxBuildMemoryMB = 16384` are flat constants (`types.go:224-227`) with no
relationship to host capacity. Once the snapshot store is disk-backed these
defaults are actually reasonable — the chromium build used well under 8GB of
*working* memory — but they should be config-derived, not compile-time.

## Proposal

### P1: Disk-backed BuildKit root on a dedicated volume (the keystone)

Attach a third volume to the builder VM (alongside `/src` and `/config`,
`manager.go:726-744`), formatted ext4, mounted at `/var/lib/buildkit`. The
nested-overlayfs `mknod` problem does not apply: the mount is a real block
device, and overlayfs-snapshotter-on-ext4 is the standard BuildKit
deployment everywhere. The volume manager already does everything required
(`CreateVolume` + `VolumeAttachment`).

- New `BuildPolicy.DiskGB` (default ~20GB, config-capped like cpus/memory),
  wired through CLI `--disk`, multipart `disk_gb`, `BuildConfig`.
- Agent change: replace the tmpfs mount with mounting the attached volume
  (or keep tmpfs as fallback when no volume is attached, for
  rolling-upgrade compatibility with old hosts).
- Delete the volume with the VM by default (ephemeral scratch).

This alone removes the image-size ceiling at stock memory limits — the
chromium build would have succeeded with the default 4GB builder — and
deletes the rationale for raising any memory cap.

### P2: Persistent per-scope cache volumes (the depot move)

Once P1 exists, persistence is a lifecycle flag away: key the volume by
cache scope (`buildcache/{scope}`) instead of by build, reattach it on the
next build of the same scope, and BuildKit's local layer store **and cache
mounts** survive. `go mod download`, `npm install`, apt archives — all warm.
This is the feature that makes `hypeman build` competitive with remote
builders rather than a from-scratch executor with VM-grade isolation.

Open questions to resolve in design review:

- **Concurrency**: a volume attaches to one VM at a time → serialize builds
  per scope (simple, probably right), or maintain a small volume pool per
  scope.
- **Eviction**: cap volume size; let `buildkitd`'s GC policy handle
  internal pruning; LRU-delete whole scope volumes server-side.
- **Trust**: a tenant's cache volume is tenant-tainted state mounted into a
  privileged-ish guest; same trust boundary as the registry cache today,
  but worth stating. Global/shared caches should remain registry-based.

### P2a: Tenancy — what a safe cache actually requires (and what it doesn't)

Builds are untrusted: Kernel users submit arbitrary code and we execute it
on hypeman hosts. The question is whether persistent caching forces full
tenant/user modeling into hypeman. It doesn't. Separate the three actors:

1. **The build itself** (arbitrary code, root in the builder VM). Already
   correctly contained: its scoped registry token can push only to
   `builds/{id}` and `cache/{its scope}` (`manager.go:419-466`). The worst
   it can do to a cache is poison *its own scope*, i.e. its own future
   builds. P2's volumes inherit this property for free as long as a volume
   is keyed by scope and only ever attached to builds of that scope. The
   host never mounts cache volumes (guests do, over virtio-blk), so a
   maliciously crafted filesystem is parsed by the next *same-scope* guest
   kernel — and that guest is already running the attacker's code as root.
   No new boundary is crossed; the VM remains the security boundary.

2. **The API caller.** Today hypeman trusts it completely: `cache_scope`
   is a free-form multipart field, and — found while writing this —
   **`is_admin_build` is an ungated boolean** (`cmd/api/api/builds.go:158`):
   any token with `build:write` can claim it and gain *push* access to
   `cache/global/{key}`, poisoning the shared cache for every tenant that
   imports it. That is acceptable only while the sole token-holder is
   Kernel's control plane (one trust domain). It must be fixed before
   tokens ever become per-tenant, and should be fixed regardless:
   - Gate global-cache push behind a new `build:admin` scope in
     `lib/scopes`.
   - Move the cache scope from a request field into a **JWT claim** (e.g.
     `cache_scope: "tenant-abc"`), minted by the control plane. The server
     derives the scope from the token and rejects mismatched request
     fields. A token can then only build into its own scope, even if
     leaked to or held by an end user.

3. **The cache artifact consumed later.** Per-scope artifacts are only
   re-ingested by the same scope (self-poisoning, bounded). The **global
   cache is the only cross-tenant edge**, and it is safe precisely because
   it is pull-only for regular builds and written only by trusted
   (operator) builds — BuildKit verifies blob digests against the cache
   manifest, and the manifest lives in a repo tenants cannot write.
   Cross-tenant *writable* sharing or dedup of cache entries is unsafe,
   period: a tenant who can write entries a victim imports can precompute
   the victim's step digests (Dockerfile prefixes like
   `FROM ubuntu` + `RUN apt-get update` are guessable) and substitute
   poisoned result layers. Shared caches also leak a timing side-channel
   (probe whether another tenant built a given layer). Don't share; only
   the trusted-producer global tier crosses tenants.

So the requirement is **one unforgeable claim plus quotas**, not user
modeling:

- scope-in-token (above) — the single piece of "tenancy" hypeman needs;
- `build:admin` gating for global cache push;
- per-scope storage quota and server-side LRU eviction of scope volumes
  (DoS-by-cache-fill is otherwise free);
- optional: TTL / `--no-cache` as a remediation path — a transiently
  compromised dependency can otherwise persist in a tenant's cache as a
  hit long after the upstream is fixed. (This poisons only the tenant's
  own image, which they could ship anyway; the cache adds persistence and
  stealth, not a new capability.)

Identity — users, orgs, billing, who maps to which scope — stays in
Kernel's control plane, exactly as it does for instances today. Hypeman
never needs to know what a "tenant" is; it needs to enforce that an opaque
scope label asserted by a trusted token is the only thing a build can read
from and write to.

### P2b: Cross-scope reuse and host-disk dedup

Two follow-on questions fall out of strict per-scope isolation: can two
scopes ever benefit from each other's caches, and what happens on host
disk when two tenants build the same Dockerfile?

**Cross-scope cache benefit — only through a trusted producer.** Direct
sharing (scope A imports scope B's cache, or any writable shared tier) is
unsafe per P2a: poisoning plus a timing side-channel. The safe channels:

1. **The existing global cache tier**, used deliberately. Two tenants
   usually have the same Dockerfile because *we gave it to them* — Kernel's
   deploy templates. The control plane knows every template's Dockerfile,
   so it can run an admin build per template release to warm
   `cache/global/{template}`; every tenant building from that template
   then imports a trusted, pull-only cache covering the shared prefix.
   This captures most of the real-world overlap without any cross-tenant
   trust.
2. **Promotion by re-execution, never by copy.** If the control plane
   observes the same Dockerfile prefix across many tenants, it may re-run
   those steps in its own admin build and publish the result to global.
   Copying a tenant-produced cache entry into the global tier would
   launder untrusted artifacts into the trusted tier — forbidden.
3. **Base-image mirroring** (already always-on) — `FROM` layers are
   fetched once per host into the shared OCI cache regardless of scope.

Note cache sharing and storage dedup are different problems: a warm global
cache makes both tenants' builds *fast*, but their final images still
differ (builds are not reproducible — timestamps and network
nondeterminism change layer digests), so it does not make their storage
converge.

**Whole-build dedup by input hash — the cheap path to "second identical
build is instant".** When two tenants submit *bit-identical* builds (the
common case: Kernel templates, or two tenants building the same public
repo like kernel-images), no cache machinery is needed at all. Key
completed builds by a host-computed canonical input hash:

    H = sha256(canonical-context-hash ‖ dockerfile ‖ sorted(build_args)
               ‖ resolved base-image digests ‖ builder-image digest)

On `POST /builds`, if H matches a prior successful build, skip the VM
entirely and register the new `builds/{id}` name against the existing
image digest — seconds, not minutes. Safety follows from a clean argument:
the only influence the first submitter had over the result is the inputs
themselves, and the second tenant chose *identical* inputs. A malicious
first builder cannot inject anything the victim's own execution of the
same instructions could not have produced — the build ran in our VM, on
our BuildKit, on inputs the victim specified. (The guest-reported
`source_hash` in provenance must not be used for this — the agent runs in
the attacker-controlled VM; the host computes H from the tarball it
already holds. The existing `hashDirectory` shape — path‖content, no
mtimes — is the right canonicalization.)

Required exclusions and caveats:

- **Builds with secrets never dedup** (results depend on values that
  can't go in a shared key without leaking equality).
- **Resolve `FROM` tags to digests before hashing** (floating tags would
  otherwise alias different bases) — the mirror step already resolves
  them.
- **TTL on dedup entries**: steps that fetch from the network
  (`apt-get update`, `curl`) bake in time-of-build content; serving a
  months-old result for a "fresh" build is wrong even if safe. `--pull`
  / `--no-cache` must bypass dedup.
- **Existence oracle**: a dedup hit reveals "someone already built
  exactly this". For template and public-repo content this is harmless;
  the control plane can scope dedup (e.g. only for inputs it recognizes)
  if it ever matters.

P2b's digest-keyed conversion store is the enabler that makes serving a
dedup hit free — registering a second name against an existing digest
must not trigger a second rootfs conversion (today it would: bug P5.3).

Dedup also yields the promotion signal for free: H-collision counts tell
the control plane exactly which non-template Dockerfiles are popular
enough to warrant a trusted global-cache warm (mechanism 2 above) —
which then accelerates the *near-miss* case dedup can't touch (tenant
edited the last line of the Dockerfile; identical prefix steps still hit
the trusted global cache).

**Host-disk duplication — real today, and worse than two copies.** Image
storage has two tiers (measured on a dev host, 2026-06-11):

- `data_dir/system/oci-cache/blobs/sha256/` — a single content-addressed
  blob pool. Identical blobs are stored once across all repos and tenants
  automatically. Base layers of two same-Dockerfile builds dedupe here;
  their `RUN`-produced layers don't (non-reproducible ⇒ different bytes ⇒
  honestly different blobs).
- `data_dir/images/{repo}/{digest}/rootfs.erofs` — the bootable converted
  artifact, a **flattened** filesystem keyed by *(repo name, digest)*.
  No sharing at any level: identical base layers are re-materialized
  inside every image's erofs, and even the *same digest* under two names
  is converted and stored twice. Empirically: after one chromium-headful
  build, the identical digest `47f079…` exists as two 1.5GB erofs files
  (`builds/{id}` and the `image_name` re-tag) — bug P5.3 is the
  single-tenant case of this. Two tenants building the same Dockerfile ⇒
  two ~1.5GB flattened artifacts whose bytes are ~95% common.

Mitigations, cheap to deep:

1. **Digest-keyed conversion store** (`images/by-digest/{digest}/rootfs.erofs`
   with name→digest references): dedupes the same-digest case across all
   repo names, fixes P5.3 structurally, and makes image deletion
   refcounted. Cheap; do this first.
2. **Per-layer conversion + overlay assembly** (composefs-style): convert
   each *layer* once, keyed by layer digest in a shared store, and boot
   instances from stacked layer images + overlay instead of one flattened
   erofs. This is the principled fix for the two-tenant case — their
   common base layers (usually the bulk) exist once on host disk; only
   their unique `RUN` layers differ. Bigger change: touches the
   boot/disk-attach path and per-VM block-device count limits; deserves
   its own RFC.
3. Filesystem-level reflink/dedup of the flattened artifacts is fragile
   (conversion must be byte-stable for extents to align) — not worth
   pursuing given (2) exists.

### P3: Cache defaults and controls

- Cache on by default: when the token carries a `cache_scope` claim (P2a),
  use it without ceremony; otherwise fall back to the token subject. The
  second build of anything should be incremental with zero flags. Keep an
  explicit flag to disable.
- Add `--no-cache` (skip import) and `--pull` (re-resolve bases) for the
  force-clean path.

### P4: API surface — cheap fixes first

1. Wire `build_args` (add the multipart case; everything downstream
   already works). Add `--build-arg` to the CLI.
2. Add `target` (CLI flag → field → `--opt target=`).
3. Expose `network_mode`/`allowed_domains` from `BuildPolicy` to the API,
   since isolated-by-request is a real security ask.
4. Make resource caps config-derived (`limits.builds.*`) instead of
   constants; validate against host capacity at startup.
5. Then, by demand: `--platform`, `--ssh`, OCI-tar output, attestation
   output. Multi-arch is a separate project (needs emulation or multi-host
   scheduling) and should not block any of the above.

### P5: Bugs found during the experiment (independent fixes)

1. **SSE build stream dies on long builds; CLI reports failure for a build
   that succeeds.** During the successful 2m42s build the CLI printed
   `build failed: build stream ended unexpectedly (status: )` and exited
   non-zero mid-apt-install while the server ran to completion. The CLI
   should reconnect/poll `GET /builds/{id}` on stream EOF instead of
   declaring failure; worth also finding what severed the stream
   (`StreamBuildEvents`, `manager.go:1103`).
2. **`build cancel` returns 404 for an actively-`building` build**
   (observed on a build listed as `building` by `ListBuilds` moments
   earlier). Likely an in-memory vs on-disk state lookup mismatch in
   `CancelBuild` (`manager.go:1060`).
3. **`image_name` re-tag triggers a second full ext4 conversion.** After a
   build, `builds/{id}` converts fine, then `ImportLocalImage`
   (`manager.go:633`) imports the same digest under the user's name and
   kicks off a *second* conversion of the identical rootfs — which timed
   out for the 1.5GB image, leaving `kernel-chromium-headful:latest`
   permanently `pending` while `builds/{id}` was `ready`. The re-tag
   should alias the existing converted artifact (it's the same digest),
   not re-convert.
4. **(cloud-hypervisor, not builds)** `hypeman run --memory 4096` panics
   CH v51.1 at boot: `Error writing RSDP: InvalidGuestAddress(GuestAddress(655360))`
   (`vmm/src/acpi.rs:1125`), reproducible ×3; default memory boots the
   same image fine. Filed here for visibility; needs its own
   investigation of our memory-layout config for sub-hotplug sizes.

## What this does not change

- The security model: ephemeral VM per build, scoped registry tokens,
  tenant cache isolation all stay as-is. P2 adds a per-scope mutable volume,
  which is the same trust boundary as the per-scope registry cache.
- The builder image and agent protocol (vsock, config disk) are untouched
  except for the mount source of `/var/lib/buildkit`.
- Production browser images: kernel-images CI builds those with its own
  BuildKit (`build-docker.sh`); hypeman pulls the result. Nothing here is
  required for prod today — the point is what `hypeman build` can credibly
  offer users.

## Appendix: measured data (2026-06-11, 1TB-RAM dev host)

| Run | Config | Outcome |
|---|---|---|
| alpine smoke test | defaults | ✅ 4s |
| chromium #1 | 8cpu/8GB/3600s, stock agent | ❌ no-space (tmpfs) @ 60s |
| chromium #2 | + 64GB instance overlay (misdiagnosis) | ❌ no-space (tmpfs) @ 17s |
| chromium #3 | + tmpfs=85% RAM, `--memory 96000` | ❌ 400: policy cap 16GB |
| chromium #4 | + `MaxBuildMemoryMB`=128GB | ❌ 500: instance cap 32GB |
| chromium #5 | + `max_memory_per_instance`=256GB, `--memory 65536` | ✅ **2m42s**, 1.5GB image, boots, artifacts verified |
| identical rebuild, no cache flags | — | 0 `CACHED` steps; full re-execution |

Builder BuildKit: v0.30.0. Base mirror covered `library/golang:1.25.0`,
`library/node:22-bullseye-slim`, `library/ubuntu:22.04`,
`ghcr.io/kernel/neko/base:3.0.8-v1.6.0` (public; private bases would need a
registry-credential story — also currently absent from the API).
