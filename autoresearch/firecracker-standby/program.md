# Firecracker Standby Autoresearch

This is an autoresearch-style loop for improving Firecracker standby and restore latency in Hypeman.

## Goal

Make Firecracker instances transition faster between:

- `Running -> Standby`
- `Standby -> Running`

The benchmark target is the standby and restore path only. Do not optimize VM create or unrelated flows unless they directly affect standby or restore latency.

## Current Baseline

Current baseline on branch `autoresearch/firecracker-standby-mar13`:

- `standby_p50_ms`: `5032.0`
- `restore_running_p50_ms`: `114.5`
- `restore_running_p95_ms`: `133.6`
- `score_ms`: `5146.5`
- `cycles`: `35`

Observed hotspot:

- every standby cycle logs `hypervisor did not exit gracefully in time, force killing process`
- restore is already fast
- standby dominates the score

Strong first hypothesis:

- reduce or eliminate the Firecracker shutdown fallback path in standby before trying anything more ambitious

## Fixed Harness

These files are fixed and must not change during the experiment loop:

- `autoresearch/firecracker-standby/prepare.go`
- `autoresearch/firecracker-standby/run.go`
- `autoresearch/firecracker-standby/bench/bench.go`
- `autoresearch/firecracker-standby/README.md`
- `autoresearch/firecracker-standby/program.md`

Fixed commands:

```bash
go run ./autoresearch/firecracker-standby/prepare.go
go run ./autoresearch/firecracker-standby/run.go -budget 180s -status baseline -description "baseline"
```

The benchmark runner writes results to:

- `tmp/autoresearch-firecracker-standby/core/results.tsv`

Do not change the workload, metric definition, or benchmark budget.

## Writable Surface

Primary optimization files:

- `lib/instances/standby.go`
- `lib/instances/restore.go`
- `lib/hypervisor/firecracker/process.go`
- `lib/hypervisor/firecracker/firecracker.go`
- `lib/hypervisor/firecracker/config.go`

Validation-only files:

- `lib/instances/firecracker_test.go`
- `lib/instances/snapshot_integration_scenario_test.go`
- `Makefile`

Out of scope:

- non-Firecracker hypervisors
- API or schema changes
- unrelated instance manager refactors
- new dependencies

## Success Metric

Primary score:

- `score_ms = standby_p50_ms + restore_running_p50_ms`

Secondary metrics:

- `restore_api_p50_ms`
- `restore_running_p95_ms`
- failure count

Keep rule:

- targeted smoke test passes
- benchmark has `0` failed cycles
- score improves by at least `5%` or `100ms`
- if an improvement is smaller than that noise band, prefer the simpler change

## Required Validation

For every experiment:

```bash
make test TEST=TestFirecrackerStandbyAndRestore
go run ./autoresearch/firecracker-standby/run.go -budget 180s -status candidate -description "<short description>"
```

If a change crashes, regresses, or exceeds the fixed budget badly, discard it.

## Experiment Loop

Work only on the dedicated experiment branch.

1. Check `git status` and the latest row in `tmp/autoresearch-firecracker-standby/core/results.tsv`.
2. Choose one narrow hypothesis.
3. Edit the smallest possible set of in-scope files.
4. Commit the experiment with a short message describing the hypothesis.
5. Run the required validation commands.
6. Append the benchmark result to `results.tsv`.
7. If the score improved enough, keep the commit.
8. If the score is worse or inside the noise band without a simplicity win, discard the commit and return to the previous best commit.

Do one idea per commit. Do not batch unrelated ideas together.

## Bounded Autonomy

- Stay inside the writable surface.
- Do not modify the fixed harness.
- Do not install packages or change dependencies.
- Prefer direct simplifications, deletions, and small Firecracker-specific fast paths over adding background goroutines, caches, or broad abstractions.
- Do not rewrite large parts of the standby/restore flow unless small changes have clearly failed.
- If the same idea fails 2-3 times, abandon it and move to a new hypothesis.
- If you discover the benchmark itself is invalid, stop and ask for human input instead of changing it yourself.

## Good Hypotheses

- Firecracker-specific shutdown handling after snapshot completion
- reducing unnecessary waits in `shutdownHypervisor`
- avoiding reconnect or retry work that is not needed for Firecracker standby
- simplifying restore path work that does not affect correctness

## Bad Hypotheses

- optimizing image pulls, create time, or unrelated startup paths
- broad refactors for style only
- adding complexity for tiny wins
- changing the benchmark to make the score look better
