# Build System

The build system provides source-to-image builds inside ephemeral Cloud Hypervisor microVMs, enabling secure multi-tenant isolation with rootless BuildKit.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         Hypeman API                              │
│  POST /v1/builds  →  BuildManager  →  BuildQueue                │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Builder MicroVM                              │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │  Builder Agent                                               ││
│  │  ┌─────────────┐  ┌──────────────┐  ┌────────────────────┐  ││
│  │  │ Load Config │→ │ Generate     │→ │ Run BuildKit       │  ││
│  │  │ from disk   │  │ Dockerfile   │  │ (rootless)         │  ││
│  │  └─────────────┘  └──────────────┘  └────────────────────┘  ││
│  │                                              │               ││
│  │                                              ▼               ││
│  │                                     Push to Registry         ││
│  │                                              │               ││
│  │                                              ▼               ││
│  │                                     Report via vsock         ││
│  └─────────────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                       OCI Registry                               │
│              localhost:8080/builds/{build-id}                    │
└─────────────────────────────────────────────────────────────────┘
```

## Components

### Core Types (`types.go`)

| Type | Description |
|------|-------------|
| `Build` | Build job status and metadata |
| `CreateBuildRequest` | API request to create a build |
| `BuildConfig` | Configuration passed to builder VM |
| `BuildResult` | Result returned by builder agent |
| `BuildProvenance` | Audit trail for reproducibility |
| `BuildPolicy` | Resource limits and network policy |

### Build Queue (`queue.go`)

In-memory queue with configurable concurrency:

```go
queue := NewBuildQueue(maxConcurrent)
position := queue.Enqueue(buildID, request, startFunc)
queue.Cancel(buildID)
queue.GetPosition(buildID)
```

**Recovery**: On startup, `listPendingBuilds()` scans disk metadata for incomplete builds and re-enqueues them in FIFO order.

### Storage (`storage.go`)

Builds are persisted to `$DATA_DIR/builds/{id}/`:

```
builds/
└── {build-id}/
    ├── metadata.json    # Build status, provenance
    ├── config.json      # Config for builder VM
    ├── source/
    │   └── source.tar.gz
    └── logs/
        └── build.log
```

### Build Manager (`manager.go`)

Orchestrates the build lifecycle:

1. Validate request and store source
2. Enqueue build job
3. Create builder VM with source volume attached
4. Wait for result via vsock
5. Update metadata and cleanup

### Dockerfile Templates (`templates/`)

Auto-generates Dockerfiles based on runtime and detected lockfiles:

| Runtime | Package Managers |
|---------|-----------------|
| `nodejs20` | npm, yarn, pnpm |
| `python312` | pip, poetry, pipenv |

```go
gen, _ := templates.GetGenerator("nodejs20")
dockerfile, _ := gen.Generate(sourceDir, baseImageDigest)
```

### Cache System (`cache.go`)

Registry-based caching with tenant isolation:

```
{registry}/cache/{tenant_scope}/{runtime}/{lockfile_hash}
```

```go
gen := NewCacheKeyGenerator("localhost:8080")
key, _ := gen.GenerateCacheKey("my-tenant", "nodejs20", lockfileHashes)
// key.ImportCacheArg() → "type=registry,ref=localhost:8080/cache/my-tenant/nodejs20/abc123"
// key.ExportCacheArg() → "type=registry,ref=localhost:8080/cache/my-tenant/nodejs20/abc123,mode=max"
```

### Builder Agent (`builder_agent/main.go`)

Guest binary that runs inside builder VMs:

1. Reads config from `/config/build.json`
2. Fetches secrets from host via vsock
3. Generates Dockerfile (if not provided)
4. Runs `buildctl-daemonless.sh` with cache flags
5. Computes provenance (lockfile hashes, source hash)
6. Reports result back via vsock

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/builds` | Submit build (multipart form) |
| `GET` | `/v1/builds` | List all builds |
| `GET` | `/v1/builds/{id}` | Get build details |
| `DELETE` | `/v1/builds/{id}` | Cancel build |
| `GET` | `/v1/builds/{id}/logs` | Stream logs (SSE) |

### Submit Build Example

```bash
curl -X POST http://localhost:8080/v1/builds \
  -H "Authorization: Bearer $TOKEN" \
  -F "runtime=nodejs20" \
  -F "source=@source.tar.gz" \
  -F "cache_scope=tenant-123" \
  -F "timeout_seconds=300"
```

### Response

```json
{
  "id": "abc123",
  "status": "queued",
  "runtime": "nodejs20",
  "queue_position": 1,
  "created_at": "2025-01-15T10:00:00Z"
}
```

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `MAX_CONCURRENT_SOURCE_BUILDS` | `2` | Max parallel builds |
| `BUILDER_IMAGE` | `hypeman/builder:latest` | Builder VM image |
| `REGISTRY_URL` | `localhost:8080` | Registry for built images |
| `BUILD_TIMEOUT` | `600` | Default timeout (seconds) |

## Build Status Flow

```
queued → building → pushing → ready
                 ↘         ↗
                   failed
                      ↑
                  cancelled
```

## Security Model

1. **Isolation**: Each build runs in a fresh microVM (Cloud Hypervisor)
2. **Rootless**: BuildKit runs without root privileges
3. **Network Control**: `network_mode: isolated` or `egress` with optional domain allowlist
4. **Secret Handling**: Secrets fetched via vsock, never written to disk in guest
5. **Cache Isolation**: Per-tenant cache scopes prevent cross-tenant cache poisoning

## Builder Images

Builder images are in `images/`:

- `base/Dockerfile` - BuildKit base
- `nodejs20/Dockerfile` - Node.js 20 + BuildKit + agent
- `python312/Dockerfile` - Python 3.12 + BuildKit + agent

Build and push:

```bash
cd lib/builds/images/nodejs20
docker build -t hypeman/builder-nodejs20:latest -f Dockerfile ../../../..
```

## Provenance

Each build records provenance for reproducibility:

```json
{
  "base_image_digest": "sha256:abc123...",
  "source_hash": "sha256:def456...",
  "lockfile_hashes": {
    "package-lock.json": "sha256:..."
  },
  "toolchain_version": "v20.10.0",
  "buildkit_version": "v0.12.0",
  "timestamp": "2025-01-15T10:05:00Z"
}
```

## Testing

```bash
# Run unit tests
go test ./lib/builds/... -v

# Test specific components
go test ./lib/builds/queue_test.go ./lib/builds/queue.go ./lib/builds/types.go -v
go test ./lib/builds/cache_test.go ./lib/builds/cache.go ./lib/builds/types.go ./lib/builds/errors.go -v
go test ./lib/builds/templates/... -v
```

