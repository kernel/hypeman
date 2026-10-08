# Experimental Darwin GuestService

The existing guest-agent executable now builds for macOS and serves the same
`guest.GuestService` gRPC contract as Linux on vsock port **2222**. It reuses
exec, copy-to/from-guest, and stat implementations; this is not the browser
prototype's custom HTTP protocol or its ports.

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
permissions, or browser readiness. Root and user desktop agents need a reviewed
handoff and explicit session selection before desktop execution is supported.
No desktop agent installer or public session-selector extension is introduced
by this initial patch.

The host API and hypervisor still enforce authorization. The vsock host-CID
check is transport admission, not a replacement for instance authority checks.
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
start marker. `Running` remains VMM-running for macOS; it does not certify system,
desktop or browser readiness. This readiness timestamp is a boot observation,
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

Remaining draft gates:

- Provision in a test guest and exercise real host GuestService connectivity.
- Live normal API exec/files, readiness and graceful stop/recovery validation;
  guest-agent version compatibility and bounded readiness wait semantics.
- Root/desktop session authorization and image provisioning.
- Broader backpressure/large-output validation of bounded non-TTY streaming,
  PTY/disconnect and descendant-process cleanup, transfer failure/size handling,
  and privilege/logging security review.
- Linux test execution on an appropriate runner, and independent authenticated
  review.

The live macOS benchmark guest and its prototype agent are unchanged by this
source patch. Native gRPC integration remains experimental until those gates
are satisfied.
