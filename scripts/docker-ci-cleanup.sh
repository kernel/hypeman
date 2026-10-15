#!/usr/bin/env bash
# Bound Docker disk use on the macOS CI runner while keeping the base images and
# build layers that the next run reuses. Below the free-space floor it runs the
# full prune these jobs used before.
set -euo pipefail

# Free space in GB on the disk the daemon stores images on. Docker runs in a VM
# with its own disk, so the host's free space does not show when that disk fills.
# Measure it from a container. If the measurement fails, prune fully.
min_free_gb="${DOCKER_CI_MIN_FREE_GB:-40}"

free_kb="$(docker run --rm alpine:3.21 df -Pk / 2>/dev/null | awk 'NR == 2 {print $4}')" || free_kb=""
docker system df

if [ -z "$free_kb" ]; then
  echo "daemon free space: unknown; removing all unused images, volumes and build cache"
  docker system prune --all --force --volumes
  exit 0
fi

free_gb=$((free_kb / 1024 / 1024))
echo "daemon free space: ${free_gb}G (floor: ${min_free_gb}G)"

if [ "$free_gb" -lt "$min_free_gb" ]; then
  echo "below floor: removing all unused images, volumes and build cache"
  docker system prune --all --force --volumes
  exit 0
fi

# Untagged images older than a week. Tagged base images (golang, alpine,
# buildkit) are kept, so the next build does not pull them again.
docker image prune --force --filter until=168h
if docker buildx version >/dev/null 2>&1; then
  docker buildx prune --force --filter until=168h
fi
