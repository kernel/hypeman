# Hypervisor Abstraction

Provides a common interface for VM management across different hypervisors.

## Purpose

Hypeman originally supported only Cloud Hypervisor. This abstraction layer allows supporting multiple hypervisors through a unified interface, enabling:

- **Hypervisor choice per instance** - Different instances can use different hypervisors
- **Platform support** - Linux uses Cloud Hypervisor/QEMU, macOS uses Virtualization.framework
- **Feature parity where possible** - Common operations work the same way
- **Graceful degradation** - Features unsupported by a hypervisor can be detected and handled

## Implementations

| Hypervisor | Platform | Process Model | Control Interface |
|------------|----------|---------------|-------------------|
| Cloud Hypervisor | Linux | External process | HTTP API over Unix socket |
| QEMU | Linux | External process | QMP over Unix socket |
| vz | macOS | Subprocess (vz-shim) | HTTP API over Unix socket |

## How It Works

The abstraction defines two key interfaces:

1. **Hypervisor** - VM lifecycle operations (create, boot, pause, resume, snapshot, restore, shutdown)
2. **VMStarter** - VM startup and configuration (start binary, get binary path)

Each implementation translates generic configuration to its native format. Cloud Hypervisor and QEMU run as external processes with socket-based control. The vz implementation runs VMs as separate vz-shim subprocesses using Apple's Virtualization.framework.

Before using optional features, callers check capabilities:

```go
if hv.Capabilities().SupportsSnapshot {
    hv.Snapshot(ctx, path)
}
```

## Platform Differences

### Linux (Cloud Hypervisor, QEMU)
- VMs run as separate processes with PIDs
- State persists across hypeman restarts (reconnect via socket)
- TAP devices and Linux bridges for networking

### macOS (vz)
- VMs run as separate vz-shim subprocesses (detached process group)
- State persists across hypeman restarts (reconnect via socket)
- NAT networking via Virtualization.framework
- Requires code signing with virtualization entitlement

## Hypervisor Switching

Instances store their hypervisor type in metadata. An instance can switch hypervisors only when stopped (no running VM, no snapshot), since:

- Disk images are hypervisor-agnostic
- Snapshots are hypervisor-specific and cannot be restored by a different hypervisor
