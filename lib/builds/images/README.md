# Builder Images

Builder images run inside Hypeman microVMs to execute source-to-image builds using BuildKit.

## Available Images

| Image | Runtime | Use Case |
|-------|---------|----------|
| `nodejs20/` | Node.js 20.x | npm, yarn, pnpm projects |
| `python312/` | Python 3.12 | pip, poetry, pipenv projects |
| `base/` | None | Base BuildKit image (for custom runtimes) |

## Creating a Builder Image

### Step 1: Create Dockerfile

Create a new directory under `images/` with a Dockerfile:

```dockerfile
# Use BuildKit rootless as base for build tools
FROM moby/buildkit:rootless AS buildkit

# Use your runtime base image
FROM node:20-alpine

# Install required dependencies
RUN apk add --no-cache \
    fuse-overlayfs \
    shadow \
    newuidmap \
    ca-certificates

# Create non-root builder user
RUN adduser -D -u 1000 builder && \
    mkdir -p /home/builder/.local/share/buildkit && \
    chown -R builder:builder /home/builder

# Copy BuildKit binaries (these specific paths are required)
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
COPY --from=buildkit /usr/bin/buildctl-daemonless.sh /usr/bin/buildctl-daemonless.sh
COPY --from=buildkit /usr/bin/buildkitd /usr/bin/buildkitd
COPY --from=buildkit /usr/bin/buildkit-runc /usr/bin/runc

# Copy the builder agent (built during image build)
COPY builder-agent /usr/bin/builder-agent

# Set environment variables
ENV HOME=/home/builder
ENV XDG_RUNTIME_DIR=/home/builder/.local/share
ENV BUILDKITD_FLAGS=""

# Run as builder user
USER builder
WORKDIR /home/builder

# The agent is the entrypoint
ENTRYPOINT ["/usr/bin/builder-agent"]
```

### Step 2: Required Components

Every builder image **must** include:

| Component | Path | Source | Purpose |
|-----------|------|--------|---------|
| `buildctl` | `/usr/bin/buildctl` | `moby/buildkit:rootless` | BuildKit CLI |
| `buildctl-daemonless.sh` | `/usr/bin/buildctl-daemonless.sh` | `moby/buildkit:rootless` | Runs buildkitd + buildctl together |
| `buildkitd` | `/usr/bin/buildkitd` | `moby/buildkit:rootless` | BuildKit daemon |
| `runc` | `/usr/bin/runc` | `moby/buildkit:rootless` (as `buildkit-runc`) | Container runtime |
| `builder-agent` | `/usr/bin/builder-agent` | Built from Go source | Hypeman orchestration agent |
| `fuse-overlayfs` | System package | apk/apt | Overlay filesystem for rootless builds |

### Step 3: Build the Agent

The builder agent must be compiled for the target architecture:

```bash
# From repository root
cd lib/builds/builder_agent
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o builder-agent .
```

### Step 4: Build the Image (OCI Format)

**Important**: Hypeman uses `umoci` to extract images, which requires OCI format (not Docker v2 manifest).

```bash
# From repository root

# Build agent first
cd lib/builds/builder_agent
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o builder-agent .
cd ../../..

# Build image with OCI output
docker buildx build --platform linux/amd64 \
  -t yourregistry/builder-nodejs20:latest \
  -f lib/builds/images/nodejs20/Dockerfile \
  --output type=oci,dest=/tmp/builder.tar \
  .
```

### Step 5: Push to Registry

Use `crane` (from go-containerregistry) to push in OCI format:

```bash
# Extract the OCI tarball
mkdir -p /tmp/oci-builder
tar -xf /tmp/builder.tar -C /tmp/oci-builder

# Push to registry
crane push /tmp/oci-builder yourregistry/builder-nodejs20:latest
```

### Step 6: Configure Hypeman

Set the builder image in your `.env`:

```bash
BUILDER_IMAGE=yourregistry/builder-nodejs20:latest
```

## Testing Your Builder Image

### 1. Pull the Image into Hypeman

```bash
TOKEN=$(make gen-jwt | tail -1)
curl -X POST http://localhost:8083/images \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "yourregistry/builder-nodejs20:latest"}'
```

### 2. Submit a Test Build

```bash
# Create minimal test source
mkdir -p /tmp/test-app
echo '{"name": "test", "version": "1.0.0"}' > /tmp/test-app/package.json
echo '{"lockfileVersion": 3, "packages": {}}' > /tmp/test-app/package-lock.json
echo 'console.log("Hello from test build!");' > /tmp/test-app/index.js
tar -czf /tmp/source.tar.gz -C /tmp/test-app .

# Submit build
curl -X POST http://localhost:8083/builds \
  -H "Authorization: Bearer $TOKEN" \
  -F "runtime=nodejs20" \
  -F "source=@/tmp/source.tar.gz"
```

### 3. Check Build Status

```bash
BUILD_ID="<id-from-response>"
curl http://localhost:8083/builds/$BUILD_ID \
  -H "Authorization: Bearer $TOKEN" | jq
```

### 4. Debug Failed Builds

If the build fails, check the builder instance logs:

```bash
# Find the builder instance
curl http://localhost:8083/instances \
  -H "Authorization: Bearer $TOKEN" | jq '.[] | select(.name | startswith("builder-"))'

# Get its logs
INSTANCE_ID="<builder-instance-id>"
curl "http://localhost:8083/instances/$INSTANCE_ID/logs" \
  -H "Authorization: Bearer $TOKEN"
```

## Environment Variables

Builder images should configure these environment variables:

| Variable | Value | Purpose |
|----------|-------|---------|
| `HOME` | `/home/builder` | User home directory |
| `XDG_RUNTIME_DIR` | `/home/builder/.local/share` | Runtime directory for BuildKit |
| `BUILDKITD_FLAGS` | `""` (empty) | BuildKit daemon flags (cgroups are mounted in VM) |

## MicroVM Runtime Environment

When the builder image runs inside a Hypeman microVM:

1. **Volumes mounted**:
   - `/src` - Source code (read-write)
   - `/config/build.json` - Build configuration (read-only)

2. **Cgroups**: Mounted by init script at `/sys/fs/cgroup` (v2 preferred, v1 fallback)

3. **Network**: Access to host registry via gateway IP `10.102.0.1`

4. **Registry**: HTTP (insecure) - agent adds `registry.insecure=true` flag

## Troubleshooting

| Issue | Cause | Solution |
|-------|-------|----------|
| `runc: not found` | Missing or wrong path | Copy `buildkit-runc` to `/usr/bin/runc` |
| `no cgroup mount found` | Cgroups not available | Ensure VM init script mounts cgroups |
| `fuse-overlayfs: not found` | Missing package | Add `fuse-overlayfs` to image |
| `permission denied` on buildkit | Wrong user/permissions | Run as non-root user with proper home dir |
| `can't enable NoProcessSandbox without Rootless` | Wrong BUILDKITD_FLAGS | Set `BUILDKITD_FLAGS=""` |

## Adding a New Runtime

To add support for a new runtime (e.g., Ruby, Go):

1. Create `images/ruby32/Dockerfile` based on the template above
2. Add Dockerfile template in `templates/templates.go`:
   ```go
   var ruby32Template = `FROM {{.BaseImage}}
   COPY . /app
   WORKDIR /app
   RUN bundle install
   CMD ["ruby", "app.rb"]
   `
   ```
3. Register the generator in `templates/templates.go`
4. Build and push the builder image
5. Test with a sample project
