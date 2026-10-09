# macOS desktop and managed browser (in development)

The macOS runtime does not need a host viewer to keep its VMM running. A desktop
session inside the guest, desktop capture/input permission, and browser readiness
are separate capabilities: none follows from VMM `Running` or root-agent readiness.
The experimental spike's host Cocoa viewer is not a supported viewer API.

This branch introduces a transport-independent CDP proxy in `lib/desktop`. It is
not yet wired into the instance API and does not enable browser control on an
existing image. No desktop agent or TCC permissions are installed automatically.

## CDP transport boundary

`NewCDPProxy` supports GET discovery (`/json`, `/json/list`, `/json/version`,
`/json/protocol`) and browser/page debugger WebSockets. Its caller supplies a
fixed transport, strips the instance route prefix, and supplies a trusted,
instance-scoped public `ws`/`wss` base. Requests cannot select the upstream host,
port, path outside that allowlist, query, or arbitrary HTTP body.

The proxy forwards only discovery/WebSocket handshake headers, removing API
credentials, cookies, origin, and arbitrary application/forwarding headers.
It rejects redirects and invalid/oversized discovery responses. Advertised
WebSocket URLs must point to the fixed browser loopback endpoint and are rewritten
to the instance-scoped public base. Unproxied DevTools frontend links and guest
cookies are removed. Hosting a DevTools frontend is not included.

This component **does not authorize callers**. Before exposing it, API integration
must enforce instance-write authorization, resolved instance ownership, declared
desktop/browser capability, a running eligible guest, origin admission, and a
selected non-root desktop agent/session. Never derive the public base from an
untrusted Host or forwarding header. CDP grants full browser authority; the HTTP
path allowlist is not a CDP-command sandbox or a limit on websites visited.

Browser launch/status are separate management operations, not arbitrary proxied
paths. Worker-specific debugger paths and `/json/new`/close/activate endpoints
remain unsupported; browser-level CDP clients may manage targets over WebSockets.

## Validation status

Synthetic tests exercise discovery rewriting, credential isolation, request
admission, fixed-upstream behavior, malicious/oversized responses, redirects, and
an actual proxied WebSocket round trip. They do not prove live guest provisioning,
vsock routing, authenticated API integration, session selection, reconnect after a
guest/browser crash, capture/input/TCC, viewer attachment, or viewer-detach behavior.
Those remain gates before this desktop/browser feature is ready for use.
