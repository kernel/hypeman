# Guest Memory

Hypeman's guest-memory feature combines passive reclaim and active reclaim.

- Passive reclaim gives pages back to the host when the guest has already freed them.
- Active reclaim asks the guest to give memory back by inflating its virtio balloon target.
- Linux page-init tuning controls whether the guest eagerly scrubs pages on allocation/free.

The important distinction is that active ballooning is not `drop_caches`. Balloon inflation makes the guest kernel feel memory pressure, so the guest reclaims memory through its normal LRU and reclaim paths. That lets the guest keep hot working-set cache and evict colder pages first.

## What Happens At Runtime

When `hypervisor.memory.enabled=true`, Hypeman enables the guest-memory features each hypervisor supports:

- Cloud Hypervisor configures a balloon device with free-page reporting and deflate-on-oom.
- QEMU adds a virtio balloon device and enables free-page reporting when available.
- Firecracker configures ballooning with hinting/reporting and deflate-on-oom.
- VZ attaches a traditional memory balloon device through `vz-shim`.

When `kernel_page_init_mode=performance`, Hypeman also adds `init_on_alloc=0 init_on_free=0` to the guest kernel command line. That reduces unnecessary guest page touching during boot and steady-state reclaim. `hardened` keeps both flags enabled.

## Automatic Active Ballooning

Automatic ballooning is controlled by `hypervisor.memory.active_ballooning`.

```yaml
hypervisor:
  memory:
    enabled: true
    reclaim_enabled: true
    kernel_page_init_mode: performance
    active_ballooning:
      enabled: true
      poll_interval: 2s
      pressure_high_watermark_available_percent: 10
      pressure_low_watermark_available_percent: 15
      protected_floor_percent: 50
      protected_floor_min_bytes: 512MB
      min_adjustment_bytes: 64MB
      per_vm_max_step_bytes: 256MB
      per_vm_cooldown: 5s
```

The automatic loop is pressure-driven by default:

1. Hypeman samples host memory pressure.
2. If the host is under pressure, it computes a global reclaim target.
3. Eligible VMs are asked to give back memory proportionally to their reclaimable headroom.
4. Each hypervisor gets a new runtime balloon target.
5. When the host is healthy again, Hypeman gradually deflates balloons back toward full guest memory.

The controller uses hysteresis so it does not flap when available memory hovers near the threshold:

- `pressure_high_watermark_available_percent` enters pressure mode.
- `pressure_low_watermark_available_percent` exits pressure mode.

### Host Pressure Signals

Linux uses:

- `/proc/meminfo` `MemAvailable` as the primary available-memory signal
- `/proc/pressure/memory` PSI as a secondary stress signal

macOS uses:

- `vm_stat` free/speculative pages to estimate available memory
- `memory_pressure -Q` as a secondary stress signal

## Protected Floors And Allocation Rules

Active reclaim never shrinks a guest below its protected floor:

- `protected_floor_percent` reserves a percentage of assigned guest RAM
- `protected_floor_min_bytes` reserves an absolute minimum
- the larger of the two becomes the guest's floor

Example:

- a 4 GiB guest with `protected_floor_percent=50` has a 2 GiB floor
- if `protected_floor_min_bytes=512MB`, the effective floor is still 2 GiB
- Hypeman can reclaim at most 2 GiB from that guest

Reclaim is also rate-limited:

- `min_adjustment_bytes` skips tiny target changes
- `per_vm_max_step_bytes` caps how much one reconcile can change a guest
- `per_vm_cooldown` prevents frequent small oscillations

## Manual Reclaim API

Hypeman also exposes a proactive reclaim endpoint:

- `POST /resources/memory/reclaim`

Request fields:

- `reclaim_bytes`: required total reclaim target across eligible guests
- `hold_for`: optional duration, default `5m`, max `1h`
- `dry_run`: optional, computes the plan without applying it
- `reason`: optional operator note for logs/traces

