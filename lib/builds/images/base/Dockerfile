# Base builder image with rootless BuildKit
# This serves as the foundation for all runtime-specific builder images

FROM moby/buildkit:rootless

# Switch to root temporarily to install additional packages
USER root

# Install common build dependencies
RUN apk add --no-cache \
    ca-certificates \
    git \
    curl \
    jq \
    tar \
    gzip

# Create directories for the builder agent
RUN mkdir -p /config /run/secrets

# Switch back to unprivileged user
USER 1000

# Set buildkit flags for rootless operation
ENV BUILDKITD_FLAGS="--oci-worker-no-process-sandbox"

# Default entrypoint is buildkitd, but we'll override for builder-agent
ENTRYPOINT ["/usr/bin/buildctl-daemonless.sh"]

