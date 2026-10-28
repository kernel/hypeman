# macOS base image publication

This is an **operator-controlled pipeline**, not an unattended Apple OS installer.
The first installation of a licensed Apple-silicon macOS guest, Setup Assistant,
local account policy, OS updates, Xcode/license acceptance, Python 3, and root
GuestService provisioning are prerequisites. No automated first-ever installation
is implemented here. See [Darwin GuestService](macos-guest-agent.md) and
[macOS builds](macos-builds.md) for the existing runtime/build contracts.

A development guest is **not** a clean production base. No real base was installed,
sanitized, published, or boot-validated as part of adding this tooling.

## Release boundary

1. Provision a fresh, dedicated guest. Never sign into Apple ID, a browser, cloud
   CLI, git credential helper, or a real developer/service account. Use no real
   passwords/tokens in scripts, recipe arguments, environment, or shell history.
2. Run the reviewed audit over the root GuestService, collect its structured
   evidence, gracefully shut down, and independently confirm VMM exit. Exclude
   restart and all writers until snapshot completes.
3. Publish the offline bundle using the local administrator command. It creates
   actual OCI machine content and verifies registry storage before writing an
   immutable manifest-digest receipt. It does not expose arbitrary filesystem
   reads over HTTP.
4. Submit a derivative through normal `/builds`; push it to a **candidate** registry
   tag via `/pushes`, create its local alias with `/images/{name}/tag`, and pre-pull
   the promoted digest on the target Mac using `/images`.
5. Cold-boot and validate the candidate sequentially. Only after approval use the
   same push/tag endpoints to move a production alias. Retain the previous digest
   for rollback; production consumers should pin digests, not mutable tags.

Identity is preserved, not rekeyed. Never boot the source guest, a derivative, or
another copy of the same identity concurrently, including on different hosts.
Do not assume distinct digests imply distinct VZ identity. CPU/memory must match
the installed base exactly; derivative builds inherit them by default.

## Audit and limited sanitation (inside guest)

Use Python 3 provisioned from a pinned, reviewed source. Run
`scripts/macos/audit-base.py --help` for arguments. The script requires root,
arm64, an exact list of non-system accounts, a pinned root-owned non-writable
agent binary and LaunchDaemon plist, the expected single-command root agent
job, and a running launchd service. The plist must not inject environment
variables. The normal host GuestService readiness gate and a successful final
GuestService exec of this script establish transport/exec functionality; the
plist flag or a readiness file alone does not. An audit run over SSH alone is
not sufficient evidence for publication.

Example final guest command, issued through the normal authenticated instance
exec interface (substitute reviewed paths/hashes; do not run on the Mac host):

```sh
/usr/local/bin/python3 /opt/base-audit/audit-base.py \
  --agent /usr/local/sbin/hypeman-guest-agent \
  --agent-sha256 "$EXPECTED_AGENT_SHA256" \
  --plist /Library/LaunchDaemons/com.hypeman.guest-agent.plist \
  --plist-sha256 "$EXPECTED_PLIST_SHA256" \
  --users builder --home-baseline /opt/base-audit/home-baseline.json \
  --clean-transient
```

The account name/job path above are examples, not an invented existing installer.
The plist/binary must first be provisioned according to the actual agent contract.
Copy only the successful JSON stdout to a private operator `audit.json`; reject
nonzero exec status, truncated output, or any extra output. Keep the exec job ID,
agent source commit, native cgo build command and artifact SHA256 in the private
release record. Build the Darwin agent with cgo on macOS as documented; a
cgo-disabled executable is not a functional native vsock agent.

`--clean-transient` only removes known shell-history files. It does not remove
credentials to turn a used guest into a clean one. The audit rejects credential,
browser, keychain, account, cloud and runtime-state locations, unknown accounts,
unknown home files, links/special files, SSH host keys, managed-device/autologin
configuration and Time Machine local snapshots. Missing tools, failed probes,
unrecognized Xcode output or incomplete checks fail closed. Diagnostics contain
fixed failure codes only, never file contents or command output.

Home directories (including root and Shared) must contain no files by default.
A private reviewed `home-baseline.json` can allow **specific nonsecret OS preference
files by exact absolute path and SHA256**, not whole directories or globs. `{}`
allows no files. Credential/browser locations remain forbidden even when present
in the baseline. Do not generate an allow-all manifest from a development guest:
review each entry for secret/state content first. Fresh macOS defaults may require
baseline review or deliberate removal, and unsupported configurations deliberately
fail instead of silently passing. Additional system-account homes or custom
application state need policy extension and review before release.

