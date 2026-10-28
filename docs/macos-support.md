# macOS guest support

Single source for what macOS guests support. Per-PR docs link here instead of repeating
the list. Status is per operation on the stacked branches. "PR n" names the stack PR that
owns the behavior.

| Area | Status | Notes | Owner |
| --- | --- | --- | --- |
| Create, get, list, start, stop, delete | Supported | Start cold-boots the disk. CPU and memory default to the image; cold-create overrides are supported (at least 2 CPUs and 4 GiB in whole MiB, within VZ host limits). The API's Linux defaults (`size` 1GB, `vcpus` 2, `overlay_size` 10GB) do not apply: the request validator fills them in, so a macOS create ignores them unless the client sent the field. To tell the two apart, a macOS host refuses create bodies over 1 MiB with 413. | PR 1 |
| Image import (offline bundle) | Supported | Local ready `darwin/arm64` image. | PR 1 |
| Image pull (OCI machine image) | Partial | An instance create can pull an uncached image when `platform: darwin/arm64` is selected; pull readiness is asynchronous and the create may need retrying. Local cross-repository tags preserve the complete bundle; normal registry pushes preserve the OCI digest. | PR 2, PR 6 |
| Exec, file copy, stat | Agent-gated | Needs the shared guest agent declared by the image. | PR 3 |
| Logs | Partial | `hypeman` and `vmm` (VZ shim log) sources. No app log file for VZ guests. | PR 1 |
| Stats | Partial | CPU and memory from the VM process. Network counters are 0 under NAT. | PR 1 |
| Memory reclaim | Unsupported | Reported per guest as `unsupported`. No balloon change. | PR 1 |
| Guest port forwarding | Agent-gated | HTTP and WebSocket to a guest loopback port at `/instances/{id}/ports/{port}/*`. The image starts its own services. See [macos-guest-ports.md](macos-guest-ports.md). | PR 5 |
| Builds | Partial | Opt-in `macos_builds`. Restricted recipe. Linux Dockerfile builds unsupported. | PR 6 |
| Stopped snapshots, same-instance restore | Experimental | Graceful stop required. Stopped and Running targets; restored metadata and storage are staged together. | PR 4 |
| Standby and restore (same instance) | Experimental | Saves memory, restores the same instance. Restore can fail (about 2% observed, cause unknown); a failed restore leaves the instance in Standby. Recover by deleting, as on Linux. Standby auto-policies rejected: no host-visible connection tracking on NAT. | PR 4 |
| Same-instance memory snapshots | Experimental | Running/Standby capture, with exact VZ configuration validation and an uncompressed disk/aux/memory bundle. Same-instance restore only; new path needs native QA. | PR 4 |
| Independent warm memory forks | Unsupported | VZ restores saved state only into an identical configuration (same machine identifier and MAC), and Apple documents concurrent VMs sharing an identifier as undefined. Repeated same-instance restore is the warm start. | — |
| Stopped clones (fork from stopped) | Experimental | Rebinds machine identifier and MAC; rekeys SSH host keys on first start. Hostname and credentials not yet checked. | PR 4 |
| Fork from stopped snapshot | Experimental | Graceful receipt and enabled agent required. Fresh machine identifier/MAC; SSH rekey on first cold boot. Stopped and Running targets supported. | PR 4 |
| Update (PATCH), VM restart policy | Partial | Expiration/TTL and `restart_policy: always` or `never` are supported. Manual stop suppresses automatic restart. A shim that dies without removing its socket (for example, SIGKILL) reads as Stopped, so `always` restarts it. `on_failure` is rejected: no workload exit status is available. | PR 1 |
| Exec health checks | Agent-gated | Supported at create and PATCH; `type: none` disables them. HTTP/TCP checks remain unsupported. | PR 3 |
| Volumes, env and commands, credentials and auto-standby policies, passthrough, shaping | Unsupported | Rejected at create, start, or update. | — |
| Ingress | Experimental | Routes to the guest's hypeman-assigned address on its own vmnet network, as on Linux, including in macOS-only mode. Caddy is not embedded on macOS (`make build-caddy-darwin`). New path needs native QA. | PR 5 |
| Snapshot schedules | Experimental | State-based stopped or memory capture, existing cadence/retention, host save/restore gate and live configuration probe. New path needs native QA. | PR 4 |

Limits that apply everywhere: one active VM per machine identifier, no clean-base
sanitation audit, and no external review verdict. Same-instance memory restore
preserves identity; stopped clones get fresh machine identifiers and SSH host keys.
