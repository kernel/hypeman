# Guest Memory Reclaim

This feature reduces host RAM waste from guest VMs by combining three behaviors:

1. Lazy host allocation preservation:
The VM is configured with requested memory capacity, but host pages should only back guest pages as they are touched.

2. Guest-to-host reclaim:
When the guest frees memory, virtio balloon/reporting/hinting features let the VMM return those pages to the host.

3. Guest boot page-touch reduction:
The guest kernel page-init mode controls whether Linux eagerly touches pages:
- `performance` mode sets `init_on_alloc=0 init_on_free=0` for better density and lower memory churn.
- `hardened` mode sets `init_on_alloc=1 init_on_free=1` for stronger memory hygiene at some density/perf cost.

## Configuration

This feature is controlled by `hypervisor.memory` in server config and is default-off:

```yaml
hypervisor:
  memory:
    enabled: false
    kernel_page_init_mode: hardened
    reclaim_enabled: true
    vz_balloon_required: true
```

To enable reclaim behavior and density-oriented kernel args, set:

```yaml
hypervisor:
  memory:
    enabled: true
    kernel_page_init_mode: performance
```

## Runtime Flow

- Operator config (`hypervisor.memory`) is normalized into one policy.
- The instances layer applies policy generically:
  - merges kernel args with the selected page-init mode;
  - sets generic memory feature toggles in `hypervisor.VMConfig.GuestMemory`.
- Each hypervisor backend maps generic toggles to native mechanisms:
  - Cloud Hypervisor: `balloon` config with free page reporting and deflate-on-oom.
  - QEMU: `virtio-balloon-pci` device options.
  - Firecracker: `/balloon` API with free page hinting/reporting.
  - VZ: attach `VirtioTraditionalMemoryBalloon` device.

## Backend Behavior Matrix

| Hypervisor | Lazy allocation | Balloon | Free page reporting/hinting | Deflate on OOM |
|---|---|---|---|---|
| Cloud Hypervisor | Yes | Yes | Reporting | Yes |
| QEMU | Yes | Yes | Reporting (+ hinting when enabled) | Yes |
| Firecracker | Yes | Yes | Hinting + reporting | Yes |
| VZ | macOS-managed | Yes | Host-managed + guest cooperation | Host-managed |

## Failure Behavior

- If policy is disabled, memory features are not applied.
- If reclaim is disabled, balloon/reporting/hinting are not applied.
- For VZ, balloon attachment is attempted when enabled.
  - If `vz_balloon_required=true`, startup fails if balloon cannot be configured.
  - If `vz_balloon_required=false`, startup continues without balloon and logs a warning.

## Quick CLI Experiment

Use this A/B check to compare host memory footprint with policy enabled vs disabled:

```bash
# 1) Start API with config A (hypervisor.memory.enabled=true), then run:
ID=$(hypeman run --hypervisor qemu --network=false --memory 1GB \
  --entrypoint /bin/sh --entrypoint -c \
  --cmd 'sleep 5; dd if=/dev/zero of=/dev/shm/hype-mem bs=1M count=256; sleep 5; rm -f /dev/shm/hype-mem; sleep 90' \
  docker.io/library/alpine:latest | tail -n1)
PID=$(jq -r '.HypervisorPID' "<data_dir>/guests/$ID/metadata.json")
awk '/^Pss:/ {print $2 " kB"}' "/proc/$PID/smaps_rollup" # Linux (preferred)
awk '/^VmRSS:/ {print $2 " kB"}' "/proc/$PID/status"      # Linux fallback
ps -o rss= -p "$PID"                                  # macOS
hypeman rm --force "$ID"

# 2) Restart API with config B (hypervisor.memory.enabled=false) and run the same command.
# 3) Compare final/steady host memory between A and B.
```

In one startup-focused sample run, absolute host footprint stayed far below guest memory size (for example, ~4GB guest with low host PSS on Cloud Hypervisor/Firecracker), while QEMU showed a larger fixed process overhead.

Sample probe results (4GB idle guest, rounded MB):

| Hypervisor | Host RSS (MB) | Host PSS (MB) | Notes |
|---|---:|---:|---|
| Cloud Hypervisor (Linux) | ~345 | ~29 | Low actual host pressure when idle |
| Firecracker (Linux) | ~295 | ~27 | Low actual host pressure when idle |
| QEMU (Linux) | ~400 | ~116 | Higher fixed process overhead |
| VZ (macOS) | ~23 | N/A | RSS sampled with `ps` |

## Out of Scope

- No API surface changes.
- No scheduler/admission logic changes.
- No automatic background tuning loops outside hypervisor-supported reclaim mechanisms.
