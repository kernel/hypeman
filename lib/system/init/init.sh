#!/bin/sh
# Minimal init wrapper that sets up environment before running Go init
# The Go runtime needs /proc and /dev to exist during initialization
#
# This pattern is used by other Go-based init systems:
# - u-root (github.com/u-root/u-root) - uses assembly stub for early mount
# - LinuxKit (github.com/linuxkit/linuxkit) - similar shell wrapper approach
# - gokrazy (github.com/gokrazy/gokrazy) - mounts filesystems before Go starts

# Mount essential filesystems BEFORE running Go binary
mkdir -p /proc /sys /dev
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev

# Now exec the Go init binary (it will take over as PID 1)
exec /init.bin "$@"
