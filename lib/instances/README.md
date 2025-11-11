# Instance Manager

Manages VM instance lifecycle using Cloud Hypervisor.

## Design Decisions

### Why State Machine? (state.go)

**What:** Single-hop state transitions matching Cloud Hypervisor's actual states

**Why:**
- Validates transitions before execution (prevents invalid operations)
- Manager orchestrates multi-hop flows (e.g., Running → Paused → Standby)
- Clear separation: state machine = rules, manager = orchestration

**States:**
- `Stopped` - No VMM, no snapshot
- `Created` - VMM created but not booted (CH native)
- `Running` - VM actively running (CH native)
- `Paused` - VM paused (CH native)
- `Shutdown` - VM shutdown, VMM exists (CH native)
- `Standby` - No VMM, snapshot exists (can restore)

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
      ch.sock                   # Cloud Hypervisor API socket
      ch-stdout.log             # CH process output
      logs/
        console.log             # Serial console (VM output)
      snapshots/
        snapshot-latest/        # Snapshot directory
          vm.json               # VM configuration
          memory-ranges         # Memory state
          memory-ranges.lz4     # Compressed (optional)
```

**Benefits:**
- Content-addressable IDs (ULID = time-ordered)
- Self-contained: all instance data in one directory
- Easy cleanup: delete directory = full cleanup
- Sparse overlays: only store diffs from base image

## Multi-Hop Orchestrations (manager.go)

Manager orchestrates multiple single-hop state transitions:

**CreateInstance:**
```
Stopped → Created → Running
1. Start VMM process
2. Create VM config
3. Boot VM
4. Expand memory (if hotplug configured)
```

**StandbyInstance:**
```
Running → Paused → Standby
1. Reduce memory (virtio-mem balloon)
2. Pause VM
3. Create snapshot
4. Compress snapshot (LZ4, optional)
5. Stop VMM
```

**RestoreInstance:**
```
Standby → Paused → Running
1. Decompress snapshot (if compressed)
2. Start VMM
3. Restore from snapshot
4. Resume VM
```

**DeleteInstance:**
```
Any State → Stopped
1. Stop VMM (if running)
2. Delete all instance data
```

## Snapshot Optimization (standby.go, restore.go)

**Reduce snapshot size:**
- Memory balloon: Deflate to base size before snapshot
- LZ4 compression: Fast compression of memory-ranges (~2x ratio)
- Sparse overlays: Only store diffs

**Fast restore:**
- Decompress to memory (tmpfs) during restore
- Don't prefault pages (lazy loading)
- Parallel with TAP device setup

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
- `lib/vmm` - Cloud Hypervisor client for VM operations
- System tools: `mkfs.erofs`, `lz4`, `cpio`, `gzip`