Manual reclaim uses the same planner and protected floors as automatic reclaim. When `hold_for` is set, Hypeman keeps at least that much reclaim in place until the hold expires, even if host pressure clears sooner. Sending `reclaim_bytes=0` with `hold_for=0s` clears the hold and allows full deflation immediately.

By design, Hypeman does not reclaim memory without a reason. Automatic reclaim only happens under real host pressure. Proactive reclaim without host pressure is only done when an operator explicitly asks for it through the API.

## Observability

Active ballooning emits structured logs, metrics, and traces so operators can tell whether reclaim is healthy and effective.

Logs:

- manual reclaim requests log start, success, and failure
- pressure state transitions log the old and new state plus current host availability
- per-VM apply failures log the affected `instance_id`, hypervisor, and requested target
- automatic reconcile summaries log when pressure changes, reclaim is applied, or errors occur

Metrics:

- `hypeman_guestmemory_reconcile_total` and `hypeman_guestmemory_reconcile_duration_seconds`
- `hypeman_guestmemory_reclaim_actions_total`
- `hypeman_guestmemory_pressure_transitions_total`
- `hypeman_guestmemory_sampler_errors_total`
- `hypeman_guestmemory_reclaim_bytes`
- `hypeman_guestmemory_host_available_bytes`
- `hypeman_guestmemory_target_reclaim_bytes`
- `hypeman_guestmemory_applied_reclaim_bytes`
- `hypeman_guestmemory_manual_hold_active`
- `hypeman_guestmemory_eligible_vms_total`
- `hypeman_guestmemory_pressure_state`

Traces:

- manual API calls create a `guestmemory.manual_reclaim` span
- each reconcile creates a `guestmemory.reconcile` span
- child spans capture host pressure sampling, VM enumeration, and balloon target application

## Passive Reclaim vs Active Ballooning

Passive reclaim and active reclaim are complementary:

- free-page reporting/hinting handles "the guest freed this already"
- active ballooning handles "the host needs memory back now"

Both are useful. Passive reporting improves density opportunistically. Active ballooning gives Hypeman a control loop for pressure events and explicit operator requests.

## Hypervisor Expectations

Cloud Hypervisor:

- boot-time ballooning plus free-page reporting
- runtime target changes through `/vm.resize`

QEMU:

- virtio balloon device on the VM command line
- runtime target changes through QMP `balloon`

Firecracker:

- balloon config at boot with hinting/reporting
- runtime target changes through the balloon API
- if a custom or older binary lacks the runtime balloon endpoint, Hypeman skips active reclaim for that VM

VZ:

- traditional memory balloon device attached through `vz-shim`
- runtime target changes through `vz-shim` balloon endpoints

## Failure Behavior

- If `hypervisor.memory.enabled=false`, none of the guest-memory features are configured.
- If `reclaim_enabled=false`, passive reclaim and active ballooning are both disabled.
- If `active_ballooning.enabled=false`, the background pressure loop stays off and the manual reclaim endpoint returns a feature-disabled error.
- If a specific VM or hypervisor backend does not support runtime balloon control, Hypeman skips that VM and continues with the rest.
- `deflate_on_oom` stays enabled where supported so guests can recover memory quickly during real guest-side pressure.

## Manual Integration Tests

The guest-memory integration tests are manual by default and cover one test per hypervisor:

- Linux: `TestGuestMemoryPolicyCloudHypervisor`
- Linux: `TestGuestMemoryPolicyQEMU`
- Linux: `TestGuestMemoryPolicyFirecracker`
- macOS: `TestGuestMemoryPolicyVZ`

All of them live in the existing `lib/instances` guest-memory test files and are gated by:

```bash
HYPEMAN_RUN_GUESTMEMORY_TESTS=1
```

Run them with:

```bash
make test-guestmemory-linux
make test-guestmemory-vz
```

The tests verify:

- boot-time guest-memory configuration is present
- runtime balloon target starts at full assigned memory
- manual reclaim changes the target in the expected direction
- protected floors prevent over-reclaim
- clearing the manual hold deflates back to full guest memory
