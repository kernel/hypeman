# Node.js 20 Builder Image
# Contains rootless BuildKit + Node.js toolchain + builder agent

FROM moby/buildkit:rootless AS buildkit

# Build the builder-agent (multi-stage build from hypeman repo)
FROM golang:1.25-alpine AS agent-builder

WORKDIR /app
COPY lib/builds/builder_agent/ ./
RUN go build -ldflags="-s -w" -o /builder-agent .

# Final builder image
FROM node:20-alpine

# Copy BuildKit from official image
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
COPY --from=buildkit /usr/bin/buildctl-daemonless.sh /usr/bin/buildctl-daemonless.sh
COPY --from=buildkit /usr/bin/buildkitd /usr/bin/buildkitd

# Copy builder agent
COPY --from=agent-builder /builder-agent /usr/bin/builder-agent

# Install additional dependencies
RUN apk add --no-cache \
    ca-certificates \
    git \
    curl \
    jq \
    tar \
    gzip \
    shadow \
    fuse-overlayfs

# Create unprivileged user for rootless BuildKit
RUN adduser -D -u 1000 builder && \
    mkdir -p /home/builder/.local/share/buildkit && \
    chown -R builder:builder /home/builder

# Create directories for build
RUN mkdir -p /config /run/secrets /src && \
    chown -R builder:builder /config /run/secrets /src

# Enable corepack for pnpm/yarn support
RUN corepack enable

# Switch to unprivileged user
USER builder
WORKDIR /src

# Set environment for rootless buildkit
ENV BUILDKITD_FLAGS="--oci-worker-no-process-sandbox"
ENV HOME=/home/builder
ENV XDG_RUNTIME_DIR=/home/builder/.local/share

# Run builder agent as entrypoint
ENTRYPOINT ["/usr/bin/builder-agent"]

