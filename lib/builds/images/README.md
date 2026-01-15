# Generic Builder Image

The generic builder image runs inside Hypeman microVMs to execute source-to-image builds using BuildKit. It is runtime-agnostic - users provide their own Dockerfile which specifies the runtime.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ Generic Builder Image (~50MB)                               │
│                                                             │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │ BuildKit    │  │ builder-    │  │ Minimal Alpine      │ │
│  │ (daemonless)│  │ agent       │  │ (git, curl, fuse)   │ │
│  └─────────────┘  └─────────────┘  └─────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
                            │
                            ▼
                    User's Dockerfile
                            │
                            ▼
            ┌───────────────────────────────┐
            │ FROM node:20-alpine           │
            │ FROM python:3.12-slim         │
            │ FROM rust:1.75                │
            │ FROM golang:1.22              │
            │ ... any base image            │
            └───────────────────────────────┘
```

## Key Benefits

- **One image to maintain** - No more runtime-specific builder images
- **Any Dockerfile works** - Node.js, Python, Rust, Go, Java, Ruby, etc.
- **Smaller footprint** - ~50MB vs 200MB+ for runtime-specific images
- **User-controlled versions** - Users specify their runtime version in their Dockerfile

## Directory Structure

```
images/
└── generic/
    └── Dockerfile    # The generic builder image
```

## Building the Generic Builder Image

Hypeman supports both Docker v2 and OCI image formats. You can use standard `docker build`
or `docker buildx` - both work.

### Prerequisites

1. **Docker** installed
2. **Docker Hub login** (or your registry):
   ```bash
   docker login
   ```

### 1. Build and Push

```bash
# From repository root
docker build \
  -t hirokernel/builder-generic:latest \
  -f lib/builds/images/generic/Dockerfile \
  .

docker push hirokernel/builder-generic:latest
```

Or with buildx for multi-platform support:

```bash
docker buildx build \
  --platform linux/amd64 \
  --push \
  --tag hirokernel/builder-generic:latest \
  -f lib/builds/images/generic/Dockerfile \
  .
```

### 2. Import into Hypeman

```bash
# Generate a token
TOKEN=$(make gen-jwt | tail -1)

# Import the image
curl -X POST http://localhost:8083/images \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "hirokernel/builder-generic:latest"}'

# Wait for import to complete
curl http://localhost:8083/images/docker.io%2Fhirokernel%2Fbuilder-generic:latest \
  -H "Authorization: Bearer $TOKEN"
```

### 3. Configure Hypeman

Set the builder image in your `.env`:

```bash
BUILDER_IMAGE=hirokernel/builder-generic:latest
```

### Building for Local Testing (without pushing)

```bash
# Build locally
docker build \
  -t hypeman/builder:local \
  -f lib/builds/images/generic/Dockerfile \
  .

# Run locally to test
docker run --rm hypeman/builder:local --help
```

## Usage

### Submitting a Build

Users must provide a Dockerfile either:
1. **In the source tarball** - Include a `Dockerfile` in the root of the source
2. **As a parameter** - Pass `dockerfile` content in the API request

```bash
# Option 1: Dockerfile in source tarball
tar -czf source.tar.gz Dockerfile package.json index.js

curl -X POST http://localhost:8083/builds \
  -H "Authorization: Bearer $TOKEN" \
  -F "source=@source.tar.gz"

# Option 2: Dockerfile as parameter
curl -X POST http://localhost:8083/builds \
  -H "Authorization: Bearer $TOKEN" \
  -F "source=@source.tar.gz" \
  -F "dockerfile=FROM node:20-alpine
WORKDIR /app
COPY . .
RUN npm ci
CMD [\"node\", \"index.js\"]"
```

### Example Dockerfiles

**Node.js:**
```dockerfile
FROM node:20-alpine
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
CMD ["node", "index.js"]
```

**Python:**
```dockerfile
FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
CMD ["python", "main.py"]
```

**Go:**
```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o main .

FROM alpine:3.21
COPY --from=builder /app/main /main
CMD ["/main"]
```

**Rust:**
```dockerfile
FROM rust:1.75 AS builder
WORKDIR /app
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN cargo build --release

FROM debian:bookworm-slim
COPY --from=builder /app/target/release/myapp /myapp
CMD ["/myapp"]
```

## Required Components

The generic builder image contains:

| Component | Path | Purpose |
|-----------|------|---------|
| `buildctl` | `/usr/bin/buildctl` | BuildKit CLI |
| `buildctl-daemonless.sh` | `/usr/bin/buildctl-daemonless.sh` | Runs buildkitd + buildctl |
| `buildkitd` | `/usr/bin/buildkitd` | BuildKit daemon |
| `runc` | `/usr/bin/runc` | Container runtime |
| `builder-agent` | `/usr/bin/builder-agent` | Hypeman orchestration |
| `fuse-overlayfs` | System package | Rootless overlay filesystem |
| `git` | System package | Git operations (for go mod, etc.) |
| `curl` | System package | Network utilities |

## Environment Variables

| Variable | Value | Purpose |
|----------|-------|---------|
| `HOME` | `/home/builder` | User home directory |
| `XDG_RUNTIME_DIR` | `/home/builder/.local/share` | Runtime directory for BuildKit |
| `BUILDKITD_FLAGS` | `""` (empty) | BuildKit daemon flags |

## MicroVM Runtime Environment

When the builder runs inside a Hypeman microVM:

1. **Volumes mounted**:
   - `/src` - Source code (read-write)
   - `/config/build.json` - Build configuration (read-only)

2. **Cgroups**: Mounted at `/sys/fs/cgroup`

3. **Network**: Access to host registry via gateway IP `10.102.0.1`

4. **Registry**: Uses HTTP (insecure) with `registry.insecure=true`

## Troubleshooting

| Issue | Cause | Solution |
|-------|-------|----------|
| Image import stuck on `pending`/`failed` | Network or registry issue | Check Hypeman logs, verify registry access |
| `Dockerfile required` | No Dockerfile in source or parameter | Include Dockerfile in tarball or pass as parameter |
| `401 Unauthorized` during push | Registry token issue | Check builder agent logs, verify token generation |
| `runc: not found` | BuildKit binaries missing | Rebuild the builder image |
| `no cgroup mount found` | Cgroups not available | Check VM init script |
| `fuse-overlayfs: not found` | Missing package | Rebuild image with fuse-overlayfs |
| `permission denied` | Wrong user/permissions | Ensure running as `builder` user |

### Debugging Image Import Issues

```bash
# Check image status
cat ~/hypeman_data_dir/images/docker.io/hirokernel/builder-generic/*/metadata.json | jq .

# Check OCI cache index
cat ~/hypeman_data_dir/system/oci-cache/index.json | jq '.manifests[-1]'
```

## Using the Generic Builder

The generic builder accepts any Dockerfile. To use it:

1. **Include a Dockerfile** in your source tarball (or pass it via the `dockerfile` parameter)
2. **Your Dockerfile specifies the runtime** - e.g., `FROM node:20-alpine` or `FROM python:3.12-slim`
3. **Configure `BUILDER_IMAGE`** in your `.env` to point to the generic builder image
