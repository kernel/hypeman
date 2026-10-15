#!/usr/bin/env bash
# Bound Docker disk use on the macOS CI runner while keeping the base images and
# build layers that the next run reuses. Below the free-space floor it runs the
# full prune these jobs used before.
set -euo pipefail

# Host free space in GB below which everything unused is removed. The runner's
# disk size is not recorded in this repo; tune this from the logged free space.
min_free_gb="${DOCKER_CI_MIN_FREE_GB:-40}"

free_gb="$(df -g / | awk 'NR == 2 {print $4}')"
echo "host free space: ${free_gb}G (floor: ${min_free_gb}G)"
docker system df

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
