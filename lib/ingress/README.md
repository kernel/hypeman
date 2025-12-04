# Ingress Manager

Manages external traffic routing to VM instances using Envoy as a reverse proxy.

## Architecture

```
External Request                Envoy (daemon)               VM
    |                               |                         |
    | Host:api.example.com:80       |                         |
    +------------------------------>| config.yaml lookup      |
                                    | route -> my-api:8080    |
                                    +------------------------>|
                                         10.100.x.y:8080
```

## How It Works

### Envoy Daemon

- Envoy binary is embedded in hypeman (like Cloud Hypervisor)
- Extracted to `/var/lib/hypeman/system/binaries/envoy/{version}/{arch}/envoy` on first use
- Runs as a daemon process that survives hypeman restarts
- Listens on `0.0.0.0:80` (configurable via `ENVOY_LISTEN_ADDRESS` and `ENVOY_LISTEN_PORT`)
- Admin API on `127.0.0.1:9901` (configurable via `ENVOY_ADMIN_ADDRESS` and `ENVOY_ADMIN_PORT`)

### Ingress Resource

An Ingress is a configuration object that defines how external traffic should be routed:

```json
{
  "name": "my-api-ingress",
  "rules": [
    {
      "match": {
        "hostname": "api.example.com",
        "port": 80
      },
      "target": {
        "instance": "my-api",
        "port": 8080
      }
    }
  ]
}
```

### Configuration Flow

1. User creates an ingress via API
2. Manager validates the ingress (name, instance exists, hostname unique)
3. Ingress is persisted to `/var/lib/hypeman/ingresses/{id}.json`
4. Envoy config is regenerated from all ingresses
5. Envoy performs a hot restart to pick up the new config

### Hostname Routing

- Uses HTTP Host header matching
- One hostname per rule (exact match)
- Hostnames must be unique across all ingresses
- Default 404 response for unmatched hostnames

## Filesystem Layout

```
/var/lib/hypeman/
  system/
    binaries/
      envoy/
        v1.36/
          x86_64/envoy
          aarch64/envoy
  envoy/
    config.yaml      # Auto-generated Envoy config
    envoy.pid        # PID file for daemon discovery
    envoy.log        # Envoy access logs
    envoy-stdout.log # Envoy process output
  ingresses/
    {id}.json        # Ingress resource metadata
```

## API Endpoints

```
POST   /ingresses      - Create ingress
GET    /ingresses      - List ingresses  
GET    /ingresses/{id} - Get ingress by ID or name
DELETE /ingresses/{id} - Delete ingress
```

## Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVOY_LISTEN_ADDRESS` | Address for ingress listeners | `0.0.0.0` |
| `ENVOY_ADMIN_ADDRESS` | Address for Envoy admin API | `127.0.0.1` |
| `ENVOY_ADMIN_PORT` | Port for Envoy admin API | `9901` |
| `ENVOY_STOP_ON_SHUTDOWN` | Stop Envoy when hypeman shuts down | `false` |

**Note on Ports:** Each ingress rule can specify a `port` in the match criteria to listen on a specific host port. If not specified, defaults to port 80. Envoy dynamically creates listeners for each unique port across all ingresses.

### OpenTelemetry Integration

When OTEL is enabled in hypeman (`OTEL_ENABLED=true`), Envoy is automatically configured to push **operational metrics** to the OTEL collector. This provides infrastructure monitoring without exposing tenant request data.

**Configuration used:**
- `OTEL_ENDPOINT` - gRPC endpoint for the OTEL collector (e.g., `otel-collector:4317`)
- `OTEL_SERVICE_NAME` - Service name (Envoy uses `{service_name}-envoy`)

**Metrics exported include:**
- Connection metrics (active connections, connection rates, errors)
- Request rates and error counts (aggregate, not per-request)
- Upstream health (backend availability, retries)
- Listener and cluster statistics
- Memory and resource usage

**Note:** Per-request tracing is intentionally disabled to protect tenant privacy. Only aggregate operational metrics are exported.

## Security

- Admin API bound to localhost only by default
- Ingress validation ensures target instances exist
- Instance IP resolution happens at config generation time
- Envoy runs as the same user as hypeman (not root)

## Daemon Lifecycle

### Startup
1. Extract Envoy binary (if needed)
2. Check for existing running Envoy (via PID file or admin API)
3. If not running, start Envoy with generated config
4. Wait for admin API to become ready

### Config Updates
1. Regenerate config to a temporary file
2. Validate the config using `envoy --mode validate`
3. If valid, atomically move the temp file to `config.yaml`
4. Perform hot restart by starting a new Envoy process with an incremented restart epoch
5. New Envoy process coordinates with the old one to take over without dropping connections

**Note:** Config validation ensures that invalid configurations are never applied. If validation fails, the operation returns an internal server error (500) and the original config remains in place.

### Shutdown
- By default (`ENVOY_STOP_ON_SHUTDOWN=false`), Envoy continues running when hypeman exits
- Set `ENVOY_STOP_ON_SHUTDOWN=true` to stop Envoy with hypeman
- Envoy can be manually stopped via admin API (`/quitquitquit`) or SIGTERM

## Testing

```bash
# Run ingress tests
go test ./lib/ingress/...
```

Tests use:
- Mock instance resolver (no real VMs needed)
- Temporary directories for filesystem operations
- Non-privileged ports to avoid permission issues

## Future Improvements

- TLS termination with ACME/Let's Encrypt
- Path-based L7 routing
- Health checks for backends
- Connection draining for graceful config updates