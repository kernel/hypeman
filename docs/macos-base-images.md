# macOS base image publication

This is an **operator-controlled pipeline**, not an unattended Apple OS installer.
The first installation of a licensed Apple-silicon macOS guest is a prerequisite:
Setup Assistant, local account policy, OS updates, Xcode and license acceptance,
Python 3, and root GuestService provisioning. Pristine macOS is assumed to have
neither Python 3 nor full Xcode. Provision both from pinned, reviewed sources before
the audit. Command Line Tools alone do not satisfy `xcodebuild -version`. A missing
tool fails the prerequisite; the audit never installs software.

See [Darwin GuestService](macos-guest-agent.md) and [macOS builds](macos-builds.md)
for the runtime and build contracts. The current development guest is **not** a
clean production base. No real base has been installed, sanitized, published or
boot-validated by this tooling.

## Release flow

1. Provision a fresh, dedicated guest. Do not sign into an Apple ID, browser, cloud
   CLI or git credential helper, and do not use real accounts, passwords or tokens
   in scripts, arguments, environment or shell history.
2. Run the reviewed audit through root GuestService, collect its evidence, shut the
   guest down gracefully, and confirm VMM exit independently. Keep all writers
   stopped until the snapshot completes.
3. Publish the offline bundle with the local administrator command below. It writes
   an immutable manifest-digest receipt only after verifying registry storage.
4. Build a derivative with `POST /builds`, then push its digest with `POST /pushes`.
   Each Mac pulls it with `POST /images`. Push to a candidate tag first.
5. Cold-boot and validate the candidate sequentially. After approval, push the
   approved digest to the production tag. Keep the previous digest for rollback, and
   have consumers pin digests, not tags.

Identity is preserved, not rekeyed. Never boot the source guest, a derivative, or
another copy of the same identity concurrently, including on another host. Distinct
digests do not imply distinct VZ identity. CPU and memory must match the installed
base exactly; derivatives inherit them.

## Audit (inside the guest)

Run `scripts/macos/audit-base.py --help` for arguments. The script requires root,
arm64, an exact list of non-system accounts, a pinned root-owned non-writable agent
binary and LaunchDaemon plist, the expected single-command root agent job, and a
running launchd service. The plist must not inject environment variables.

The host GuestService readiness gate and a successful final GuestService exec of the
script establish transport and exec. A plist flag or readiness file alone does not.
An audit run over SSH alone is not sufficient evidence for publication.

Stage the script and baseline outside every audited home, for example under a
root-owned `/opt/base-audit`. Do not add them to the home allowlist to make an audit
pass. Pass hashes as literal GuestService exec arguments, not exported from an
interactive root shell. Close existing shells first: a shell can rewrite history on
exit after `--clean-transient` has removed it. The audit cannot detect future writers,
so keep the guest quiescent through shutdown.

```sh
/usr/local/bin/python3 /opt/base-audit/audit-base.py \
  --agent /usr/local/sbin/hypeman-guest-agent \
  --agent-sha256 "$EXPECTED_AGENT_SHA256" \
  --plist /Library/LaunchDaemons/com.hypeman.guest-agent.plist \
  --plist-sha256 "$EXPECTED_PLIST_SHA256" \
  --users builder --home-baseline /opt/base-audit/home-baseline.json \
  --clean-transient
```

The account name and job path are examples. Provision the plist and binary according
to the actual agent contract first. Copy only the successful JSON stdout to a private
operator `audit.json`. Reject nonzero exit, truncated output or extra output. Record
the exec job ID, agent source commit, native cgo build command and artifact SHA256 in
the private release record. Build the Darwin agent with cgo on macOS, since a
cgo-disabled binary is not a working vsock agent.

`--clean-transient` removes only known shell-history files. It does not make a used
guest clean. The audit rejects credential, browser, keychain, account, cloud and
runtime-state locations, unknown accounts, unknown home files, links and special
files, SSH host keys, managed-device or autologin configuration, and Time Machine
local snapshots. Missing tools, failed probes, unrecognized Xcode output and
incomplete checks fail closed. Diagnostics carry fixed failure codes only, never file
contents or command output.

Home directories, including root and Shared, must be empty by default. A private,
reviewed `home-baseline.json` can allow specific nonsecret OS preference files by
exact absolute path and SHA256. Globs and whole directories are not allowed, and `{}`
allows nothing. Credential and browser locations stay forbidden even if listed.
Do not generate an allow-all manifest from a development guest. Review each entry for
secret or state content. Unsupported configurations fail on purpose. Extra system-account
homes or custom application state need a policy change and review before release.

