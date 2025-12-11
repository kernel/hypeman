# Logger

Structured logging with per-subsystem log levels and OpenTelemetry trace context integration.

## Features

- Per-subsystem configurable log levels
- Automatic trace_id/span_id injection when OTel is active
- Context-based logger propagation
- JSON output format
- Per-instance log files via `InstanceLogHandler`

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `LOG_LEVEL` | Default log level | `info` |
| `LOG_LEVEL_<SUBSYSTEM>` | Per-subsystem level | inherits default |

Subsystems: `API`, `IMAGES`, `INSTANCES`, `NETWORK`, `VOLUMES`, `VMM`, `SYSTEM`, `EXEC`

Example:
```bash
LOG_LEVEL=info LOG_LEVEL_NETWORK=debug ./hypeman
```

## Usage

```go
// Create subsystem-specific logger
cfg := logger.NewConfig()
log := logger.NewSubsystemLogger(logger.SubsystemInstances, cfg)

// Add logger to context
ctx = logger.AddToContext(ctx, log)

// Retrieve from context
log = logger.FromContext(ctx)
log.InfoContext(ctx, "instance created", "instance_id", instanceID)
```

## Per-Instance Logging

The `InstanceLogHandler` automatically writes logs with an `"instance_id"` attribute to per-instance `hypeman.log` files. This provides an operations audit trail for each VM.

```go
// Wrap any handler with instance logging
handler := logger.NewInstanceLogHandler(baseHandler, func(id string) string {
    return paths.InstanceHypemanLog(id)
})

// Logs with "instance_id" attribute are automatically written to that instance's hypeman.log
log.InfoContext(ctx, "starting VM", "instance_id", instanceID)

// Related operations (e.g., ingress creation) can also include instance_id
// to appear in the instance's audit log
log.InfoContext(ctx, "ingress created", "ingress_id", ingressID, "instance_id", targetInstance)
```

## Output

When OTel tracing is active, logs include trace context:

```json
{
  "level": "INFO",
  "msg": "instance created",
  "subsystem": "INSTANCES",
  "trace_id": "abc123...",
  "span_id": "def456...",
  "instance_id": "instance-123"
}
```

