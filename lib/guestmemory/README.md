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

## Out of Scope

- No API surface changes.
- No scheduler/admission logic changes.
- No automatic background tuning loops outside hypervisor-supported reclaim mechanisms.
