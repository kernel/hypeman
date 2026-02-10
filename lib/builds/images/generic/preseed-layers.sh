#!/bin/sh
# Pre-seed BuildKit content store with base image layers.
#
# This script builds the builder image and then warms BuildKit's content store
# by pulling and extracting each base image. The result is a builder image that
# skips the ~13s base image extraction step on first tenant builds.
#
# Usage:
#   ./preseed-layers.sh [options] <image1> [image2] ...
#
# Options:
#   -t, --tag TAG      Final image tag (default: onkernel/builder-generic:latest)
#   -b, --base TAG     Base builder image tag to warm (default: builds from Dockerfile)
#   -h, --help         Show this help
#
# Examples:
#   ./preseed-layers.sh docker.io/onkernel/nodejs22-base:0.1.1
#   ./preseed-layers.sh -t myregistry/builder:v2 onkernel/nodejs22-base:0.1.1 onkernel/python311-base:0.1.1

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"

TAG="onkernel/builder-generic:latest"
BASE_TAG=""
IMAGES=""

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \?//'
    exit 0
}

while [ $# -gt 0 ]; do
    case "$1" in
        -t|--tag)   TAG="$2"; shift 2 ;;
        -b|--base)  BASE_TAG="$2"; shift 2 ;;
        -h|--help)  usage ;;
        *)          IMAGES="$IMAGES $1"; shift ;;
    esac
done

IMAGES="$(echo "$IMAGES" | xargs)"
if [ -z "$IMAGES" ]; then
    echo "Error: at least one base image is required"
    echo "Usage: $0 [options] <image1> [image2] ..."
    exit 1
fi

CONTAINER_NAME="builder-preseed-$$"
BUILDKIT_ROOT="/home/builder/.local/share/buildkit"

cleanup() {
    echo "Cleaning up..."
    docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

# Step 1: Build the base builder image (if no --base provided)
if [ -z "$BASE_TAG" ]; then
    BASE_TAG="builder-preseed-base:$$"
    echo "==> Building base builder image..."
    docker buildx build \
        --output "type=image,oci-mediatypes=true,name=$BASE_TAG" \
        --load \
        -f "$SCRIPT_DIR/Dockerfile" \
        "$REPO_ROOT"
fi

# Step 2: Run privileged container for warmup
echo "==> Starting warmup container..."
docker run -d --privileged --name "$CONTAINER_NAME" "$BASE_TAG" sleep 3600

# Create /run/buildkit (builder user can't create it)
docker exec -u root "$CONTAINER_NAME" mkdir -p /run/buildkit
docker exec -u root "$CONTAINER_NAME" chown builder:builder /run/buildkit

# Start buildkitd as root (avoids rootless namespace requirement)
echo "==> Starting buildkitd..."
docker exec -u root "$CONTAINER_NAME" sh -c \
    "buildkitd --root $BUILDKIT_ROOT --addr unix:///run/buildkit/buildkitd.sock 2>/tmp/buildkitd.log &"
sleep 3

# Verify buildkitd is running
if ! docker exec "$CONTAINER_NAME" sh -c 'pidof buildkitd >/dev/null'; then
    echo "Error: buildkitd failed to start"
    docker exec "$CONTAINER_NAME" cat /tmp/buildkitd.log
    exit 1
fi

# Step 3: Warm each base image
for IMAGE in $IMAGES; do
    echo "==> Pre-seeding: $IMAGE"
    docker exec -u root "$CONTAINER_NAME" sh -c "
        echo 'FROM $IMAGE
RUN echo preseed-done' > /tmp/Dockerfile.preseed &&
        buildctl --addr unix:///run/buildkit/buildkitd.sock build \
            --frontend dockerfile.v0 \
            --local context=/tmp \
            --local dockerfile=/tmp \
            --opt filename=Dockerfile.preseed \
            --no-cache 2>&1
    "
done

# Step 4: Stop buildkitd and clean up temp files
echo "==> Stopping buildkitd..."
docker exec -u root "$CONTAINER_NAME" sh -c 'kill $(pidof buildkitd) 2>/dev/null; sleep 2'

# Fix ownership so builder user can use the content store at runtime
docker exec -u root "$CONTAINER_NAME" chown -R builder:builder "$BUILDKIT_ROOT"

# Remove temp files and buildkitd lock
docker exec -u root "$CONTAINER_NAME" rm -f /tmp/Dockerfile.preseed /tmp/buildkitd.log
docker exec -u root "$CONTAINER_NAME" rm -f "$BUILDKIT_ROOT/buildkitd.lock"

# Remove /run/buildkit (will be recreated at runtime by rootlesskit)
docker exec -u root "$CONTAINER_NAME" rm -rf /run/buildkit

echo "==> Content store size:"
docker exec "$CONTAINER_NAME" du -sh "$BUILDKIT_ROOT"

# Step 5: Commit the warmed container as the final image
echo "==> Committing pre-seeded image as $TAG..."
docker commit \
    --change 'USER builder' \
    --change 'WORKDIR /src' \
    --change 'ENV BUILDKITD_FLAGS=""' \
    --change 'ENV HOME=/home/builder' \
    --change 'ENV XDG_RUNTIME_DIR=/home/builder/.local/share' \
    --change 'ENTRYPOINT ["/usr/bin/builder-agent"]' \
    "$CONTAINER_NAME" "$TAG"

echo "==> Done! Pre-seeded builder image: $TAG"
echo "    Images pre-seeded: $IMAGES"
docker image inspect "$TAG" --format '    Size: {{.Size}}' | awk '{printf "    Size: %.1fMB\n", $2/1048576}'
