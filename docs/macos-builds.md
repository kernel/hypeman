# macOS image builds (PR5, foundation only)

Goal: ordinary build jobs provision a digest-pinned installed macOS base inside
an isolated VM, then publish a complete cold-boot OCI machine image. This is not
Linux rootfs conversion, macOS installation/bootstrap, memory forking or safe
identity rekeying.

This checkpoint adds an **internal lifecycle/publication contract** and tests.
It does not register an HTTP build mode, choose a recipe language, implement the
VZ/GuestService driver or stream an artifact into a production registry. Existing
Linux build behavior and macOS-only build rejection are unchanged. Do not claim
that normal macOS builds work from this foundation alone.

## Dependency and recipe boundaries

The branch begins on PR2's machine-image support. The real driver additionally
requires reviewed PR3's system GuestService/readiness/lifecycle changes; desktop
provisioning needs PR4 where applicable. Integrate those dependencies before API
activation. Do not replay stale guest-agent code or bypass identity exclusivity.

No Macfile syntax or unrestricted Dockerfile/BuildKit compatibility is introduced.
Packer versus a restricted familiar provisioning recipe still needs a deliberate
choice before the public request/source schema is frozen. The internal driver
consumes the existing build job's staged source archive; it must run recipe
commands in the isolated guest, never in a host shell. Public source/log/secret/
cancellation/provenance/output contracts should remain shared across OS backends.

## Lifecycle contract

`MachineBuildRunner` requires a ready installed `darwin/arm64` base pinned by a
canonical SHA256 reference. CPU/memory must match that base (zero inherits), fit
existing build limits, and preserve exact MiB precision. Negative/unbounded
resource or timeout values are rejected before VM start. Timeout defaults to 600
seconds and is capped at 24 hours. Networking defaults to isolated; egress means
unrestricted VZ NAT, not domain/TAP/policy parity. Domain allowlists are rejected.

Source input is staged in a private server-owned workspace, with bounded streaming
(default 64MiB compressed input), cancellation checks, SHA256 provenance and optional
expected-hash verification before start. Guest extraction must independently bound
expanded archive size and reject traversal/symlink escapes; this is not implemented
by the host compressed-size bound.

The successful order is:

1. Resolve pinned installed base and validate resource policy.
2. Stage/verify source; start one owned builder with a valid identity lease.
3. Provision and sanitize build credentials/source/secret artifacts.
4. Obtain a graceful-stop receipt **and actual VMM exit**.
5. Export matching disk/aux/platform into a separate build-owned directory.
6. Validate the complete bundle, preserve base identity/resources, reject extra
   files and unknown platform fields, and make payload modes private.
7. Destroy the stopped builder and remove staged source before publishing.
8. Stream blobs and commit/verify the manifest last through a server-owned,
   build-scoped publisher; return a matching immutable digest receipt.

The driver must reject unprovisioned system agents; it cannot bootstrap via an
unreviewed host SSH/sudo fallback. Default lifecycle APIs that silently force-stop
are insufficient evidence of graceful export readiness. A forced stop or any
pre-publication failure must not export/publish a successful image. Sanitation is
a mandatory driver operation, not something structural bundle validation proves.
Base credentials and reusable-image SSH/account/browser/TCC policy remain a
separately reviewed requirement.

Cancellation/failure cleanup gets its own bounded 30-second context. It may destroy
instance storage only after a receipt confirms VMM exit. Otherwise the instance
is quarantined for operator recovery, never deleted or published. Instance storage
must be outside the input/export workspace; that private workspace is removed by
the runner. Raw guest error text is masked in ordinary error/log strings while
`errors.Is/As` remain available internally. The concrete log stream must separately
redact injected secrets before retention/forwarding.

Publication starts only after validation and fallible builder/source cleanup.
A failed network commit may have ambiguous remote effects; partial blobs are not
success, and no ready result is returned without a verified receipt. Atomic
manifest-last behavior and reconciliation belong to the concrete publisher;
these interfaces alone do not prove remote nonpublication after a lost response.

## Validation and remaining gates

Synthetic tests cover input/resource/hash/size admission before VM start, exact
phase order, partial-start cleanup, independent cancellation cleanup, forced/
unconfirmed-stop nonpublication and no deletion of live storage, failed-stage
nonpublication, identity changes, extra/unknown secret metadata rejection, private
output modes, error-text masking and verified digest/source provenance. Expanded
failure tests include partial source-read failure, actual deadline expiry with
independent cleanup, ambiguous publisher errors without retry, invalid/tagged/
mismatched digest receipts, empty disk/aux, symlink/hardlink payloads, truncated/
oversized metadata, invalid Ethernet MAC and changed resources. Repeated existing
build queue/cache/storage/secret-provider/registry-token regressions, CGO-disabled
machine tests and `go vet ./lib/builds` also pass locally.

No actual VM is created and no guest commands or registry uploads are executed by
these tests. Fake tiny disk files establish contract behavior, not bootability.
The shared machine-bundle validator is the same structural validator used for OCI
imports, not a macOS/VZ integrity or credential scanner.

Remaining: concrete driver and streamed publisher, common build queue/status/log/
secret/API integration, pinned toolchain/version provenance, recipe choice and
source validation, storage floor/quota enforcement, sanitation audit, large-layer
upload transport validation, repeat builds and output cold boot through normal
APIs. Keep PR5 draft and runtime admission disabled until these gates pass.
