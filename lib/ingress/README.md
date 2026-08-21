# Ingress Manager

Manages external traffic routing to VM instances using Caddy as a reverse proxy with automatic TLS via ACME.

## Architecture

```
External Request         Caddy (daemon)        DNS Server        VM
    |                         |                    |               |
    | Host:api.example.com    |                    |               |
    +------------------------>| route match        |               |
                              | lookup my-api      |               |
                              +------------------->|               |
                              | A: 10.100.x.y      |               |
                              |<-------------------+               |
                              | proxy to 10.100.x.y:8080           |
                              +----------------------------------->|
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

Pattern hostnames enable convention-based routing where the subdomain maps to an instance name:

```json
{
  "name": "wildcard-ingress",
  "rules": [
    {
      "match": { "hostname": "{instance}.dev.example.com" },
      "target": { "instance": "{instance}", "port": 8080 },
      "tls": true
    }
  ]
}
```

This routes `foobar.dev.example.com` → instance `foobar`, `myapp.dev.example.com` → instance `myapp`, etc.

### Request Header Authorization

A rule can require a dedicated verification header before Caddy resolves or contacts its target:

```json
{
  "match": { "hostname": "service.example.com", "port": 443 },
  "target": { "instance": "service", "port": 8080 },
  "tls": true,
  "request_header_auth": {
    "header": "X-Origin-Verification",
    "value": "0123456789abcdef0123456789abcdef",
    "trust_forwarded_headers": true
  }
}
```

Use a cryptographically random bearer secret whose encoded value is 32–256 bytes of printable ASCII without whitespace or Caddy matcher metacharacters (`*`, `{`, `}`). For example, encode at least 32 random bytes as base64. Invalid values prevent ingress creation and configuration regeneration.

Caddy requires an exact hostname and header-value match. Missing or incorrect credentials return 403 without resolving the dynamic upstream. Before proxying an authorized request, Caddy deletes the verification header so it is never delivered to the guest. The normal reverse proxy remains responsible for HTTP streaming, WebSocket upgrades, and server-sent events. An HTTP-to-HTTPS redirect does not require the header, but the HTTPS destination does.

`trust_forwarded_headers` preserves the authenticated caller's `X-Forwarded-For`, `X-Forwarded-Proto`, and `X-Forwarded-Host` values and presents `X-Forwarded-Host` as the guest request's `Host`. Enable it only when the authenticated caller overwrites forwarding headers received from untrusted clients.

The value is sensitive but follows the same API visibility model as instance environment variables: authenticated create, get, and list responses include it. Clients should hide it by default and require an explicit option to display it. Hypeman persists ingress metadata and generated Caddy configuration in owner-only files. The value is not included in application logs or errors.

To rotate a value, delete and recreate the ingress with the replacement value. Caddy applies each configuration change with a live reload.

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

#### TLS Requirements

To use TLS on any ingress rule, you **must** configure:

1. **ACME credentials**: `ACME_EMAIL` and `ACME_DNS_PROVIDER` (with provider-specific credentials)
2. **Allowed domains**: `TLS_ALLOWED_DOMAINS` must include the hostname pattern

If TLS is requested without proper configuration, the ingress creation will fail with a descriptive error.

#### Allowed Domains (`TLS_ALLOWED_DOMAINS`)

This environment variable controls which hostnames can have TLS certificates issued. It's a comma-separated list of patterns:

| Pattern | Matches | Does NOT Match |
|---------|---------|----------------|
| `api.example.com` | `api.example.com` (exact) | Any other hostname |
| `*.example.com` | `foo.example.com`, `bar.example.com` | `example.com` (apex), `a.b.example.com` (multi-level) |
| `*` | Any hostname (use with caution) | - |

**Wildcard behavior:**
- `*.example.com` matches **single-level** subdomains only
- It does NOT match the apex domain (`example.com`)
- It does NOT match multi-level subdomains (`foo.bar.example.com`)
- To allow both apex and subdomains, use: `TLS_ALLOWED_DOMAINS=example.com,*.example.com`

**Example configuration:**
```bash
# Allow TLS for any subdomain of example.com plus the apex
TLS_ALLOWED_DOMAINS=example.com,*.example.com

# Allow TLS for specific subdomains only
TLS_ALLOWED_DOMAINS=api.example.com,www.example.com

# Allow TLS for any domain (not recommended for production)
TLS_ALLOWED_DOMAINS=*
```

#### Warning Scenarios

The ingress manager logs warnings in these situations:

- **TLS ingresses exist but ACME not configured**: If existing ingresses have `tls: true` but `ACME_EMAIL` or `ACME_DNS_PROVIDER` is not set, a warning is logged at startup. TLS will not work until ACME is configured.

- **Domain not in allowed list**: Creating an ingress with `tls: true` for a hostname not in `TLS_ALLOWED_DOMAINS` will fail with error `domain_not_allowed`.

### Hostname Routing

- Uses HTTP Host header matching (HTTP) or SNI (HTTPS)
- Supports exact hostnames (`api.example.com`) and patterns (`{instance}.example.com`)
- Exact hostnames take precedence over matching patterns on the same port
- Pattern hostnames enable convention-based routing (e.g., `foobar.example.com` → instance `foobar`)
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
| `ACME_DNS_PROVIDER` | DNS provider: `cloudflare` | |
| `ACME_CA` | ACME CA URL (for staging, etc.) | Let's Encrypt production |
| `TLS_ALLOWED_DOMAINS` | Comma-separated domain patterns allowed for TLS (required for TLS ingresses) | |
| `DNS_PROPAGATION_TIMEOUT` | Max time to wait for DNS propagation (e.g., `2m`, `120s`) | |
| `DNS_RESOLVERS` | Comma-separated DNS resolvers for propagation checking | |

### Cloudflare DNS Provider

| Variable | Description |
|----------|-------------|
| `CLOUDFLARE_API_TOKEN` | Cloudflare API token with DNS edit permissions |

**Note on Ports:** Each ingress rule can specify a `port` in the match criteria to listen on a specific host port. If not specified, defaults to port 80. Caddy dynamically listens on all unique ports across all ingresses.

## Security

- Admin API bound to localhost only by default
- Ingress validation ensures target instances exist (for exact hostnames)
- Instance IP resolution happens at request time via internal DNS server
- Caddy runs as the same user as hypeman (not root)
- Private keys for TLS certificates stored with restrictive permissions

## Daemon Lifecycle

### Startup
1. Extract Caddy binary (if needed)
2. Start internal DNS server for dynamic upstream resolution (port 5353)
3. Check for existing running Caddy (via PID file or admin API)
4. If not running, start Caddy with generated config
5. Wait for admin API to become ready

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
