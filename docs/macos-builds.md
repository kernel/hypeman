# macOS image builds (experimental, opt-in HTTP)

Ordinary build jobs can provision a digest-pinned installed macOS base in an
isolated VZ VM and publish a complete cold-boot OCI machine image. This is not
macOS installation/bootstrap, Linux rootfs conversion, memory forking or identity
rekeying. This branch depends on PR3's shared system GuestService.

`Config.MachineBuild` opts into `CreateBuildRequest.MachineBaseImage` at the Go
boundary. Queue, persisted request, deadline, source staging/hash, status, log
completion, provenance and image-readiness gate remain owned by the normal build
manager. The concrete implementations are `GuestMachineBuildDriver` and
`StreamingMachinePublisher`. HTTP activation is disabled by default. Set both
`macos_only: true` and `macos_builds: true` on an Apple-silicon VZ server, with
an explicit `registry.url` reachable from the host (not a guest gateway), including
its scheme. For example, use `http://localhost:4973` for loopback HTTP or a
system-trusted `https://` endpoint. Credentials, URL paths, queries and fragments
are rejected; custom CA/insecure HTTPS machine configurations are rejected.
Linux servers reject the machine selector.

Use the existing authenticated `POST /builds` multipart endpoint:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  -F "machine_base_image=registry.example/base@sha256:$BASE_DIGEST" \
  -F "source=@source.tar.gz;type=application/gzip" \
  -F "timeout_seconds=1200" "$API/builds"
