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
        "hostname": "api.example.com"
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
5. SIGHUP sent to Envoy to reload config

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
        v1.33/
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
| `ENVOY_LISTEN_PORT` | Port for ingress listeners | `80` |
| `ENVOY_ADMIN_ADDRESS` | Address for Envoy admin API | `127.0.0.1` |
| `ENVOY_ADMIN_PORT` | Port for Envoy admin API | `9901` |

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
1. Regenerate `config.yaml` from all ingresses
2. Send SIGHUP to Envoy process
3. Envoy hot-reloads configuration

### Shutdown
- Hypeman does not stop Envoy on exit (daemon persists)
- Envoy can be manually stopped via admin API or SIGTERM

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
- Multiple listener ports
- Health checks for backends
- Connection draining for graceful config updates

