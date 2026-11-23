# middleware

HTTP middleware for the hypeman API.

## Authentication

JWT bearer token validation for protected endpoints. Extracts user identity and adds it to the request context.

Tokens whose SHA-256 appears in `revoked_token_hashes` are rejected even when they are correctly signed and unexpired, so a single leaked token can be revoked without rotating `jwt_secret`. Revoked tokens are logged at warn level with the request path and remote address.

A token issued by the OIDC assertion exchange (`POST /oauth/token`, see [docs/oidc-federation.md](../../docs/oidc-federation.md)) names the federation rule it was issued for. It is rejected once that rule is removed or changed, and it always carries an explicit `permissions` list.

## Resource Resolution

Automatically resolves user-provided identifiers (IDs, names, or prefixes) to full resource objects before handlers run. This enables:

- **Flexible lookups**: Users can reference resources by full ID, name, or ID prefix
- **Consistent error handling**: Returns 404 for not-found, handles ambiguous matches
- **Automatic logging enrichment**: The resolved resource ID is added to the request logger

Handlers can trust that if they're called, the resource exists and is available via `mw.GetResolvedInstance[T](ctx)` etc.

## Observability

OpenTelemetry instrumentation for HTTP requests, including request counts, latencies, and status codes.
