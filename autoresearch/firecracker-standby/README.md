# Firecracker Standby Autoresearch

This harness adapts the `karpathy/autoresearch` pattern to Hypeman's Firecracker standby and restore path.

## Goal

Optimize how quickly a Firecracker VM can move:

- `Running -> Standby`
- `Standby -> Running`

The benchmark is intentionally narrow. It is not a VM create/start benchmark.

## Fixed Harness

These files are the fixed evaluator and setup surface:

- `autoresearch/firecracker-standby/prepare.go`
- `autoresearch/firecracker-standby/run.go`
- `autoresearch/firecracker-standby/bench/bench.go`

Once the baseline is locked, do not change these files during the optimization loop.

## Commands

```bash
# One-time setup for the benchmark workspace and prepared instance
go run ./autoresearch/firecracker-standby/prepare.go

# Fixed-budget benchmark run
go run ./autoresearch/firecracker-standby/run.go -budget 180s -status baseline -description "baseline"
```

## Artifact Paths

- Workspace root: `tmp/autoresearch-firecracker-standby/core`
- Manifest: `tmp/autoresearch-firecracker-standby/core/manifest.json`
- Last summary: `tmp/autoresearch-firecracker-standby/core/last-run.json`
- Results TSV: `tmp/autoresearch-firecracker-standby/core/results.tsv`
- Prepared Hypeman data dir: `/tmp/hypeman-fcsb-<hash>`

The Firecracker data dir is intentionally short because Unix socket paths must stay under `SUN_LEN`.

## Fixed Metric

Each benchmark run:

- reuses the prepared Firecracker instance
- runs repeated standby/restore cycles for a fixed wall-clock budget
- records `standby_ms`, `restore_api_ms`, and `restore_running_ms` per cycle
- waits for `StateRunning` after restore so the metric cannot be gamed by returning early

Primary score:

- `score_ms = standby_p50_ms + restore_running_p50_ms`

Keep criteria:

- `0` failed cycles
- the targeted smoke test still passes
- score improves by a meaningful buffer
- if two changes are inside the noise band, prefer the simpler diff

## Writable Surface

Primary optimization files:

- `lib/instances/standby.go`
- `lib/instances/restore.go`
- `lib/hypervisor/firecracker/process.go`
- `lib/hypervisor/firecracker/firecracker.go`
- `lib/hypervisor/firecracker/config.go`

Validation and harness-adjacent files:

- `lib/instances/firecracker_test.go`
- `lib/instances/snapshot_integration_scenario_test.go`
- `Makefile`

Read-only during the optimization loop:

- `autoresearch/firecracker-standby/prepare.go`
- `autoresearch/firecracker-standby/run.go`
- `autoresearch/firecracker-standby/bench/bench.go`
- `autoresearch/firecracker-standby/README.md`
- `autoresearch/firecracker-standby/program.md`

Out of scope unless explicitly approved:

- non-Firecracker hypervisors
- API or schema changes
- unrelated instance-manager refactors
- changing the workload, metric definition, or budget mid-loop

## Baseline Smoke Test

Use this before and after substantial changes:

```bash
make test TEST=TestFirecrackerStandbyAndRestore
```

The benchmark itself is the latency evaluator. The integration test is the correctness gate.