```

The installed base must already be Ready. A 202 response queues an ordinary job;
GET /builds/{id}, cancellation and status/provenance use the shared APIs. OpenAPI
and generated server request types include machine_base_image; external language
SDK releases are separate, not claimed here. Only source, machine_base_image,
machine_network_mode, timeout_seconds, memory_mb, cpus and tags are accepted for machine HTTP builds.
Duplicates/unknown fields, malformed numbers and mutable bases return 400. Metadata
is limited to 64 KiB per field, source 64 MiB and the entire HTTP body 65 MiB before OpenAPI
validation. These limits do not change Linux source parsing.

## Use the built image on another Mac host

After a build is ready, `GET /builds/{id}` returns a digest-pinned `image_ref`
that can be passed directly to the normal instance-create API. For use across
hosts, configure `registry.url` to a registry address reachable from each Mac
host; loopback is only suitable for same-host builds. On a macOS-only server, an
omitted platform defaults to `darwin/arm64`; on a mixed host, request that
platform explicitly. If the image isn't cached locally, Hypeman starts a
platform-specific pull. Large machine images may take longer than the create
request's readiness wait, so retry after the image becomes ready or pull it
explicitly first.

```json
{
  "name": "macos-app",
  "image": "registry.example/builds/<build-id>@sha256:<manifest-digest>",
  "platform": "darwin/arm64"
}
```

Each instance is pinned to the resolved image digest. Use the build's returned
image reference until explicitly promoted. Local cross-repository image tags now
retain the raw disk, auxiliary storage and platform metadata together. Normal
`POST /pushes` publishes a candidate registry tag without changing the OCI manifest
digest; local tagging alone does not publish. The [base-image pipeline](macos-base-images.md)
provides build → candidate push → local tag → destination pre-pull orchestration.

## Base and policy

The ready installed base must be pinned by canonical SHA256, declare a provisioned
system agent on port 2222 and be `darwin/arm64`. Driver provisioning additionally checks
actual guest UID0. The driver never installs an agent or falls back to host
SSH/sudo. CPU/memory match the base exactly; zero request values inherit them.
Build limits, exact MiB precision and bounded deadlines still apply. Timeout
normally defaults to 600 s, capped at 24 hours; full machine-image compression/pull can need a
longer explicitly selected deadline.

Networking defaults isolated. Explicit `machine_network_mode=egress` enables
unrestricted VZ NAT for pinned, checksum-verified artifact downloads, not Linux
TAP/domain-policy parity. Omit the selector or set it to `isolated` for offline recipes. Domain allowlists, Linux Dockerfile/builder/cache/
build-argument/secret/admin options are rejected, not ignored. Template identity
is preserved: jobs pinned to the same base digest are serialized, and concurrent
same-identity guests remain forbidden.

## Experimental versioned provisioning recipe

Source is the normal build-owned tar.gz, privately staged and SHA256 verified,
including recovery. Compressed machine input is capped at 64 MiB; Linux staging gets
no new size cap. Host extraction into an exclusive 0700 temporary directory limits
uncompressed data 128 MiB, entries 1024 and individual regular files 64 MiB. Only regular
files/directories are accepted; duplicate names, links/devices and path escapes
fail before transfer. Files are 0600 or owner-executable 0700. Guest tar is not used.

The archive must contain `hypeman-macos-build.json`, limited to 64 KiB. The
[version 1 JSON schema](schemas/macos-build-v1.schema.json) describes its fields;
the server additionally rejects duplicate/case-variant keys and enforces byte limits:

```json
{"version":1,"steps":[{"command":["/bin/sh","build.sh"],"env":{"MODE":"release"}}]}
```

At at most 32 steps execute as root in one exclusive `/var/root/hypeman-build-{id}`
directory. Command executable paths must be absolute; arguments/environment are
bounded and reject NUL. Commands run only in the guest, never a host shell.
Unknown JSON fields, duplicate keys, null strings, case-variant field names and
extra JSON objects fail. The exact validated recipe bytes are SHA256-hashed in
`provenance.recipe_hash`, alongside the source archive hash and pinned base digest.
Version 1 remains experimental pending native production validation; no OS/compiler
version is inferred from the host or a base image name.

Sanitation removes and verifies absence of the build-owned guest source directory
and removes extracted host source before stopping. There is no injected registry
token, host credential or secret in the guest. Guest stdout/stderr are discarded.
Bounded host-authored lifecycle and step-number progress uses the existing build
logs/SSE endpoint; commands, environment values and raw guest errors never enter
that stream. This is progress logging, not arbitrary recipe-output streaming. A failed step reports
only its number and exit code, for example `machine build provision failed: recipe
step 1 exited with code 1`. Host admission errors, such as the base's
machine identity already being used by a running instance, are also shown; the
builder boots that identity, so instances made from the base must be stopped.
Source cleanup does not sanitize inherited accounts, SSH keys, browser state or
credentials. The [base audit/publication tools](macos-base-images.md) require a
reviewed fresh installation and explicit operator evidence; they do not certify a
used development guest as clean.

## Stop, export and cleanup safety

The publication path is two instance operations, not one copy-inside-stop:
`StopMacOSBuildInstance` performs only the strict graceful stop (no storage copy)
and rejects forced fallback, a pre-existing Stopped state, unmanaged/non-macOS
guests and any stop without shutdown acknowledgement plus owned VMM exit.
`ExportStoppedMacOSBuildInstance` later copies the closed storage of a
still-Stopped instance under the instance lock, so a concurrent Start makes export
fail rather than race it. Each storage file is copied once, into a private
exclusive 0700 output that holds only 0600 disk.img, aux.img and bounded config.json.

Failure cleanup uses `Abort`, not `Stop`. Nothing from a failed build is exported,
so cleanup first attempts the strict stop, then uses the ordinary forced
hypervisor stop and deletes storage only after confirmed VMM exit. A VM that still
has no confirmed exit remains quarantined for operator recovery. A re-enqueued
attempt of the same job first reaps the builder recorded by the interrupted attempt
(stop if running, then delete) and fails rather than start a second builder if that
delete fails. Builders orphaned outside this job path still have no general reaper. Cleanup has a 2-minute budget (strict stop window plus forced
fallback and deletion), separate from the build deadline.

On macOS-only server startup, the build manager recovers queued, building and
pushing machine jobs without preparing a Linux builder image or starting Linux
builder services. Persisted Linux jobs, and machine jobs whose backend is disabled,
remain unchanged on disk for recovery when their supported mode is enabled.

Darwin shutdown accepts a detached, short-delayed power-off command so gRPC can
send its acceptance reply before vsock disappears. RPC cancellation after
acceptance does not undo shutdown. The host still independently awaits VMM exit;
an acknowledgement alone is insufficient.

The backend validates complete output and preserved identity/resources and destroys
the stopped builder **before** publication. Staged source is retained until a terminal
status is persisted, so a job interrupted while pushing or converting is rebuilt on
recovery; startup also reaps cancelled builders and removes terminal-job source. Each
attempt first removes export and guest workspaces left by an interrupted attempt of
the same job; every export holds a full disk image. Cancellation
interrupts the active normal machine job, not forced instance deletion. It records
the owned builder before provisioning and keeps Cancelled terminal. Potentially secret-bearing guest errors remain masked in
ordinary error/log strings while errors.Is/As work internally.

## Streaming publication

The publisher uses a server-configured registry and a token scoped only to
`builds/{id}`. Recipes cannot select destinations or credentials. HTTPS uses system
trust; plaintext is permitted only on loopback. Redirects are never followed and
environment HTTP proxies are disabled for these credential-bearing requests.

The layer is never written to disk. A first pass generates the gzipped tar from
the exported bundle only to compute its compressed digest/size and uncompressed
DiffID; the upload pass generates the same bytes again (tar headers carry no
timestamps, so the output is deterministic) and the registry rejects any digest
mismatch. Configuration and layer blobs stream through monolithic POST uploads,
not go-containerregistry's large-buffer PATCH path. Each persisted
blob is independently GET/hash/size verified. The `latest` manifest is committed
last (the normal manager's image-readiness convention), then immutable digest
content is verified. Ambiguous errors do not return successful receipts. Disk
admission settings are described below; a compressed-size bound is not a storage quota.

## Validation and remaining gates

Repeated race tests cover source/recipe admission, shared queue/status/provenance,
cancellation without publication, phase failures/quarantine, bundle identity/modes,
closed-storage/hardlink rejection and authenticated production-registry publication
with manifest-last ordering and redirect isolation. Tiny test disks are synthetic,
not bootability evidence.

Native Apple-silicon QA has run real builds end to end through the HTTP API:
provisioning from a pinned installed base, sanitation, strict stop, export,
publication, the readiness gate and a cold boot of the result with the build
marker present. It also covered recipe-step failures, cancellation, API crashes
during provisioning, publication and conversion (each rebuilt on recovery), and
host admission errors. These runs used a development base, not a sanitized
production template.

Remaining gates are native execution of the new pipeline and admission code,
a reviewed fresh base with approved artifact pins, cross-host publication/pull and
sequential cold-boot validation, operational monitoring and SDK/CLI release. The
broader Linux runtime suite must also pass in an environment with its required
hypervisor/network permissions. Keep experimental HTTP activation disabled by default.

## Storage admission

The native backend requires `build.machine_max_concurrent` (default 1),
`build.machine_max_export_bytes` (default 128 GiB), and
`build.machine_min_free_bytes` (default 10 GiB). Effective worker concurrency is the
smaller of `machine_max_concurrent` and `max_concurrent_source_builds`. Linux builds
ignore these machine-only settings.

Admission budgets `4 × machine_max_export_bytes + 256 MiB` per active/quarantined
job, plus the configured free-space floor. Defaults therefore require about
522.25 GiB free for one job. Choose an explicitly reviewed export cap for the
actual logical disk/auxiliary size and available capacity; sparse allocation and
APFS cloning are not permission to lower that logical-size budget. Headroom is
rechecked before export and publication, and files are size-checked before native
export copying. Queued source uploads share the same admission lock.

Reservations persist across restart, cancellation and ambiguous publication until
the builder and asynchronous image import are safely reconciled. They are admission
budgets, not filesystem quotas: other processes/writers are not coordinated. A
quarantined reservation is not permission to delete a VM, disk or marker manually;
first prove VMM exit, workspace cleanup and terminal image import. Offline initial
base publication has its own operator capacity preflight and is not covered by the
build backend reservation.

## Rollout gates and rollback

1. **Contract gate:** pass build, image, registry/push, config and API contract
   tests with real embedded binaries; run pipeline Python tests. The Linux suite
   must pass on a host with its normal hypervisor/network prerequisites. Fixtures
   prove admission/transport, not a bootable macOS installation.
2. **Base/recipe gate:** review a fresh installation and final GuestService audit,
   retain the strict stop evidence, publish the base receipt, and approve all
   downstream browser/runtime artifact hashes. Never promote a development guest
   or the placeholder example pins. The initial Apple install remains an explicit
   operator prerequisite.
3. **Native failure gate:** on an isolated authorized builder, exercise success,
   recipe failure, cancellation and process interruption during provisioning,
   export, publication and import. Restart and verify shared job recovery; verify
   failed/cancelled jobs never report Ready and quarantined storage is not released
   before confirmed VMM exit and terminal import. Check safe logs contain only
   lifecycle/step progress, not test secrets.
4. **Distribution/canary gate:** publish only a candidate tag, pre-pull the exact
   digest on a second approved Mac, then cold-boot sequentially with no other copy
   of the identity running. Require agent readiness, root exec, expected OS/tool
   versions and workload health. Preserve the previous validated image digest and
   successful private evidence before changing any production selection.
5. **Operations gate:** enable one explicit builder and canary first. Record build
   success/failure counts, queue/active jobs, build duration, filesystem free space
   and active/quarantined reservations using the [build metrics](../lib/otel/README.md#source-builds).
   Treat missing filesystem telemetry as a failed gate, not zero usage. Configure operator-owned alerts and stop
   expansion on any unresolved quarantine, free-space-floor violation, missed
   deadline, wrong digest/platform, or unexpected workload failure. No production
   alert routing or inventory is fabricated by this change.

For rollback, first revoke new build submissions at the authenticated admission
boundary and drain/cancel existing jobs through normal APIs while keeping the
backend available for cleanup/recovery. Repoint workload selection to the prior
validated immutable digest and pre-pull it through the same image API. Only after
jobs/imports and quarantine recovery are resolved, set `macos_builds: false` on
the dedicated builder. Disabling the backend does not delete persisted jobs or
images. Do not delete the old digest or stop an unrelated instance to free space.
