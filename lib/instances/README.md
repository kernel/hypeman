# Instance Manager

Manages VM instance lifecycle across multiple hypervisors (Cloud Hypervisor, QEMU on Linux; vz on macOS).

## Design Decisions

### Why State Machine? (state.go)

**What:** Single-hop state transitions matching hypervisor states

**Why:**
- Validates transitions before execution (prevents invalid operations)
- Manager orchestrates multi-hop flows (e.g., Running → Paused → Standby)
- Clear separation: state machine = rules, manager = orchestration

**States:**
- `Stopped` - No VMM, no snapshot
- `Created` - VMM created but not booted (CH native)
- `Initializing` - VM is running while guest init is still in progress
- `Running` - Guest program start boundary reached and guest-agent readiness observed (unless `skip_guest_agent=true`)
- `Paused` - VM paused (CH native)
- `Shutdown` - VM shutdown, VMM exists (CH native)
- `Standby` - No VMM, snapshot exists (can restore)

### Windows launch defaults

Windows machine images boot through UEFI with Secure Boot and TPM 2.0. The default 8 GiB memory and 4 vCPUs provide headroom for Windows 11 startup and the guest service; the lower admission limits of 4 GiB and 2 vCPUs permit explicitly sized, constrained workloads without making that minimum the default.

The launchable Windows image already defines its virtual disk size, so instance creation clones that size exactly. Windows disk, partition, and filesystem growth are not implemented, and an `overlay_size` that differs from the image is rejected rather than silently presenting inconsistent capacity.

A Windows VM remains `Initializing` until its guest agent answers over VioSock. This avoids treating firmware completion as application readiness.

### Windows networking

Windows uses the same host-side TAP allocation as Linux. Once the guest agent is reachable, the manager sends the complete allocation through the typed `ReconfigureNetwork` RPC. The agent selects the virtio-net adapter by MAC address, replaces stale IPv4 addresses and default routes, and applies DNS through native Windows APIs. It never invokes the Linux shell-command fallback.

Create treats network configuration as part of readiness and tears down a VM if it fails. Start reapplies the current allocation because a stopped instance may receive a different address or MAC before its next boot.

### Why Config Disk? (configdisk.go)

**What:** Read-only erofs disk with instance configuration

**Why:**
- Zero modifications to OCI images (images used as-is)
- Config injected at boot time (not baked into image)
- Efficient (compressed erofs, ~few KB)
- Contains: entrypoint, cmd, env vars, workdir

## Filesystem Layout (storage.go)

```
/var/lib/hypeman/
  guests/
    {instance-id}/              # ULID-based ID
      metadata.json             # State, versions, timestamps
      overlay.raw               # 50GB sparse writable overlay
      config.erofs              # Compressed config disk
      ch.sock                   # Hypervisor API socket (abbreviated for SUN_LEN limit)
      logs/
        app.log                 # Guest application log (serial console output)
        vmm.log                 # Hypervisor log (stdout+stderr)
        hypeman.log             # Hypeman operations log
      snapshots/
        snapshot-latest/        # Snapshot directory
          config.json           # VM configuration
          memory-ranges         # Memory state
```

`metadata.json` also carries controller-owned auto-standby runtime timestamps when that feature is enabled, so idle countdown state can survive Hypeman restarts.

**Benefits:**
- Content-addressable IDs (ULID = time-ordered)
- Self-contained: all instance data in one directory
- Easy cleanup: delete directory = full cleanup
- Sparse overlays: only store diffs from base image

## Multi-Hop Orchestrations (manager.go)

Manager orchestrates multiple single-hop state transitions:

**CreateInstance:**
```
Stopped → Created → Initializing → Running
1. Start VMM process
2. Create VM config
3. Boot VM
4. Wait for guest-agent readiness gate (event-driven, exec mode, unless skipped)
5. Guest program start marker observed
6. Kernel headers setup continues asynchronously (does not gate `Running`)
7. Expand memory (if hotplug configured)
```

**StandbyInstance:**
```
Running → Paused → Standby
1. Reduce memory (virtio-mem hotplug)
2. Pause VM
3. Create snapshot
4. Stop VMM
```

**RestoreInstance:**
```
Standby → Paused → Running
1. Start VMM
2. Restore from snapshot
3. Resume VM
```

**DeleteInstance:**
```
Any State → Stopped
1. Stop VMM (if running)
2. Delete all instance data
```

## Snapshot Optimization (standby.go, restore.go)

**Reduce snapshot size:**
- Memory hotplug: Reduce to base size before snapshot (virtio-mem)
- Sparse overlays: Only store diffs from base image

**Fast restore:**
- Don't prefault pages (lazy loading)
- Parallel with TAP device setup

## Scheduled Snapshot Behavior

- Schedules are configured per instance and persisted in the server data store (outside snapshot payloads).
- A background scheduler evaluates due schedules every minute.
- Each due run chooses snapshot behavior from current source state:
  - `Running`/`Standby` sources use `Standby` snapshots.
  - `Stopped` sources use `Stopped` snapshots.
- `Standby` runs from `Running` sources perform a brief pause/resume cycle during capture.
- The minimum interval is `1m`, but larger intervals are recommended for heavier or latency-sensitive workloads because running captures pause/resume the guest.
- Scheduled snapshot `name_prefix` is optional and capped at 47 chars so generated names stay within the 63-char snapshot name limit.
- New schedules establish cadence at `now + interval + deterministic jitter` derived from the instance ID.
- Updating only retention, metadata, or `name_prefix` preserves `next_run_at`; changing `interval` establishes a new cadence.
- Schedule runs advance to the next future interval (no backfill flood after downtime).
- Each schedule stores operational status:
  - `next_run_at`
  - `last_run_at`
  - `last_snapshot_id`
  - `last_error`
- Retention cleanup runs after successful scheduled snapshot creation and only affects scheduled snapshots for that instance.
- If an instance is deleted, its schedule is retained so retention can continue cleaning existing scheduled snapshots.
- Once the deleted instance has no scheduled snapshots left, the scheduler removes that schedule automatically.

## WaitForState (wait.go, lifecycle_events.go)

Waits for an instance to reach a target state using the shared lifecycle event bus with a polling fallback. State-changing manager methods publish lifecycle events after successful mutations, and `WaitForState` filters them by `instance_id`. A 5s polling fallback guards against missed or dropped events. Returns early on terminal (`Stopped`, `Standby`, `Shutdown`) or error (`Unknown`) states. Used by `GET /instances/{id}/wait`.

## Reference Handling

Instances use OCI image references directly:
```go
req := CreateInstanceRequest{
    Image: "docker.io/library/alpine:latest",  // OCI reference
}
// Validates image exists and is ready via image manager
```

## Testing

Tests focus on testable components:
```bash
# State machine (pure logic, no VM needed)
TestStateTransitions - validates all transition rules

# Storage operations (filesystem only, no VM needed)
TestStorageOperations - metadata persistence, directory cleanup

# Full integration (requires kernel/initrd)
# Skipped by default, needs system files from system manager
```

## Dependencies

- `lib/images` - Image manager for OCI image validation
- `lib/system` - System manager for kernel/initrd files
- `lib/hypervisor` - Hypervisor abstraction for VM operations
- System tools: `mkfs.erofs`, `cpio`, `gzip` (Linux); `mkfs.ext4` (macOS)
