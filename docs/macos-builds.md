# macOS image builds (experimental, opt-in)

A build job can provision a digest-pinned installed macOS base in an isolated VZ
VM, run the source's `build.sh` in it, and publish the result as an OCI machine
image. This is not macOS installation, Linux rootfs conversion or memory forking.
It depends on the shared system GuestService.

The normal build manager owns the queue, persisted request, deadline, source
staging, status, provenance and the image-readiness gate. `GuestMachineBuildDriver`
runs the guest and `StreamingMachinePublisher` writes the registry.

## Enabling

Off by default. Set `macos_only: true` and `macos_builds: true` on an Apple-silicon
VZ server, with `registry.url` set to an address the host can reach (not a guest
gateway), including its scheme: `http://localhost:4973` for loopback, or a
system-trusted `https://` endpoint. Credentials, paths, queries, fragments, custom
CAs and insecure HTTPS are rejected. Linux servers reject the machine selector.

```sh
curl -H "Authorization: Bearer $TOKEN" \
  -F "machine_base_image=registry.example/base@sha256:$BASE_DIGEST" \
  -F "source=@source.tar.gz;type=application/gzip" \
  -F "timeout_seconds=1200" "$API/builds"
```

The base must already be Ready. A 202 queues an ordinary job; status, cancellation
and provenance use the shared `/builds` APIs. Only `source`, `machine_base_image`,
`machine_network_mode`, `timeout_seconds`, `memory_mb`, `cpus` and `tags` are
accepted. Duplicate or unknown fields, malformed numbers and mutable bases return
400. Limits: 64 KiB per metadata field, 64 MiB source, 65 MiB total body. Linux
source parsing is unchanged.

## Using the result

`GET /builds/{id}` returns a digest-pinned `image_ref` for instance create. Other
Mac hosts need `registry.url` reachable from them; loopback only works on the build
host. A host that hasn't cached the image pulls on create, which can exceed the
readiness wait for large images, so retry or pre-pull. Distribute by digest with
`POST /pushes`. Consumers should pin the digest, not a tag.

## Base and policy

The base must be pinned by canonical SHA256, be `darwin/arm64`, and declare a
provisioned system agent on port 2222. The driver also checks guest UID 0 and never
installs an agent or falls back to host SSH. CPU and memory match the base exactly;
zero request values inherit them. Timeout defaults to 600 s, capped at 24 h.

Networking defaults to isolated. `machine_network_mode=egress` gives unrestricted
VZ NAT for checksum-verified downloads; it is not a domain allowlist. Linux builder,
cache, build-arg, secret and admin options are rejected.

Each builder gets its own machine identifier and MAC, and its SSH host keys are
rekeyed on first start, the same way a cold clone is. The output keeps that identity.
Builds on one base run concurrently, and an output can run beside its base.

## Source

The source tar.gz is staged privately and SHA256-verified, including on recovery,
and `provenance.source_hash` pins exactly what ran. Input is capped at 64 MiB
compressed, 128 MiB uncompressed, 1024 entries and 64 MiB per file. Only regular
files and directories are accepted; links, devices, duplicates and path escapes fail
before transfer. The archive must contain a non-empty `build.sh` at its root. It is
copied to `/var/root/hypeman-build-{id}` in the guest and run as root with
`/bin/sh build.sh`.

Before stopping, the guest's build directory is removed and its absence verified,
and the host copy is removed. No registry token, host credential or secret is
injected. Guest stdout and stderr are discarded, so build logs never contain guest
output. Failures report the phase and, for the script, its exit code.

## Stop, publish and cleanup

Publication follows two instance operations. `StopMacOSBuildInstance` does a strict
graceful stop and rejects forced fallback, an instance already Stopped, non-macOS
guests, and any stop without shutdown acknowledgement and confirmed VMM exit.
`MacOSBuildStorage` then returns the closed storage of the still-Stopped instance,
checked under the instance lock: its read-only ASIF layers, its writable overlay
`boot.asif`, its auxiliary storage and its platform. Nothing is copied. The build
checks that the overlay opens as an ASIF stack on those layers, that the hardware
model and resources match the base and that the identity differs. A Start during
publication changes the overlay, so the registry's digest check fails the upload.

Failure cleanup uses a forced stop and publishes nothing. Storage is deleted only after
confirmed VMM exit; a VM with no confirmed exit stays quarantined for an operator.
Cleanup has its own 2-minute budget. A re-enqueued attempt first reaps the builder
left by the interrupted attempt, and fails rather than start a second one if that
delete fails. Builders orphaned outside this path have no general reaper.

The builder is destroyed **after** the registry has verified the publication, since
its storage is the publication source. Source stays staged until a terminal
status is persisted, so a job interrupted while pushing is rebuilt on recovery.
Startup on a macOS-only server recovers machine jobs without preparing Linux builder
state, reaps cancelled builders and removes terminal-job source. Cancellation
interrupts the running job and keeps Cancelled terminal. Guest errors that may carry
secrets are masked in strings and logs.

## Publication

The publisher uses a server-configured registry and a token scoped to `builds/{id}`.
Plaintext is allowed only on loopback; redirects are refused and proxies are
disabled. It writes the image with go-containerregistry's `remote.Write`. The layer is
never written to disk on the publishing side: a first pass computes its digest, size
and DiffID, and the upload regenerates the same bytes. Blobs the registry already
holds are skipped, and the manifest is committed last. The registry writes chunked
uploads to disk, hashing as they arrive, and commits a blob only if it matches its
digest. It serves manifests by digest from its blob store, so a base stays usable
after a restart. Ambiguous errors never return success.

The image is the base's layers plus one layer holding the builder's overlay,
auxiliary storage and platform. The overlay holds only the build's writes, so for a
base in the same registry only that layer is uploaded; the base's layers are reused
by digest. A base outside the registry has its layers uploaded too. There is no
garbage-collection rule yet; deleting a base that a derivative references breaks a
later re-pull of that derivative.

## Status and gates

Race tests cover admission, the shared queue, cancellation without publication, phase
failures and quarantine, storage validation, and authenticated publication with
manifest-last ordering and base-layer reuse. Test disks are synthetic and prove nothing about bootability.
A development base has been run end to end through the HTTP API on Apple silicon,
including a cold boot of the result; this trimmed branch has not been re-run natively.

Before production use: a reviewed fresh base with approved artifact pins (base
publication tooling is a separate PR), cross-host publish and pull with sequential
cold boot, disk-space monitoring (this change has no disk admission, so run machine
builds serially on a dedicated host), SDK and CLI release, and Apple licence review.
Keep `macos_builds` off until then.

**Rollback:** stop new submissions, let or cancel running jobs through the normal
APIs, repoint workloads to the prior validated digest, then set `macos_builds: false`.
Disabling does not delete persisted jobs or images.
