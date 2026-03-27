# OpenTelemetry

Provides OpenTelemetry initialization and metric definitions for Hypeman.

## Features

- Always-on Prometheus pull metrics via `/metrics`
- Optional OTLP push export for traces, metrics, and logs (gRPC)
- Runtime metrics (Go GC, goroutines, memory)
- Application-specific metrics per subsystem
- Log bridging from slog to OTel (viewable in Grafana/Loki)
- Single metric instrumentation pipeline shared by pull and push paths

## Dual Export Model

Hypeman always exposes metrics on `/metrics` (pull), by default on `127.0.0.1:9464`.  
If `otel.enabled=true`, it also pushes the same metric stream on a schedule to OTLP.

This keeps pull and push views aligned because both are sourced from the same OTel meter provider/instruments.

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENV` | Deployment environment (`deployment.environment` attribute) | `unset` |
| `OTEL_ENABLED` | Enable OpenTelemetry | `false` |
| `OTEL_ENDPOINT` | OTLP endpoint (gRPC) | `127.0.0.1:4317` |
| `OTEL_SERVICE_NAME` | Service name | `hypeman` |
| `OTEL_SERVICE_INSTANCE_ID` | Instance ID (`service.instance.id` attribute) | hostname |
| `OTEL_INSECURE` | Disable TLS for OTLP | `true` |
| `OTEL__METRIC_EXPORT_INTERVAL` | OTLP metric push interval (when enabled) | `60s` |
| `OTEL__SUCCESSFUL_GET_SAMPLE_RATIO` | Sample rate for successful root HTTP `GET` traces | `0.1` |
| `METRICS__LISTEN_ADDRESS` | Bind address for `/metrics` listener | `127.0.0.1` |
| `METRICS__PORT` | Port for `/metrics` listener | `9464` |
| `METRICS__VM_LABEL_BUDGET` | Warning threshold for observed per-VM labeled VM metrics | `200` |

## Metrics

### System
| Metric | Type | Description |
|--------|------|-------------|
| `hypeman_uptime_seconds` | gauge | Process uptime |
| `hypeman_info` | gauge | Build info (version, go_version labels) |

### HTTP
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_http_requests_total` | counter | method, path, status | Total HTTP requests |
| `hypeman_http_request_duration_seconds` | histogram | method, path, status | Request latency |

### Images
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_images_build_queue_length` | gauge | | Current build queue size |
| `hypeman_images_build_duration_seconds` | histogram | status | Image build time |
| `hypeman_images_total` | gauge | status | Cached images count |
| `hypeman_images_pulls_total` | counter | status | Registry pulls |
| `hypeman_image_retention_pending_images` | gauge | state | Images currently tracked by retention state |
| `hypeman_image_retention_stale_references_total` | counter | | Stale image references skipped during retention sweeps |
| `hypeman_image_retention_sweeps_total` | counter | status | Retention sweep runs |
| `hypeman_image_retention_sweep_duration_seconds` | histogram | status | Retention sweep latency |
| `hypeman_image_retention_deletes_total` | counter | status | Retention delete attempts |

### Instances
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_instances_total` | gauge | state | Instances by state |
| `hypeman_instances_create_duration_seconds` | histogram | status | Create time |
| `hypeman_instances_time_to_running_seconds` | histogram | hypervisor | Time from boot start until an instance reaches Running |
| `hypeman_instances_restore_duration_seconds` | histogram | status, hypervisor, algorithm, level | Restore time |
| `hypeman_instances_standby_duration_seconds` | histogram | status, hypervisor, algorithm, level | Standby time |
| `hypeman_instances_state_transitions_total` | counter | from, to | State transitions |