This is a policy audit, not a forensic erasure/secret detector or attestation of
all disk bytes. It cannot prove absence of arbitrary secrets in system files,
APFS deleted blocks, other APFS snapshots, OS keychains, provisioning artifacts,
or arbitrary custom paths. The operator must review those surfaces and enforce
the fresh/no-real-credentials prerequisite. If the guest has ever contained real
credentials, discard it and reinstall; deletion is not sufficient. Keep the guest
quiescent between final audit and graceful shutdown; do not reopen a browser or
run more provisioning commands. Review known OS-generated default keychains and
machine identity separately; never waive user credentials in the home baseline.

Audit evidence contains OS product/build, architecture, agent/plist hashes,
Xcode version/build, a toolchain probe hash (Xcode + clang), the reviewed home
baseline hash, and the audit script hash. It contains no user/account lists,
home paths, process output, tokens, or logs. Keep script, baseline and toolchain
inputs under version control in the private release source for reproducibility.
The audit is trusted operator input, not a cryptographically signed guest proof;
protect its custody and associate it with the exact guest/stop receipt.

## Offline base → verified OCI (Mac host)

Build `./cmd/publish-macos-base` on the Apple-silicon operator host from the reviewed
commit, with the repository's real embedded binaries built. On the native Mac,
run `make build-embedded build-vz-shim` first (requires the macOS SDK/cgo). Do not
use dummy embedded binaries. No HTTP import endpoint is added.
The source must be an exclusively owned, stopped, clean macvm bundle with exactly
these nonempty, private regular files under a real 0700 directory:

- `disk.img`: raw installed OS disk;
- `aux.img`: VZ auxiliary storage;
- `config.json`: strict `images.MacOSImage` metadata with `guest_agent: true`.

Do not add audit reports, logs, credentials, provenance files or sockets to that
bundle. Symlinks, aliased payload inodes, unknown metadata and public permissions
are rejected. Do not put evidence inside an imported disk. Stage any necessary
layout conversion under operator control before the final audit/stop.

```sh
umask 077
go build -o ./bin/publish-macos-base ./cmd/publish-macos-base
sudo ./bin/publish-macos-base \
  --source /private/base-release/stopped-bundle \
  --audit /private/base-release/audit.json \
  --registry https://internal-registry.example \
  --registry-secret-file /private/operator/internal-registry-signing-secret \
  --id base-macos-toolchain-001 \
  --output /private/base-release/base-receipt.json \
  --timeout 2h --stopped --clean-install-reviewed
```

Run as root so the closed-storage probe can inspect other users' open files.
The two attestations are explicit operator assertions, not automated proof of
sanitation or graceful stop. Use the normal strict stop/export path for managed
instances when available; that path confirms shutdown acknowledgement and owned
VMM exit under the instance lock. Offline `lsof` detects open storage but cannot
prove graceful shutdown or exclude a future restart. Keep the source under
exclusive operator control; never publish a bundle still managed by another
running process/API that could restart it. There is no claim of a lock shared
with arbitrary external VM launchers.

The command reuses `ImportMacOSImage`'s fail-closed closed-storage check and APFS
clones into an exclusive temporary data directory, canonicalizes imported machine
metadata, validates the strict snapshot, hashes its files, then reuses
`StreamingMachinePublisher`. This avoids inventing another OCI format/upload
implementation. Unlike import alone (which creates local raw-image metadata,
**not an OCI cache**), the publisher creates the OCI config/layer/manifest in
`builds/base-...:latest`. It hashes and verifies both stored blobs, commits the
manifest last and reads it back by digest. The receipt uses the immutable
`registry/builds/base-...@sha256:...` reference, never `latest`.

The signing-secret file is local administrator access to the configured internal
registry, never guest/recipe input. It must be a private regular file of 32–4096
bytes (surrounding whitespace is trimmed). The tool mints a deadline-bounded
repository-scoped token internally; never print it or pass the secret in argv.
Registry addresses follow the existing publisher policy: HTTPS/system trust or
loopback HTTP only, no embedded credentials, proxies or redirects. External
registry credentials belong to normal image push APIs, not this command.

The successful new 0600 receipt contains the immutable OCI reference/digest,
allowlisted audit fields, audit JSON hash, exact snapshot file hashes and UTC
publication timestamp. It is written only after verification. Existing output
files are never overwritten. An interrupted run may leave an unreferenced blob,
a committed candidate or a temporary directory; no receipt means unverified, not
proof that nothing was uploaded. Do not retry under a production alias. Operator
cleanup must first exclude active publication processes. Error text is deliberately
masked; inspect private authenticated registry/host diagnostics, not public CI logs.

## Derivative → candidate promotion → pre-pull

`scripts/macos/example/` is a no-download versioned recipe that inherits the pinned
base's OS/toolchain and installs a fixed validation marker. Produce a deterministic
archive with a pinned Python 3 toolchain (tar headers zeroed, sorted names, gzip
mtime zero):

