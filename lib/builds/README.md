# Build System

The build system provides source-to-image builds inside ephemeral Cloud Hypervisor microVMs, enabling secure multi-tenant isolation with rootless BuildKit.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         Hypeman API                              │
│  POST /builds  →  BuildManager  →  BuildQueue                   │
│                        │                                         │
│              Start() → VsockHandler (port 5001)                 │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Builder MicroVM                              │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │  Volumes Mounted:                                            ││
│  │  - /src (source code, read-write)                           ││
│  │  - /config/build.json (build configuration, read-only)      ││
│  ├─────────────────────────────────────────────────────────────┤│
│  │  Builder Agent                                               ││
│  │  ┌─────────────┐  ┌──────────────┐  ┌────────────────────┐  ││
│  │  │ Load Config │→ │ Read User's  │→ │ Run BuildKit       │  ││
│  │  │ /config/    │  │ Dockerfile   │  │ (buildctl)         │  ││
│  │  └─────────────┘  └──────────────┘  └────────────────────┘  ││
│  │                                              │               ││
│  │                                              ▼               ││
│  │                                     Push to Registry         ││
│  │                                     (JWT token auth)         ││
│  └─────────────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                       OCI Registry                               │
│              {REGISTRY_URL}/builds/{build-id}                    │
│              (default: 10.102.0.1:8083 from VM)                 │
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
2. Write build config to disk
3. Enqueue build job
4. Create source volume from archive
5. Create config volume with `build.json`
6. Create builder VM with both volumes attached
7. Wait for build completion
8. Update metadata and cleanup

**Important**: The `Start()` method must be called to start the vsock handler for builder communication.

### Cache System (`cache.go`)

Registry-based caching with tenant isolation:

```
{registry}/cache/{tenant_scope}/{runtime}/{lockfile_hash}
```

```go
gen := NewCacheKeyGenerator("localhost:8080")
key, _ := gen.GenerateCacheKey("my-tenant", "myapp", lockfileHashes)
// key.ImportCacheArg() → "type=registry,ref=localhost:8080/cache/my-tenant/myapp/abc123"
// key.ExportCacheArg() → "type=registry,ref=localhost:8080/cache/my-tenant/myapp/abc123,mode=max"
```

### Registry Token System (`registry_token.go`)

JWT-based authentication for builder VMs to push images:

```go
generator := NewRegistryTokenGenerator(jwtSecret)
token, _ := generator.GeneratePushToken(buildID, []string{"builds/abc123", "cache/tenant-x"}, 30*time.Minute)
// Token grants push access only to specified repositories
// Validated by middleware on /v2/* registry endpoints
```

| Field | Description |
|-------|-------------|
| `BuildID` | Build job identifier for audit |
| `Repositories` | Allowed repository paths |
| `Scope` | Access scope: `push` or `pull` |
| `ExpiresAt` | Token expiry (matches build timeout) |

### Metrics (`metrics.go`)

OpenTelemetry metrics for monitoring:

| Metric | Type | Description |
|--------|------|-------------|
| `hypeman_build_duration_seconds` | Histogram | Build duration |
| `hypeman_builds_total` | Counter | Total builds by status/runtime |
| `hypeman_build_queue_length` | Gauge | Pending builds in queue |
| `hypeman_builds_active` | Gauge | Currently running builds |

### Builder Agent (`builder_agent/main.go`)

Guest binary that runs inside builder VMs:

1. Reads config from `/config/build.json`
2. Fetches secrets from host via vsock (if any)
3. Uses user-provided Dockerfile (from source or config)
4. Runs `buildctl-daemonless.sh` with cache and insecure registry flags
5. Computes provenance (lockfile hashes, source hash)
6. Reports result back via vsock

**Note**: The agent requires a Dockerfile to be provided. It can be included in the source tarball or passed via the `dockerfile` config parameter.

