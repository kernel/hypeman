# OpenTelemetry

Provides OpenTelemetry initialization and metric definitions for Hypeman.

## Features

- OTLP export for traces, metrics, and logs (gRPC)
- Runtime metrics (Go GC, goroutines, memory)
- Application-specific metrics per subsystem
- Log bridging from slog to OTel (viewable in Grafana/Loki)
- Graceful degradation (failures don't crash the app)

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENV` | Deployment environment (`deployment.environment` attribute) | `unset` |
| `OTEL_ENABLED` | Enable OpenTelemetry | `false` |
| `OTEL_ENDPOINT` | OTLP endpoint (gRPC) | `127.0.0.1:4317` |
| `OTEL_SERVICE_NAME` | Service name | `hypeman` |
| `OTEL_SERVICE_INSTANCE_ID` | Instance ID (`service.instance.id` attribute) | hostname |
| `OTEL_INSECURE` | Disable TLS for OTLP | `true` |

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

## Usage

```go
provider, shutdown, err := otel.Init(ctx, otel.Config{
    Enabled:     true,
    Endpoint:    "localhost:4317",
    ServiceName: "hypeman",
})
defer shutdown(ctx)

meter := provider.Meter       // Use for creating metrics
tracer := provider.Tracer     // Use for creating traces
logHandler := provider.LogHandler // Use with slog for logs to OTel
```

## Logs

Logs are exported via the OTel log bridge (`otelslog`). When OTel is enabled, all slog logs are sent to Loki (via OTLP) and include:
- `subsystem` attribute (API, IMAGES, INSTANCES, etc.)
- `trace_id` and `span_id` when available
- Service attributes (name, instance, environment)

