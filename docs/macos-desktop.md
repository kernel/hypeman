# macOS desktop and managed browser (draft)

The macOS runtime does not need a host viewer to keep its VMM running. VMM
`Running`, system GuestService readiness, selected desktop-session readiness,
managed browser readiness, and capture/input permission are separate. The spike's
host Cocoa viewer is not a supported viewer API.

This branch adds authenticated instance CDP routes, a bounded fixed-upstream proxy,
and a versioned desktop-role service backed by a non-root Darwin Chrome launcher.
It **does not install a desktop LaunchAgent automatically**. Existing images and
the spike's old GUI agent do not satisfy the new handshake. No guest or host
TCC permissions are installed automatically.

## Explicit admission

An administrator must configure `macos_desktop_origin` to the public API origin,
for example `https://hypeman.example`. Empty disables the routes. Configuration
rejects credentials, paths (other than `/`), fragments, queries, and non-HTTP schemes.
Discovery never trusts incoming Host or forwarding headers.

The machine-image metadata must declare `desktop_agent_uid` as the provisioned
non-root user's UID. Zero disables desktop operations. This capability is separate
from `guest_agent` (system GuestService on 2222), and `SkipGuestAgent` disables both.
The desktop role uses fixed vsock port 2223, not a client-selected endpoint. Its
native Darwin listener admits host-CID connections only. A declaration is a
capability claim, not a credential or proof of provisioning.

All routes require JWT `instance:write`, including discovery and status, and use
the same instance resolution/authorization model as exec. Read-only tokens are
rejected before resolution or guest connection. This does not add multi-tenant
ownership semantics beyond the existing API's scoped-token model.

Only running macOS/VZ guests with an enabled desktop capability are admitted.
Present Origin headers must exactly match the configured scheme/authority;
multiple, null, cross-origin, and malformed origins are rejected. Native clients
may omit Origin but must still authenticate; cookies/query tokens are not accepted
as authentication. Encoded selectors/paths are not supported on these routes; use
the canonical instance ID (ordinary names/aliases resolve normally).

## Management and readiness

- `GET /instances/{id}/cdp/status`: live desktop handshake plus derived
  `desktop_ready`; this is not a persisted boot marker or VMM/system-agent health.
- `POST /instances/{id}/cdp/start`: explicit fixed-policy browser launch. No body,
  query, command, arguments, browser binary, profile, or user selection is accepted.
- `GET /instances/{id}/cdp/json[/list|/version|/protocol]`: CDP discovery.
- `GET /instances/{id}/cdp/devtools/{browser|page}/{id}`: debugger WebSocket.

The protocol-v1 handshake must report Darwin/arm64, the declared non-root UID,
the active console UID and GUI-session availability. Desktop readiness requires
matching console/agent UIDs and a GUI session. Browser readiness additionally
requires the backend to establish ownership of the managed browser; discovering
an open debugging port is insufficient. Inconsistent or incompatible handshakes
are rejected. This does not claim an unlocked screen or TCC permission.

`lib/desktop.NewService` defines the guest role's status, launch and CDP boundary.
`DarwinBackend` checks `/dev/console` ownership and `launchctl managername` (`Aqua`),
uses the fixed `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`
binary and private `~/Library/Application Support/Hypeman/Chrome` profile, refuses
occupied unmanaged debugging ports, serializes launches and observes child exit.
Profile directories must belong to the selected user, have no symlink components,
and use private permissions for the Hypeman subtree. Chrome inherits only HOME
and a fixed PATH, not arbitrary agent environment/credentials. It is headful.

Readiness requires `lsof` to identify the fixed IPv4 loopback listener on the
agent's own launched Chrome PID, followed by bounded validated Chrome discovery.
The browser is not killed when the launch request, CDP socket or viewer closes.
After a browser crash, an explicit start can launch a replacement. Agent restart
adoption of a surviving Chrome is deliberately unsupported: the new agent refuses
to attach to that unmanaged listener rather than guessing process ownership. No system shutdown, arbitrary exec, file access or OS input is
available through this role. The system GuestService remains separate.

