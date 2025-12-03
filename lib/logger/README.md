# Logger

Structured logging with per-subsystem log levels and OpenTelemetry trace context integration.

## Features

- Per-subsystem configurable log levels
- Automatic trace_id/span_id injection when OTel is active
- Context-based logger propagation
- JSON output format

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
log.InfoContext(ctx, "instance created", "id", instanceID)
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
  "id": "instance-123"
}
```

