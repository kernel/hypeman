# Ingress Manager

Manages external traffic routing to VM instances using Caddy as a reverse proxy with automatic TLS via ACME.

## Architecture

```
External Request                Caddy (daemon)               VM
    |                               |                         |
    | Host:api.example.com:443      |                         |
    +------------------------------>| config.json lookup      |
                                    | route -> my-api:8080    |
                                    +------------------------>|
                                         10.100.x.y:8080
```

## How It Works

### Caddy Daemon

- Caddy binary is embedded in hypeman (like Cloud Hypervisor)
- Extracted to `/var/lib/hypeman/system/binaries/caddy/{version}/{arch}/caddy` on first use
- Runs as a daemon process that survives hypeman restarts
- Listens on configured ports (default: 80, 443)
- Admin API on `127.0.0.1:2019` (configurable via `CADDY_ADMIN_ADDRESS` and `CADDY_ADMIN_PORT`)

### Ingress Resource

An Ingress is a configuration object that defines how external traffic should be routed:

```json
{
  "name": "my-api-ingress",
  "rules": [
    {
      "match": {
        "hostname": "api.example.com",
        "port": 443
      },
      "target": {
        "instance": "my-api",
        "port": 8080
      },
      "tls": true,
      "redirect_http": true
    }
  ]
}
```

### Configuration Flow

1. User creates an ingress via API
2. Manager validates the ingress (name, instance exists, hostname unique)
3. Generates Caddy JSON config from all ingresses
4. Validates config via Caddy's admin API
5. If valid, persists ingress to `/var/lib/hypeman/ingresses/{id}.json`
6. Applies config via Caddy's admin API (live reload, no restart needed)

### TLS / HTTPS

When `tls: true` is set on a rule:
- Caddy automatically issues a certificate via ACME (Let's Encrypt)
- DNS-01 challenge is used (requires DNS provider configuration)
- Certificates are stored in `/var/lib/hypeman/caddy/data/`
- Automatic renewal ~30 days before expiry

When `redirect_http: true` is also set:
- An automatic HTTP → HTTPS redirect is created for the hostname

### Hostname Routing

- Uses HTTP Host header matching (HTTP) or SNI (HTTPS)
- One hostname per rule (exact match)
- Hostnames must be unique across all ingresses
- Default 404 response for unmatched hostnames

## Filesystem Layout

```
/var/lib/hypeman/
  system/
    binaries/
      caddy/
        v2.10.2/
          x86_64/caddy
          aarch64/caddy
  caddy/
    config.json    # Caddy configuration (applied via admin API)
    caddy.pid      # PID file for daemon discovery
    caddy.log      # Caddy process output
    data/          # Caddy data (certificates, etc.)
    config/        # Caddy config storage
  ingresses/
    {id}.json      # Ingress resource metadata
```

## API Endpoints

```
POST   /ingresses      - Create ingress
GET    /ingresses      - List ingresses  
GET    /ingresses/{id} - Get ingress by ID or name
DELETE /ingresses/{id} - Delete ingress
```

## Configuration

### Caddy Settings

| Variable | Description | Default |
|----------|-------------|---------|
| `CADDY_LISTEN_ADDRESS` | Address for ingress listeners | `0.0.0.0` |
| `CADDY_ADMIN_ADDRESS` | Address for Caddy admin API | `127.0.0.1` |
| `CADDY_ADMIN_PORT` | Port for Caddy admin API | `2019` |
| `CADDY_STOP_ON_SHUTDOWN` | Stop Caddy when hypeman shuts down | `false` |

### ACME / TLS Settings

| Variable | Description | Default |
|----------|-------------|---------|
| `ACME_EMAIL` | ACME account email (required for TLS) | |
| `ACME_DNS_PROVIDER` | DNS provider: `cloudflare` or `route53` | |
| `ACME_CA` | ACME CA URL (for staging, etc.) | Let's Encrypt production |

### Cloudflare DNS Provider

| Variable | Description |
|----------|-------------|
| `CLOUDFLARE_API_TOKEN` | Cloudflare API token with DNS edit permissions |

### AWS Route53 DNS Provider

| Variable | Description |
|----------|-------------|
| `AWS_ACCESS_KEY_ID` | AWS access key |
| `AWS_SECRET_ACCESS_KEY` | AWS secret key |
| `AWS_REGION` | AWS region (default: `us-east-1`) |
| `AWS_HOSTED_ZONE_ID` | Specific hosted zone ID (optional) |

**Note on Ports:** Each ingress rule can specify a `port` in the match criteria to listen on a specific host port. If not specified, defaults to port 80. Caddy dynamically listens on all unique ports across all ingresses.

## Security

- Admin API bound to localhost only by default
- Ingress validation ensures target instances exist
- Instance IP resolution happens at config generation time
- Caddy runs as the same user as hypeman (not root)
- Private keys for TLS certificates stored with restrictive permissions

## Daemon Lifecycle

### Startup
1. Extract Caddy binary (if needed)
2. Check for existing running Caddy (via PID file or admin API)
3. If not running, start Caddy with generated config
4. Wait for admin API to become ready

### Config Updates

Caddy's admin API allows live configuration updates:

1. Generate new JSON config
2. POST to `/load` endpoint on admin API
3. Caddy validates and applies atomically
4. Active connections are preserved during reload

### Shutdown
- By default (`CADDY_STOP_ON_SHUTDOWN=false`), Caddy continues running when hypeman exits
- Set `CADDY_STOP_ON_SHUTDOWN=true` to stop Caddy with hypeman
- Caddy can be manually stopped via admin API (`/stop`) or SIGTERM

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

- Path-based L7 routing
- Health checks for backends
- Rate limiting
- Custom error pages