**Key Details**:
- Config path: `/config/build.json`
- Source path: `/src`
- Uses `registry.insecure=true` for HTTP registries
- Inherits `BUILDKITD_FLAGS` from environment

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/builds` | Submit build (multipart form) |
| `GET` | `/builds` | List all builds |
| `GET` | `/builds/{id}` | Get build details |
| `DELETE` | `/builds/{id}` | Cancel build |
| `GET` | `/builds/{id}/logs` | Stream logs (SSE) |

### Submit Build Example

```bash
# Option 1: Dockerfile in source tarball
curl -X POST http://localhost:8083/builds \
  -H "Authorization: Bearer $TOKEN" \
  -F "source=@source.tar.gz" \
  -F "cache_scope=tenant-123" \
  -F "timeout_seconds=300"

# Option 2: Dockerfile as parameter
curl -X POST http://localhost:8083/builds \
  -H "Authorization: Bearer $TOKEN" \
  -F "source=@source.tar.gz" \
  -F "dockerfile=FROM node:20-alpine
WORKDIR /app
COPY . .
RUN npm ci
CMD [\"node\", \"index.js\"]" \
  -F "cache_scope=tenant-123"
```

### Response

```json
{
  "id": "abc123",
  "status": "queued",
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

### Registry URL Configuration

The `REGISTRY_URL` must be accessible from inside builder VMs. Since `localhost` in the VM refers to the VM itself, you need to use the host's gateway IP:

```bash
# In .env
REGISTRY_URL=10.102.0.1:8083  # Gateway IP accessible from VM network
```

### Registry Authentication

Builder VMs authenticate to the registry using short-lived JWT tokens:

1. **Token Generation**: The build manager generates a scoped token for each build
2. **Token Scope**: Grants push access only to `builds/{build_id}` and `cache/{cache_scope}`
3. **Token TTL**: Matches build timeout (minimum 30 minutes)
4. **Authentication**: Builder agent sends token via Basic auth (`token:` format)

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
6. **Registry Auth**: Short-lived JWT tokens scoped to specific repositories (builds/{id}, cache/{scope})

## Builder Images

The generic builder image is in `images/generic/`:

- `generic/Dockerfile` - Minimal Alpine + BuildKit + agent (runtime-agnostic)

The generic builder does not include any runtime (Node.js, Python, etc.). Users provide their own Dockerfile which specifies the runtime. BuildKit pulls the runtime as part of the build process.

### Required Components

Builder images must include:

| Component | Source | Purpose |
|-----------|--------|---------|
| `buildctl` | `moby/buildkit:rootless` | BuildKit CLI |
| `buildctl-daemonless.sh` | `moby/buildkit:rootless` | Daemonless wrapper |
| `buildkitd` | `moby/buildkit:rootless` | BuildKit daemon |
| `buildkit-runc` | `moby/buildkit:rootless` | Container runtime (as `/usr/bin/runc`) |
| `builder-agent` | Built from `builder_agent/main.go` | Hypeman agent |
| `fuse-overlayfs` | apk/apt | Overlay filesystem support |

### Build and Push

See [`images/README.md`](./images/README.md) for detailed build instructions.

```bash
# Build and push the builder image (must use OCI mediatypes)
docker buildx build \
  --platform linux/amd64 \
  --output type=image,oci-mediatypes=true,push=true \
  --tag yourregistry/builder:latest \
  -f lib/builds/images/generic/Dockerfile \
  .
```

### Environment Variables

The builder image should set:

```dockerfile
# Empty or minimal flags - cgroups are mounted in microVM
ENV BUILDKITD_FLAGS=""
ENV HOME=/home/builder
ENV XDG_RUNTIME_DIR=/home/builder/.local/share
```

## MicroVM Requirements

Builder VMs require specific kernel and init script features:

### Cgroups

The init script mounts cgroups for BuildKit/runc:

```bash
# Cgroup v2 (preferred)
mount -t cgroup2 none /sys/fs/cgroup

# Or cgroup v1 fallback
mount -t tmpfs cgroup /sys/fs/cgroup
for ctrl in cpu cpuacct memory devices freezer blkio pids; do
  mkdir -p /sys/fs/cgroup/$ctrl
  mount -t cgroup -o $ctrl cgroup /sys/fs/cgroup/$ctrl
done
```

### Volume Mounts

Two volumes are attached to builder VMs:

1. **Source volume** (`/src`, read-write): Contains extracted source tarball
2. **Config volume** (`/config`, read-only): Contains `build.json`

The source is mounted read-write so the generated Dockerfile can be written.

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

### Unit Tests

```bash
# Run unit tests
go test ./lib/builds/... -v

# Test specific components
go test ./lib/builds/queue_test.go ./lib/builds/queue.go ./lib/builds/types.go -v
go test ./lib/builds/cache_test.go ./lib/builds/cache.go ./lib/builds/types.go ./lib/builds/errors.go -v
go test ./lib/builds/registry_token_test.go ./lib/builds/registry_token.go -v
```

### E2E Testing

1. **Start the server**:
   ```bash
   make dev
   ```

2. **Ensure builder image is available**:
   ```bash
   TOKEN=$(make gen-jwt | tail -1)
   curl -X POST http://localhost:8083/images \
     -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"name": "onkernel/builder-generic:latest"}'
   ```

3. **Create test source with Dockerfile**:
   ```bash
   mkdir -p /tmp/test-app
   echo '{"name": "test", "version": "1.0.0", "dependencies": {}}' > /tmp/test-app/package.json
   echo 'console.log("Hello!");' > /tmp/test-app/index.js
   cat > /tmp/test-app/Dockerfile << 'EOF'
   FROM node:20-alpine
   WORKDIR /app
   COPY package.json index.js ./
   CMD ["node", "index.js"]
   EOF
   tar -czf /tmp/source.tar.gz -C /tmp/test-app .
   ```

4. **Submit build**:
   ```bash
   curl -X POST http://localhost:8083/builds \
     -H "Authorization: Bearer $TOKEN" \
     -F "source=@/tmp/source.tar.gz"
   ```

5. **Poll for completion**:
   ```bash
   BUILD_ID="<id-from-response>"
   curl http://localhost:8083/builds/$BUILD_ID \
     -H "Authorization: Bearer $TOKEN"
   ```

6. **Run the built image**:
   ```bash
   curl -X POST http://localhost:8083/instances \
     -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{
       "name": "test-app",
       "image": "builds/'$BUILD_ID':latest",
       "size": "1GB",
       "vcpus": 1
     }'
   ```

## Troubleshooting

### Common Issues

| Error | Cause | Solution |
|-------|-------|----------|
| `image not found` | Builder image not imported | Import image using `POST /images` endpoint |
| `no cgroup mount found` | Cgroups not mounted in VM | Update init script to mount cgroups |
| `http: server gave HTTP response to HTTPS client` | BuildKit using HTTPS for HTTP registry | Add `registry.insecure=true` to output flags |
| `connection refused` to localhost:8080 | Registry URL not accessible from VM | Use gateway IP (10.102.0.1) instead of localhost |
| `401 Unauthorized` | Registry auth issue | Check registry_token in config.json; verify middleware handles Basic auth |
| `No space left on device` | Instance memory too small for image | Use at least 1GB RAM for Node.js images |
| `can't enable NoProcessSandbox without Rootless` | Wrong BUILDKITD_FLAGS | Use empty flags or remove the flag |

### Debug Builder VM

Check logs of the builder instance:

```bash
# List instances
curl http://localhost:8083/instances -H "Authorization: Bearer $TOKEN" | jq

# Get builder instance logs
INSTANCE_ID="<builder-instance-id>"
curl http://localhost:8083/instances/$INSTANCE_ID/logs \
  -H "Authorization: Bearer $TOKEN"
```

### Verify Build Config

Check the config volume contents:

```bash
cat $DATA_DIR/builds/$BUILD_ID/config.json
```

Expected format:
```json
{
  "job_id": "abc123",
  "registry_url": "10.102.0.1:8083",
  "registry_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "cache_scope": "my-tenant",
  "source_path": "/src",
  "dockerfile": "FROM node:20-alpine\nWORKDIR /app\n...",
  "timeout_seconds": 300,
  "network_mode": "egress"
}
```

Note: `registry_token` is a short-lived JWT granting push access to `builds/abc123` and `cache/my-tenant`.