This is a policy audit, not a secret scanner or attestation over all disk bytes. It
cannot prove the absence of secrets in system files, APFS deleted blocks, other
snapshots, OS keychains, provisioning artifacts or custom paths. If the guest has ever
held real credentials, discard it and reinstall. Deletion is not sufficient. Review
OS-generated default keychains and machine identity separately, and never waive user
credentials through the baseline.

Audit evidence records OS product and build, architecture, agent and plist hashes,
Xcode version and build, a toolchain probe hash (Xcode and clang), the baseline hash
and the script hash. It records no account lists, home paths, process output, tokens
or logs. Keep the script, baseline and toolchain inputs under version control in the
private release source. The audit is trusted operator input, not a signed guest
proof. Protect its custody and tie it to the exact guest and stop receipt.

## Offline base to verified OCI (Mac host)

Build the tool on the Apple-silicon operator host from the reviewed commit, after
`make build-embedded build-vz-shim` produces the real embedded binaries (this needs
the macOS SDK and cgo). Dummy embedded binaries are not acceptable. No HTTP import
endpoint is added.

The source must be an exclusively owned, stopped, clean macvm bundle under a real
0700 directory, with exactly these nonempty private regular files:

- `disk.img`: raw installed OS disk
- `aux.img`: VZ auxiliary storage
- `config.json`: strict `images.MacOSImage` metadata with `guest_agent: true`

Keep audit reports, logs, credentials, provenance files and sockets out of the
bundle, and never place evidence inside an imported disk. Symlinks, aliased payload
inodes, unknown metadata and public permissions are rejected. Do any layout conversion
under operator control before the final audit and stop.

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

Run as root so the closed-storage probe can see other users' open files. `--stopped`
and `--clean-install-reviewed` are explicit operator assertions, not proof of sanitation
or graceful stop. For managed instances, prefer the strict stop and export path, which
checks shutdown acknowledgement and owned VMM exit under the instance lock. Offline
`lsof` detects open storage but cannot prove graceful shutdown or stop a future restart.
Never publish a bundle that another running process or API could still restart. There
is no lock shared with external VM launchers.

The tool reuses `ImportMacOSImage`'s closed-storage check, APFS-clones into an exclusive
temporary directory, canonicalizes metadata, validates the strict snapshot and hashes
its files. It then hands off to `StreamingMachinePublisher`, so there is no second OCI
format or upload path. Import alone creates local raw-image metadata, not an OCI cache.
The publisher writes the config, layer and manifest to `builds/base-...:latest`,
verifies both stored blobs, commits the manifest last and reads it back by digest.

The signing-secret file is local administrator access to the internal registry. It is
never guest or recipe input. It must be a private regular file of 32 to 4096 bytes,
with surrounding whitespace trimmed. The tool mints a deadline-bounded token scoped to
one repository. Never print the token or pass the secret in argv. Registry addresses
follow the publisher policy: HTTPS with system trust, or loopback HTTP. No embedded
credentials, proxies or redirects. External registry credentials belong to the
normal push API, not this command.

The 0600 receipt is written only after verification. It holds the immutable
`registry/builds/base-...@sha256:...` reference (never `latest`), allowlisted audit
fields, the audit JSON hash, exact snapshot file hashes and a UTC timestamp. Existing
output files are never overwritten. An interrupted run can leave an unreferenced blob,
a committed candidate or a temporary directory. No receipt means unverified, not proof
that nothing was uploaded. Do not retry under a production alias. Exclude active
publication processes before cleaning up. Errors are masked on purpose, so inspect
private authenticated registry and host diagnostics instead of public CI logs.

## Derivative build, external push and pull

Build the derivative with `POST /builds`, passing the pinned base digest and the source
archive. Add `machine_network_mode=egress` only if the recipe downloads pinned
artifacts. The example `build.sh` in `scripts/macos/example/` needs no network. Verify the
build's `provenance` before use.

```sh
curl -H "Authorization: Bearer $HYPEMAN_TOKEN" \
  -F "machine_base_image=registry.example/base@sha256:$BASE_DIGEST" \
  -F "source=@source.tar.gz;type=application/gzip" \
  "$HYPEMAN_URL/builds"
```

Push the digest with `POST /pushes`, giving the external repository and registry
credentials. Each Mac then runs `POST /images` with the digest and `platform:
darwin/arm64`. A pre-pull proves content admission, not bootability. Cold-boot the
digest on one authorized Mac and run the marker check and UID 0 exec before any
production use.

## Remaining production gates

- An approved fresh base installation, with its final audit and a confirmed graceful stop.
- Choice of external registry, and a test push of a 26–28 GB machine layer.
- Cold-boot validation of the promoted digest on an authorized Mac.
- Build-host disk capacity. The build path has no admission control in this change.
  Run machine builds serially on a dedicated host with enough free space for the base
  copy, the export and the published layer.
- Apple licence review for distributing macOS-derived images.
