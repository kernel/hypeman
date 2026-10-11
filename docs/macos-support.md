# macOS guest support

Single source for what macOS guests support. Per-PR docs link here instead of repeating
the list. Status is per operation on the stacked branches. "PR n" names the stack PR that
owns the behavior.

| Area | Status | Notes | Owner |
| --- | --- | --- | --- |
| Create, get, list, start, stop, delete | Supported | Start cold-boots the disk. CPU and memory default to the image; cold-create overrides are supported (at least 2 CPUs and 4 GiB in whole MiB, within VZ host limits). The API's Linux defaults (`size` 1GB, `vcpus` 2, `overlay_size` 10GB) do not apply: the request validator fills them in, so a macOS create ignores them unless the client sent the field. | PR 1 |
| Image import (offline bundle) | Supported | Local ready `darwin/arm64` image. | PR 1 |
| Image pull (OCI machine image) | Partial | Repository-scoped. No tag promotion. | PR 2 |
| Exec, file copy, stat | Agent-gated | Needs the shared guest agent declared by the image. | PR 3 |
| Logs | Partial | `hypeman` and `vmm` (VZ shim log) sources. No app log file for VZ guests. | PR 1 |
| Stats | Partial | CPU and memory from the VM process. Network counters are 0 under NAT. | PR 1 |
| Memory reclaim | Unsupported | Reported per guest as `unsupported`. No balloon change. | PR 1 |
| Guest port forwarding | Agent-gated | HTTP and WebSocket to a guest loopback port at `/instances/{id}/ports/{port}/*`. The image starts its own services. See [macos-guest-ports.md](macos-guest-ports.md). | PR 5 |
| Builds | Partial | Opt-in `macos_builds`. Restricted recipe. Linux Dockerfile builds unsupported. | PR 6 |
| Stopped snapshots, same-instance restore | Partial | Graceful stop required. Running restore has an open readiness issue. | PR 4 |
| Standby, memory snapshot, memory fork | Unsupported | Rejected with a stable error. | — |
| Stopped clones, fork from snapshot | Unsupported | Needs identity rebinding and guest rekeying. | PR 4 (later) |
| Update (PATCH), VM restart policy | Partial | Expiration/TTL and `restart_policy: always` or `never` are supported. Manual stop suppresses automatic restart. A shim that dies without removing its socket (for example, SIGKILL) reads as Stopped, so `always` restarts it. `on_failure` is rejected: no workload exit status is available. | PR 1 |
| Exec health checks | Agent-gated | Supported at create and PATCH; `type: none` disables them. HTTP/TCP checks remain unsupported. | PR 3 |
| Volumes, env and commands, credentials and auto-standby policies, passthrough, shaping | Unsupported | Rejected at create, start, or update. | — |
| Ingress | Unsupported | Refused in macOS-only mode and for macOS targets. | — |
| Snapshot schedules | Unsupported | Refused at admission. | — |

Limits that apply everywhere: preserved machine identity (one active instance per
identifier, no rekeying), no clean-base sanitation audit, and no external review verdict.