Host probes are bounded (3 seconds for status, 20 for explicit launch, at most
8KiB of JSON). The guest service bounds status/launch work and serializes launches.
Both sides limit active CDP sessions to 16; host admission also counts management
requests. Connections release slots on disconnect, without a guest shutdown call.
Real process ownership, session detection, startup and recovery remain live
validation gates; these OS interactions have not been exercised by this PR's tests.

## CDP transport boundary

The host `NewCDPProxy` is the only discovery-validation/public-URL rewriting
boundary. The host-only guest listener uses `NewCDPForwarder` to carry Chrome's
fixed-upstream discovery unchanged. Both retain body/path/query/encoding admission,
header isolation and no-redirect policy; guest session/browser ownership is still
checked before forwarding. The guest forwarder is not a public authenticated proxy.

`NewCDPProxy` supports GET discovery and browser/page debugger WebSockets. Its
caller supplies a fixed transport, strips the instance route prefix, and supplies
a trusted instance-scoped `ws`/`wss` base. Clients cannot select the upstream host,
port, other HTTP paths, query or body. Advertised debugger URLs must point to the
fixed browser loopback endpoint (`127.0.0.1:9222`), then are rewritten to the
canonical instance ID even when accessed by an alias.

Only discovery/WebSocket handshake headers are forwarded. API credentials,
cookies, origin, URL userinfo, and application/forwarding headers are stripped.
Redirects, invalid/oversized discovery (2MiB), guest cookies, and unproxied DevTools
frontend links are rejected/removed. Hosting a frontend is not included.

CDP grants full browser authority; the HTTP path allowlist is not a command
sandbox or a limit on websites visited. Worker-specific debugger paths and
`/json/new`/close/activate remain unsupported; browser-level CDP can manage targets.
There is no transparent browser auto-launch on discovery or WebSocket connection.

## Provisioning (isolated guest only)

Build `./lib/system/guest_agent` for Darwin/arm64 with CGO enabled. Provision the
binary at `/Library/Application Support/Hypeman/guest-agent` in the image and
Chrome at the fixed application path above. Installation in this protected path
requires separately authorized guest provisioning, not a host-root installer.

The example `docs/examples/macos-desktop-agent.plist` belongs in the selected
user's `~/Library/LaunchAgents/`, not `/Library/LaunchDaemons/`. Bootstrap it in
`gui/<uid>` as that user after login; it runs the same binary with `--role desktop`.
Do not configure UserName=root or launch it in the system domain. Default/no-role
invocation remains the existing system GuestService on 2222. Desktop role is
unsupported off Darwin or without native CGO vsock support, and rejects root or
set-ID execution. Only mark `desktop_agent_uid` on a stopped matching image after
provisioning and handshake validation. System-agent readiness must not be used as
proof that the desktop role is provisioned.

No install/bootstrap command has been run by these tests. This example does not
provide automatic login, credential storage, TCC grants, rekeying or fleet rollout.

## Validation status

Synthetic tests cover role/version/UID validation, readiness separation, fixed
launch admission, bounded probes, trusted-origin validation, real JWT/write-scope
middleware and instance resolution, unsupported/stopped/disabled admission before
dial, alias-safe discovery rewriting, credential isolation, malicious/oversized
responses, redirects, active-session caps, actual WebSocket round trips, reconnect
and slot release. Darwin-specific tests inspect fixed launch arguments/environment,
listener-ownership parsing and private-profile/symlink policy without launching
Chrome. Focused tests run with the race detector.

OS desktop capture/input, automatic TCC grants and a supported viewer API are not
implemented in this checkpoint; they remain in this draft's display scope.

Not yet proven: live Darwin backend operation, LaunchAgent provisioning/console selection,
native desktop listener/API vsock routing, browser process ownership and crash
recovery, agent restart recovery, capture/input/TCC, viewer attachment/detachment.
The new routes are disabled by default and this feature remains a draft until
those gates pass in an explicitly authorized isolated guest. No live deployment
or guest provisioning is implied by these local tests.
