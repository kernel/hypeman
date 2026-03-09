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

### Instances
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_instances_total` | gauge | state | Instances by state |
| `hypeman_instances_create_duration_seconds` | histogram | status | Create time |
| `hypeman_instances_restore_duration_seconds` | histogram | status | Restore time |
| `hypeman_instances_standby_duration_seconds` | histogram | status | Standby time |
| `hypeman_instances_state_transitions_total` | counter | from, to | State transitions |

### Network
| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `hypeman_network_allocations_total` | gauge | | Active IP allocations |
| `hypeman_network_tap_operations_total` | counter | operation | TAP create/delete ops |

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