```sh
python3 - <<'PY'
import gzip, io, pathlib, tarfile
root = pathlib.Path('scripts/macos/example')
with open('source.tar.gz', 'wb') as output:
    with gzip.GzipFile(filename='', fileobj=output, mode='wb', mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode='w|', format=tarfile.USTAR_FORMAT) as tar:
            for path in sorted(root.iterdir()):
                data = path.read_bytes()
                info = tarfile.TarInfo(path.name)
                info.size, info.mode = len(data), 0o600
                tar.addfile(info, io.BytesIO(data))
PY
```

The source tar/recipe is reproducible; an installed macOS disk with mutable OS
state is not claimed byte-reproducible across boots/builds. Retain archive hash,
base digest, source commits and all artifact/toolchain pins. Real browser recipes
must pin browser, Node, package and API artifacts and verify downloads, not rely
on floating install commands. Existing macOS browser provisioning is a downstream
recipe, not Apple OS installation or sanitation support here.

The pipeline requires Python 3, normal authenticated APIs, machine builds enabled
on the source server, and the configured internal registry reachable from both
Mac hosts. Provision push/pull credentials through existing server/operator
mechanisms; do not embed them in source. The caller supplies tokens via a protected
CI environment (never `set -x` or verbose HTTP logging):

```sh
python3 scripts/macos/pipeline.py \
  --api https://build-mac.example \
  --base-receipt /private/base-release/base-receipt.json \
  --source source.tar.gz \
  --target registry.example/macos-app:candidate-001 \
  --destination-api https://runtime-mac.example \
  --output /private/base-release/derivative-receipt.json --timeout 7200
```

Set `HYPEMAN_TOKEN` and `HYPEMAN_DEST_TOKEN` using the existing protected credential
mechanism. The script streams multipart source, bounds polling/HTTP requests,
requires digest-pinned inputs/outputs, verifies base/source build provenance,
waits for push success, and confirms source/destination image readiness, digest
and platform. It uses the normal cross-repository tag endpoint, not direct tag
symlink writes. API redirects/proxies and non-loopback plaintext are rejected.
Response bodies and guest logs are never printed or retained; evidence contains
only release refs, hashes, job IDs and explicit validation state. Timeout/failure
produces no success receipt; queued/running jobs can still exist and must be
inspected/cancelled through normal APIs. No automatic unsafe retry or rollback.

`destination_prepull: verified` proves image readiness/content admission, **not**
bootability. `boot_validation` remains `required-before-production-alias`.
Cold-boot the promoted immutable digest on one authorized Mac, require normal
GuestService readiness and UID0 exec, compare `sw_vers`/Xcode to audit evidence,
verify `/usr/local/share/hypeman/base-pipeline-marker`, and rerun the audited
credential/state policy adapted for the derivative's reviewed files. For browser
images additionally validate API health, pinned browser/runtime versions and an
unauthenticated smoke flow without real credentials. Gracefully stop and confirm
VMM exit before booting another copy. Save validation results privately against
the exact digest; only then move a production registry tag with `/pushes` and its
local tag with `/images/{name}/tag`. Pre-pull that digest on each intended host;
rollback uses the previously validated digest through the same APIs.

## Capacity and remaining production gates

Reserve capacity before an authorized run. A source disk, private APFS clone,
derivative writable disk, stopped export, OCI cache/raw conversion and registry
compressed blob can coexist. Clones are copy-on-write, not guaranteed free space.
Hashing reads logical sparse bytes; publication makes multiple streaming passes.
This tool adds no disk floor/quota/reservation policy; deployment capacity gates
must fail admission before staging. Retain enough free disk for the largest
logical disk/export/cache combination and recovery, not just compressed size.
Do not treat a reported free-space number as permission to modify a host.

Before production use: native Apple-silicon execution of these new scripts and
command; reviewed fresh-install/baseline/agent provenance; graceful-stop evidence;
real registry publication and destination pull; sequential cold-boot validation;
capacity/quota/recovery policy; approved derivative recipe and alias promotion.
Unit fixtures and API mocks are not substitutes for these gates. No live host
changes or deployments are performed by the repository tests.

Tests:

```sh
python3 -m unittest discover -s scripts/macos -p 'test_*.py' -v
go test ./cmd/publish-macos-base
go test -race ./cmd/publish-macos-base
```

These cover strict receipt/schema/pin admission, payload modes/links/extra files,
fail-closed home-state checks, secret-free failures, API transport policy,
multipart submission/provenance, and the normal push/tag orchestration. Native
snapshot/publication uses the existing importer/publisher tests plus a separate
Apple-silicon release validation; no tiny fixture is claimed to be a bootable OS.
