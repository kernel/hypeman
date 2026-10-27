# macOS guest port forwarding

`/instances/{id}/ports/{port}/*` forwards HTTP and WebSocket requests through the
guest agent (vsock 2223) to `127.0.0.1:{port}` in a running macOS guest. It
replaces ingress, which macOS guests do not support because their addresses are
vmnet NAT leases.

## Request handling

- The route requires the same `instance:write` scope as exec. Exec already runs
  commands as root in the guest, so the forwarder does not restrict which loopback
  port it reaches.
- The path after `/ports/{port}` and the query string are forwarded unchanged.
  The `Host` header is rewritten to `127.0.0.1:{port}`, because services such as
  Chrome DevTools reject other hosts. The `Authorization` header is removed.
- Responses come back unchanged. URLs a guest service returns (for example
  `webSocketDebuggerUrl` from `/json/version`) still name the guest's loopback
  address. Rewriting them for an external client is the caller's job.
- `GET`, `HEAD`, `POST`, `PUT`, `PATCH` and `DELETE` are forwarded. WebSocket
  upgrades are forwarded as ordinary `GET` requests.
- There is no response timeout, so long polls and slow responses work. A client
  disconnect ends the forward.

## Errors

| Status | Cause |
| --- | --- |
| 501 | Linux guest, a macOS image that does not declare the guest agent, or an agent that did not report the port-forward capability (including one that became ready before agents reported capabilities) |
| 400 | Port outside 1-65535 |
| 409 | Instance is not running (it is `Initializing` until the guest agent is ready) |
| 502 | Nothing is listening on the guest port, or the forward failed |
| 503 | Guest agent unavailable, forwarding limit reached, or an unrecognized handshake reply |

## Guest side

The guest agent listens on vsock 2223 only on Darwin. It binds the port before
reporting ready and advertises the port-forward capability only if the bind
succeeded, so an agent that reports the capability is already accepting. Each
host connection starts with the target port as two big-endian bytes. The agent
connects to that port on `127.0.0.1` and answers with one status byte (0
connected, 1 failed) before copying bytes both ways. The host treats any other
byte as a forwarder error. Either side closing ends the forward. The handshake
is defined once, in `lib/guest/portforward`, for both the host and the agent.

Each guest agent admits at most 64 concurrent forwards, including handshakes.
Excess connections are closed immediately, without dialing the guest service
or creating handler goroutines. Each handshake step on the agent (reading the
port, dialing it, writing the status) has its own five-second deadline, and the
host allows ten seconds for the whole handshake. Closing a forward releases its
slot.

Hypeman does not start or supervise services inside the guest. A browser, its
flags and its profile belong to the guest image, for example as a LaunchAgent in
the logged-in user's session that starts the browser with
`--remote-debugging-port`. Hypeman only forwards to the port the image chose.
