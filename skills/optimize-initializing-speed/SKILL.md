---
name: optimize-initializing-speed
description: Use when optimizing VM Initializing-to-Running latency while preserving functionality and low implementation complexity.
---

# Optimize Initializing Speed

## Goal
Minimize `Create/Start -> Running` latency without removing functionality. Base your decisions on what to optimize by using real measurements.

## Priority Levers
1. Keep `Running` gated only on `program-start` + `agent-ready` markers.
2. Replace readiness polling with event-driven signaling
3. Move heavy non-critical setup (kernel headers) off the critical path (ask permission from user if moving logic to async / could be still processing after Running is set).
4. Add fast-path checks (skip work when already installed/valid).
5. Parallelize independent init stages with simple barriers (no DAG engine). Avoid parallel tasks that are likely to conflict.

## Guardrails
- Keep guest-agent gate strict unless `skip_guest_agent=true`.
- Preserve lifecycle semantics and blocked/allowed operations in `Initializing`.

## Measurement Protocol
1. Measure baseline and candidate on the same host with the same 5-run harness.
2. Report per-run samples + median/mean/min/max.
3. Validate full regression suite before merge.

## Required Outputs
- Exact before/after latency numbers.
- Short breakdown of biggest contributors.
- Risk notes, if any
