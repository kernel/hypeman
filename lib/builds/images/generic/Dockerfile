# Generic Builder Image
# Contains rootless BuildKit + builder agent + guest-agent for debugging
# Builds any Dockerfile provided by the user

FROM moby/buildkit:rootless AS buildkit

# Build the builder-agent and guest-agent (multi-stage build from hypeman repo)
FROM golang:1.25-alpine AS agent-builder

WORKDIR /app

# Copy go.mod and go.sum first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the builder_agent and guest_agent sources
COPY lib/builds/builder_agent/ ./lib/builds/builder_agent/
COPY lib/system/guest_agent/ ./lib/system/guest_agent/
# Only copy proto files for guest-agent (not client.go which has host-side deps)
COPY lib/guest/guest.pb.go lib/guest/guest_grpc.pb.go ./lib/guest/

# Build the builder-agent
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /builder-agent ./lib/builds/builder_agent

# Build the guest-agent (for exec into builder VMs)
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /guest-agent ./lib/system/guest_agent

# Final builder image - minimal alpine base
FROM alpine:3.21

# Copy BuildKit binaries from official image
COPY --from=buildkit /usr/bin/buildctl /usr/bin/buildctl
COPY --from=buildkit /usr/bin/buildctl-daemonless.sh /usr/bin/buildctl-daemonless.sh
COPY --from=buildkit /usr/bin/buildkitd /usr/bin/buildkitd
COPY --from=buildkit /usr/bin/buildkit-runc /usr/bin/runc

# Copy builder agent and guest agent
COPY --from=agent-builder /builder-agent /usr/bin/builder-agent
COPY --from=agent-builder /guest-agent /usr/bin/guest-agent

# Install minimal dependencies
RUN apk add --no-cache \
    ca-certificates \
    git \
    curl \
    fuse-overlayfs

# Create unprivileged user for rootless BuildKit
RUN adduser -D -u 1000 builder && \
    mkdir -p /home/builder/.local/share/buildkit /config /run/secrets /src && \
    chown -R builder:builder /home/builder /config /run/secrets /src

# Switch to unprivileged user
USER builder
WORKDIR /src

# Set environment for buildkit in microVM
ENV BUILDKITD_FLAGS=""
ENV HOME=/home/builder
ENV XDG_RUNTIME_DIR=/home/builder/.local/share

# Run builder agent as entrypoint
ENTRYPOINT ["/usr/bin/builder-agent"]



