# Scoped API Key Permissions

Hypeman API keys can be restricted to specific operations using scoped permissions. This lets you create least-privilege tokens — for example, a token that can only read instance status but not create or delete anything.

## How It Works

Permissions are embedded in the JWT token as a `permissions` claim containing an array of scope strings. When a request hits an API endpoint, the middleware checks whether the token carries the required scope for that endpoint. If the scope is missing, the request is rejected with `403 Forbidden`.

Tokens without a `permissions` claim are treated as having **full access**. This means all existing tokens continue to work without any changes.

## Available Scopes

Scopes follow the pattern `resource:action`. Each resource type supports `read`, `write`, and `delete` actions.

### Instances

| Scope | Grants access to |
|---|---|
| `instance:read` | List instances, get instance details, view logs, get stats, stat paths |
| `instance:write` | Create, start, stop, standby, restore, fork instances; exec and cp (WebSocket) |
| `instance:delete` | Delete instances |

### Images

| Scope | Grants access to |
|---|---|
| `image:read` | List images, get image details |
| `image:write` | Pull/create images |
| `image:delete` | Delete images |

### Volumes

| Scope | Grants access to |
|---|---|
| `volume:read` | List volumes, get volume details |
| `volume:write` | Create volumes, create from archive, attach/detach volumes |
| `volume:delete` | Delete volumes |

### Snapshots

| Scope | Grants access to |
|---|---|
| `snapshot:read` | List snapshots, get snapshot details |
| `snapshot:write` | Create snapshots, restore snapshots, fork from snapshots |
| `snapshot:delete` | Delete snapshots |

### Builds

| Scope | Grants access to |
|---|---|
| `build:read` | List builds, get build details, stream build events |
| `build:write` | Create builds |
| `build:delete` | Cancel/delete builds |
| `build:admin` | Operator-only build options: `is_admin_build`, explicit `cache_scope` |

### Devices

| Scope | Grants access to |
|---|---|
| `device:read` | List devices, get device details, list available devices |
| `device:write` | Register devices |
| `device:delete` | Unregister devices |

### Ingresses

| Scope | Grants access to |
|---|---|
| `ingress:read` | List ingresses, get ingress details |
| `ingress:write` | Create ingresses |
| `ingress:delete` | Delete ingresses |

### Resources

| Scope | Grants access to |
|---|---|
| `resource:read` | Health check, resource capacity/allocations |

### Wildcard

The `*` scope grants access to all endpoints. It is equivalent to a full-access token but explicitly declared in the permissions claim.

## Creating Scoped Tokens

Use the `hypeman-token` CLI to generate tokens with specific scopes.

```bash
# List all available scopes
hypeman-token -list-scopes

# Create a read-only token for instances and images
hypeman-token -user-id myuser -scopes "instance:read,image:read"

# Create a token that can manage instances but not delete them
hypeman-token -user-id myuser -scopes "instance:read,instance:write"

# Create a full-access token with explicit wildcard
hypeman-token -user-id myuser -scopes "*"

# Create a full-access token (legacy style, no permissions claim)
hypeman-token -user-id myuser
```

Multiple scopes are comma-separated. Whitespace around scope names is trimmed.

## Backward Compatibility

Existing tokens that were generated before this feature was added do not have a `permissions` claim in the JWT. These tokens are treated as having **full access** to all endpoints — no action is required to keep them working.

Only tokens generated with the `-scopes` flag carry a `permissions` claim and are subject to scope enforcement.

## Example Scenarios

**Monitoring / dashboard token** — can read instance status and stats but cannot modify anything:
```bash
hypeman-token -user-id dashboard -scopes "instance:read,resource:read"
```

**CI/CD build token** — can create builds and pull images, but has no access to instances or volumes:
```bash
hypeman-token -user-id ci -scopes "build:read,build:write,image:read,image:write"
```

**Instance operator** — full instance lifecycle management without access to images or builds:
```bash
hypeman-token -user-id operator -scopes "instance:read,instance:write,instance:delete,volume:read,volume:write,snapshot:read,snapshot:write"
```

**Read-only audit token** — can view everything but change nothing:
```bash
hypeman-token -user-id auditor -scopes "instance:read,image:read,volume:read,snapshot:read,build:read,device:read,ingress:read,resource:read"
```

## Error Responses

When a scoped token attempts an operation it does not have permission for, the API returns:

```json
{"code": "Forbidden", "message": "missing required scope: instance:write"}
```

The response includes the specific scope that was required, making it straightforward to diagnose permission issues.
