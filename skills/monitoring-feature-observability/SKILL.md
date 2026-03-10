---
name: monitoring-feature-observability
description: Add or adjust monitoring for a Hypeman feature using repository standards for logs, traces, and metrics. Use when a user asks for instrumentation, observability reviews, telemetry consistency changes, metric design, or production-signal improvements.
---

# Monitoring Feature Observability

Your task is to add monitoring for a specific feature or to perform a specific monitoring-related ask from the user.

## Logging

- Logging uses structured slog JSON with per-subsystem levels (`LOG_LEVEL`, `LOG_LEVEL_<SUBSYSTEM>`). Logs are enriched with `subsystem`, and when trace context exists, `trace_id`/`span_id`: `lib/logger/logger.go`.
- During normal running of the system without requests or events being sent to the system, there should be minimal to no logging at the INFO level or greater. So ongoing maintenance items should not be logging at INFO or greater.
- During a request (for example, API call) or interrupt/event (for example, guest program stops), the normal case should have about one informative log at INFO level, usually just one log.
- Other useful but normal information within a single request/event should be at DEBUG level accordingly. Do not use TRACE level.
- Logs resulting from a single request or event should not provide much duplicated information.
- Logs with `instance_id` are also duplicated into per-instance `logs/hypeman.log`, and instance log APIs stream them, so be sure to set `instance_id` or other `resourcetype_id` accordingly.
- Use WARN and ERROR logs appropriately.
- Logs should be associated with traces.

## Tracing

- All API requests should support tracing.
- Tracing should span down as far as reasonable, ideally all the way down unless there is a good reason not to.
- For example, trace down into clients calling each hypervisor.
- Per-instance identifiers (for example, `instance_id`) are allowed in trace attributes when they materially improve debugging or correlation.
- Still avoid sensitive or unbounded attributes by default (for example, full guest paths, user identifiers, tokens, arbitrary payload fields).

## Metrics

- Metrics should be created in Prometheus/OpenMetrics format using normal best practices.
- Metrics are emitted via OTel instruments (counters/histograms/gauges) across subsystems (instances, images, network, and so on).
- Low-cardinality labels only (for example, no VM name, IP address, or ID labels).
- Per-VM metric labels are an explicit exception when operationally required (for example `instance_id`, `instance_name`) and should be guarded with budget/alerting.
- Confirm with the user before adding any new high-cardinality metric label.
- Use counters where advisable to avoid sampling errors in data.
- Usually include timing histogram metrics.
- Work with the user to agree on good application-level signals to monitor for a given feature, providing examples in terms of what it would look like on the `/metrics` endpoint.
- All features should have at least one good application-level metric.
- Confirm with the user before removing metrics.
- Do not create denormalized metrics (containing information that can be derived from other metrics).

Look in `DEVELOPMENT.md` (section: `Local OpenTelemetry (optional)`) for how to collect telemetry from a local server.