### Network
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_network_allocations_total` | gauge | | Active IP allocations |
| `hypeman_network_tap_operations_total` | counter | operation | TAP create/delete ops |

### Resources
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_resources_capacity` | gauge | resource | Raw host capacity by resource type |
| `hypeman_resources_effective_limit` | gauge | resource | Effective allocatable limit after oversubscription |
| `hypeman_resources_allocated` | gauge | resource | Current allocation by resource type |
| `hypeman_resources_oversub_ratio` | gauge | resource | Oversubscription ratio by resource type |
| `hypeman_resources_disk_breakdown_bytes` | gauge | component | Allocation/provisioned disk breakdown by component |
| `hypeman_disk_utilization_bytes` | gauge | component | Actual filesystem bytes allocated by disk component |
| `hypeman_resources_image_storage_bytes` | gauge | kind | Current and maximum image storage bytes |
| `hypeman_resources_gpu_slots` | gauge | kind | Total and used GPU slots |
| `hypeman_resources_gpu_profile_slots` | gauge | profile, kind | Available GPU slots by profile |

### Volumes
| Metric | Type | Description |
|--------|------|-------------|
| `hypeman_volumes_total` | gauge | Volume count |
| `hypeman_volumes_allocated_bytes` | gauge | Total provisioned size |
| `hypeman_volumes_used_bytes` | gauge | Actual disk space consumed |
| `hypeman_volumes_create_duration_seconds` | histogram | Creation time |

### VMM
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_vmm_api_duration_seconds` | histogram | operation, status | CH API latency |
| `hypeman_vmm_api_errors_total` | counter | operation | CH API errors |

### Guest Memory
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_guestmemory_reconcile_total` | counter | trigger, status | Active ballooning reconcile cycles |
| `hypeman_guestmemory_reconcile_duration_seconds` | histogram | trigger, status | Reconcile latency |
| `hypeman_guestmemory_reclaim_actions_total` | counter | trigger, status, hypervisor | Per-VM reclaim action outcomes |
| `hypeman_guestmemory_pressure_transitions_total` | counter | from, to | Host pressure state transitions |
| `hypeman_guestmemory_sampler_errors_total` | counter | sampler | Host pressure sampling errors |
| `hypeman_guestmemory_reclaim_bytes` | histogram | trigger, kind | Reclaim byte targets and outcomes |
| `hypeman_guestmemory_host_available_bytes` | gauge | | Last observed host available memory |
| `hypeman_guestmemory_target_reclaim_bytes` | gauge | source | Current reclaim target (auto, manual, effective) |
| `hypeman_guestmemory_applied_reclaim_bytes` | gauge | | Current applied reclaim across eligible VMs |
| `hypeman_guestmemory_manual_hold_active` | gauge | | Whether a manual reclaim hold is active |
| `hypeman_guestmemory_eligible_vms_total` | gauge | | Eligible VM count seen by the controller |
| `hypeman_guestmemory_pressure_state` | gauge | | Current host pressure state (0 healthy, 1 pressure) |

### Exec
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_exec_sessions_total` | counter | status, exit_code | Exec sessions |
| `hypeman_exec_duration_seconds` | histogram | status | Command duration |
| `hypeman_exec_bytes_sent_total` | counter | | Bytes to guest (stdin) |
| `hypeman_exec_bytes_received_total` | counter | | Bytes from guest (stdout+stderr) |

### VM Metrics Guardrails
| Metric | Type | Description |
|--------|------|-------------|
| `hypeman_vm_metrics_instances_observed` | gauge | Current number of VM instances represented by per-VM labeled metrics |
| `hypeman_vm_metrics_label_budget_exceeded_total` | counter | Count of transitions into over-budget VM metric label cardinality |

## Usage

```go
provider, shutdown, err := otel.Init(ctx, otel.Config{
    Enabled:              true,
    Endpoint:             "localhost:4317",
    ServiceName:          "hypeman",
    MetricExportInterval: "60s",
})
defer shutdown(ctx)

meter := provider.Meter       // Use for creating metrics
tracer := provider.Tracer     // Use for creating traces
logHandler := provider.LogHandler // Use with slog for logs to OTel
metricsHandler := provider.MetricsHandler // Attach to GET /metrics
```

## Logs

Logs are exported via the OTel log bridge (`otelslog`). When OTel is enabled, all slog logs are sent to Loki (via OTLP) and include:
- `subsystem` attribute (API, IMAGES, INSTANCES, etc.)
- `trace_id` and `span_id` when available
- Service attributes (name, instance, environment)
