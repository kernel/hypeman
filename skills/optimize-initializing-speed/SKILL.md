---
name: optimize-initializing-speed
description: Use when optimizing VM Initializing-to-Running latency while preserving functionality and low implementation complexity.
---

# Optimize Initializing Speed

## Goal
Minimize `Create/Start -> Running` latency without removing functionality.

## Priority Levers
1. Keep `Running` gated only on `program-start` + `agent-ready` markers.
2. Replace readiness polling with event-driven signaling (pipe FD via `ExtraFiles`).
3. Move heavy non-critical setup (kernel headers) off the critical path.
4. Add fast-path checks (skip work when already installed/valid).
5. Parallelize independent init stages with simple barriers (no DAG engine).

## Guardrails
- Do not gate `Running` on kernel headers readiness.
- Keep guest-agent gate strict unless `skip_guest_agent=true`.
- Preserve lifecycle semantics and blocked/allowed operations in `Initializing`.
- Prefer simple staged orchestration over framework complexity.

## Measurement Protocol
1. Measure baseline and candidate on the same host with the same 5-run harness.
2. Report per-run samples + median/mean/min/max.
3. Validate full regression suite (`make test-linux`) before merge.

## Required Outputs
- Exact before/after latency numbers.
- Short breakdown of biggest contributors.
- Risk notes and rollback-safe behavior.
