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
timeout_seconds, memory_mb, cpus and tags are accepted for machine HTTP builds.
Duplicates/unknown fields, malformed numbers and mutable bases return 400. Metadata
is limited to 64 KiB per field, source 64 MiB and the entire HTTP body 65 MiB before OpenAPI
validation. These limits do not change Linux source parsing.

## Base and policy

The ready installed base must be pinned by canonical SHA256, declare a provisioned
system agent on port 2222 and be `darwin/arm64`. Driver provisioning additionally checks
actual guest UID0. The driver never installs an agent or falls back to host
SSH/sudo. CPU/memory match the base exactly; zero request values inherit them.
Build limits, exact MiB precision and bounded deadlines still apply. Timeout
normally defaults to 600 s, capped at 24 hours; full machine-image compression/pull can need a
longer explicitly selected deadline.

Networking defaults isolated. Optional egress means unrestricted VZ NAT, not
Linux TAP/domain-policy parity. Domain allowlists, Linux Dockerfile/builder/cache/
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

The archive must contain `hypeman-macos-build.json`, limited to 64 KiB:

```json
{"version":1,"steps":[{"command":["/bin/sh","build.sh"],"env":{"MODE":"release"}}]}
```

At at most 32 steps execute as root in one exclusive `/var/root/hypeman-build-{id}`
directory. Command executable paths must be absolute; arguments/environment are
bounded and reject NUL. Commands run only in the guest, never a host shell.
Unknown JSON fields and extra JSON objects fail. Version 1 remains experimental;
public source/schema/toolchain contracts require review before merge.

Sanitation removes and verifies absence of the build-owned guest source directory
and removes extracted host source before stopping. There is no injected registry
token, host credential or secret in the guest. Guest stdout/stderr are not forwarded
or retained yet; recipe-level log streaming is still a gate. A failed step reports
only its number and exit code, for example `machine build provision failed: recipe
step 1 exited with code 1`. Host admission errors, such as the base's
machine identity already being used by a running instance, are also shown; the
builder boots that identity, so instances made from the base must be stopped. This does not sanitize
inherited template accounts, SSH keys, browser state or credentials: use a separately
reviewed clean base, not a private development guest, for distributable images.

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
quota/floor policy belongs to deployment; a compressed-size bound is not a storage quota.

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

Remaining: external SDK releases, common safe recipe logs,
clean-template sanitation policy/audit, toolchain/version provenance, production
storage quotas/floors and Linux runtime regressions. Keep PR6 draft and experimental HTTP activation disabled by default.
