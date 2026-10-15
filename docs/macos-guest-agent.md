# Experimental Darwin GuestService

The existing guest-agent executable now builds for macOS and serves the same
`guest.GuestService` gRPC contract as Linux on vsock port **2222**. It reuses the
copy-to/from-guest and stat implementations. Exec on Darwin runs a separate
lifecycle (`exec_lifecycle.go`): a client disconnect cancels the command,
cancellation kills its process group, and output drains are bounded. Linux exec
is unchanged.

## Build

Build on a macOS development machine with the macOS SDK and cgo enabled:

```sh
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
  -o guest-agent-darwin-arm64 ./lib/system/guest_agent
```

Darwin native AF_VSOCK requires cgo. A cgo-disabled Darwin executable builds but
refuses to listen with an explicit error. Linux keeps its existing Go vsock
listener and cgo-disabled build path. The Darwin listener accepts only host
CID 2, marks descriptors close-on-exec, and provides net.Conn deadlines for
gRPC. Do not start the executable on the development host to test guest services.

## Provisioning boundary

Install the binary **inside a stopped-template provisioning guest**, not on the
host. System operations need a root LaunchDaemon. Use a root-owned, non-writable
binary location and launchd configuration; provision permissions and logs
explicitly. The default readiness file is `/var/run/hypeman/guest-agent-ready`
(`HYPEMAN_AGENT_READY_FILE` can override it).

A listening system agent does not establish autologin, desktop readiness, TCC
permissions, or browser readiness.

The host API and hypervisor still enforce authorization. The vsock host-CID
check is transport admission, not a replacement for instance authority checks.
As with the Linux guest agent, commands and file operations run as root with no
per-caller credential switch; the host API alone decides who may reach them.
An exec `timeout_seconds` ends the command; it does not bound a healthy stream.
Do not expose this privileged service through unauthenticated host forwarding.

## Image declaration and host integration

Set `"guest_agent": true` in the image's experimental macOS platform configuration
only after provisioning the root shared agent on vsock 2222. This field is omitted
by default, so existing local templates remain unmanaged. The normal instance
`skip_guest_agent` option can disable agent integration even for a declared image.

For declared/enabled instances, the normal exec and file-copy handlers use the
shared GuestService and stop attempts its Shutdown RPC before the existing
forced-stop fallback. An unavailable agent still produces a timeout/failure,
not an assertion that it is ready. Readiness probing records the existing
`guest_agent_ready_at` marker without requiring or inventing a Linux workload
start marker. An enabled instance is `Initializing` until that marker is recorded,
then `Running`; this does not certify desktop or browser readiness. This readiness timestamp is a boot observation,
not continuous agent health. Start clears the prior boot's readiness marker.

Undeclared/disabled images reject exec/copy before WebSocket upgrade and skip
agent shutdown/probes. The declaration is an image capability claim, not a
verified live guest handshake or a security credential. Runtime connectivity
and privileged provisioning still require the validation below.

## OS-specific operations

- **Shutdown:** root-only `/sbin/shutdown -h now`, not a signal to launchd/PID 1.
  Signal 0 (default) and SIGTERM mean orderly shutdown; other signals are rejected.
  Permission, cancellation, and command errors are reported. An RPC response is
  not proof the VMM has exited: the host must wait for teardown.
- **Network identity reconfiguration:** returns gRPC `Unimplemented` on Darwin.
  Current VZ NAT uses guest DHCP; no static-address, MAC-rekey, ingress, or network
  policy parity is claimed.
- **GPU status:** Linux NVIDIA initialization reporting is not macOS graphics
  readiness. A Darwin guest without that device reports the existing unknown
  state.

## Validation and remaining integration

In-process gRPC tests exercise exec stdout/stderr/exit/env, disconnect
cancellation, and file copy/stat round-trips. Darwin-specific tests exercise
shutdown policy without executing a real shutdown, explicit network rejection,
and transport deadline errors using a local socket pair. These do not prove a
live guest AF_VSOCK handshake for this executable.

## Isolated live validation (2026-10-10)

A manually provisioned root LaunchDaemon in the isolated QA guest survives normal
API cold boots. Authenticated normal API exec reports UID0 in both TTY modes;
private root-owned copy-to/from round trips and command/descendant timeout cleanup
pass repeated race-instrumented client tests. The readiness probe uses
`/usr/bin/true` on Darwin (`/bin/true` remains the Linux command); a live regression
first failed with the Linux path, then passed after this correction. A normal GET
persisted `GuestAgentReadyAt` while leaving `ProgramStartedAt` absent.

Darwin can disconnect vsock while shutdown is in progress, before the RPC reply
arrives. The host therefore retains the configured grace period on Darwin even
when that reply is unavailable, instead of Linux's short failure fallback. Success
still requires the owned VMM process to exit. A live normal API stop confirmed
shutdown RPC acceptance, VMM exit and closed storage without forced fallback after
the correction. HTTP stop200 or `Stopped` alone is not a build/export receipt.

This is manual provisioning of one QA instance, not automatic image installation,
public macOS builds, output publication or reusable-image sanitation proof. The
immutable imported base remains unchanged and does not acquire this capability.

Remaining gates:

- Reusable image provisioning and guest-agent version compatibility.
- Strict build/export stop receipts and API recovery with the root deployment;
  bounded readiness wait semantics.
- Broader backpressure/large-output validation of bounded non-TTY streaming,
  PTY/disconnect and descendant-process cleanup, transfer failure/size handling,
  and privilege/logging security review.
- Linux test execution on an appropriate runner, and independent authenticated
  review.
