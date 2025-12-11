# middleware

HTTP middleware for the hypeman API.

## Authentication

JWT bearer token validation for protected endpoints. Extracts user identity and adds it to the request context.

## Resource Resolution

Automatically resolves user-provided identifiers (IDs, names, or prefixes) to full resource objects before handlers run. This enables:

- **Flexible lookups**: Users can reference resources by full ID, name, or ID prefix
- **Consistent error handling**: Returns 404 for not-found, handles ambiguous matches
- **Automatic logging enrichment**: The resolved resource ID is added to the request logger

Handlers can trust that if they're called, the resource exists and is available via `mw.GetResolvedInstance[T](ctx)` etc.

## Observability

OpenTelemetry instrumentation for HTTP requests, including request counts, latencies, and status codes.
