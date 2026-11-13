#!/bin/bash
# Build a 64 MB ext4 rootfs whose only content is a static /sbin/init built from the given C file.
# Usage: build-rootfs.sh <out.ext4> <init.c>
set -eu
export PATH="/usr/local/sbin:/usr/sbin:/sbin:$PATH"
OUT=${1:?usage: build-rootfs.sh <out.ext4> <init.c>}
INIT_SRC=${2:?usage: build-rootfs.sh <out.ext4> <init.c>}
SRC=$(mktemp -d)
trap 'rm -rf "$SRC"' EXIT

mkdir -p "$SRC/sbin" "$SRC/dev" # devtmpfs mounts on /dev; the read-only root needs the directory
gcc -static -O2 -o "$SRC/sbin/init" "$INIT_SRC"
truncate -s 64M "$OUT"
mkfs.ext4 -q -F -d "$SRC" "$OUT"
