# Build System TODOs

Outstanding issues and improvements for the build system.

---

## 🟡 Medium Priority

### 1. Enable cgroups for BuildKit Secrets

**Issue:** When `--secret` flags are passed to BuildKit, runc requires cgroup mounts that aren't present in the microVM.

**Error:** `runc run failed: no cgroup mount found in mountinfo`

**Status:** The secrets API flow works correctly (host → vsock → agent → BuildKit flags), but BuildKit execution fails due to missing cgroups.

**Workaround:** Builds without secrets work fine. The secrets code is ready once cgroups are enabled.

#### Root Cause

The VM init (`lib/system/init/mount.go`) mounts `/proc`, `/sys`, `/dev`, `/dev/pts`, `/dev/shm` but does NOT mount `/sys/fs/cgroup`. When BuildKit receives `--secret` flags, it uses runc which requires cgroups even for rootless execution.

#### Proposed Solutions

**Option A: Add cgroup mount to VM init (all VMs)**

File: `lib/system/init/mount.go`

```go
// In mountEssentials(), add:
if err := os.MkdirAll("/sys/fs/cgroup", 0755); err != nil {
    return fmt.Errorf("mkdir /sys/fs/cgroup: %w", err)
}
if err := syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
    log.Info("mount", "cgroup2 failed (non-fatal)")
}

// In bindMountsToNewRoot(), add to mounts slice:
{"/sys/fs/cgroup", newroot + "/sys/fs/cgroup"},
```

Pros:
- Enables cgroups for all VM workloads
- Happens early in boot before user processes
- Properly bind-mounts to new root

Cons:
- All VMs get cgroup access (larger attack surface, though mitigated by VM isolation)

**Option B: Add cgroup mount in builder-agent only**

File: `lib/builds/builder_agent/main.go`

```go
func mountCgroups() error {
    if err := os.MkdirAll("/sys/fs/cgroup", 0755); err != nil {
        return err
    }
    return syscall.Mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, "")
}
```

Pros:
- Only affects builder VMs
- Minimal scope

Cons:
- Late in boot (after chroot)
- May not work if /sys is read-only in newroot

#### Security Analysis

| Concern | Risk Level | Mitigation |
|---------|------------|------------|
| Container escape via cgroup | Very Low | VM hypervisor isolation + cgroup v2 (no release_agent) |
| Resource manipulation | Low | VM has hypervisor-level resource limits |
| Attack surface for user VMs | Medium | Consider making cgroups opt-in or read-only |

**Recommendation:** Option A with cgroup v2 is safe because:
1. VMs are already isolated by Cloud Hypervisor (hardware boundary)
2. Builder VMs are ephemeral (destroyed after each build)
3. Builder runs as unprivileged user (uid 1000)
4. Cgroup v2 has better security than v1 (no release_agent escape vector)

#### After Implementation

1. Rebuild init binary: `make init`
2. Rebuild initrd: `make initrd`
3. Test builds with secrets

---

## 🟢 Low Priority

### 2. Builder Image Tooling

**File:** `lib/builds/images/README.md`

**Suggestion:** Create a script or tooling for building and publishing new builder images.
