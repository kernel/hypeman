# Hypervisor Abstraction

Provides a common interface for VM management across different hypervisors.

## Purpose

Hypeman originally supported only Cloud Hypervisor. This abstraction layer allows supporting multiple hypervisors (e.g., QEMU) through a unified interface, enabling:

- **Hypervisor choice per instance** - Different instances can use different hypervisors
- **Feature parity where possible** - Common operations work the same way
- **Graceful degradation** - Features unsupported by a hypervisor can be detected and handled

## How It Works

The abstraction defines two key interfaces:

1. **Hypervisor** - VM lifecycle operations (create, boot, pause, resume, snapshot, restore, shutdown)
2. **ProcessManager** - Hypervisor process lifecycle (start binary, get binary path)

Each hypervisor implementation translates the generic configuration and operations to its native format. For example, Cloud Hypervisor uses an HTTP API over a Unix socket, while QEMU would use QMP.

Before using optional features, callers check capabilities:

```go
if hv.Capabilities().SupportsSnapshot {
    hv.Snapshot(ctx, path)
}
```

## Hypervisor Switching

Instances store their hypervisor type in metadata. An instance can switch hypervisors only when stopped (no running VM, no snapshot), since:

- Disk images are hypervisor-agnostic
- Snapshots are hypervisor-specific and cannot be restored by a different hypervisor
