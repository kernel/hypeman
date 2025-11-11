package system

// generateInitScript returns the comprehensive init script for initrd
// This consolidates ALL init logic - no modifications to OCI images needed
//
// The script:
// 1. Mounts essential filesystems (proc, sys, dev)
// 2. Sets up overlay filesystem (lowerdir=rootfs, upperdir=overlay disk)
// 3. Mounts and sources config disk (/dev/vdc)
// 4. Configures networking (if enabled)
// 5. Executes container entrypoint
func generateInitScript(version InitrdVersion) string {
	return `#!/bin/sh
set -xe

echo "overlay-init: start (` + string(version) + `)" > /dev/kmsg

# Mount essential filesystems
mount -t proc none /proc
mount -t sysfs none /sys  
mount -t devtmpfs none /dev
mkdir -p /dev/pts /dev/shm
mount -t devpts devpts /dev/pts
chmod 1777 /dev/shm

echo "overlay-init: mounted proc/sys/dev" > /dev/kmsg

# Redirect all output to serial console
exec >/dev/ttyS0 2>&1

echo "overlay-init: redirected to serial console"

# Mount readonly rootfs from /dev/vda
mkdir -p /lower
mount -o ro /dev/vda /lower
echo "overlay-init: mounted readonly rootfs from /dev/vda"

# Mount writable overlay disk from /dev/vdb
mkdir -p /overlay
mount -t ext4 /dev/vdb /overlay
mkdir -p /overlay/upper /overlay/work /overlay/newroot
echo "overlay-init: mounted overlay disk from /dev/vdb"

# Create overlay filesystem
mount -t overlay \
  -o lowerdir=/lower,upperdir=/overlay/upper,workdir=/overlay/work \
  overlay /overlay/newroot
echo "overlay-init: created overlay filesystem"

# Mount config disk (/dev/vdc)
mkdir -p /mnt/config
mount -o ro /dev/vdc /mnt/config
echo "overlay-init: mounted config disk"

# Source configuration
if [ -f /mnt/config/config.sh ]; then
  . /mnt/config/config.sh
  echo "overlay-init: sourced config"
else
  echo "overlay-init: ERROR - config.sh not found!"
  /bin/sh -i
  exit 1
fi

# Move mounts to new root before chroot
cd /overlay/newroot
mkdir -p proc sys dev mnt
mount --move /proc proc
mount --move /sys sys
mount --move /dev dev
mount --move /mnt mnt

echo "overlay-init: moved mounts to new root"

# Set up /dev symlinks for process substitution (Docker compatibility)
chroot . ln -sf /proc/self/fd /dev/fd 2>/dev/null || true
chroot . ln -sf /proc/self/fd/0 /dev/stdin 2>/dev/null || true
chroot . ln -sf /proc/self/fd/1 /dev/stdout 2>/dev/null || true
chroot . ln -sf /proc/self/fd/2 /dev/stderr 2>/dev/null || true

# Configure network (if GUEST_IP is set)
if [ -n "${GUEST_IP:-}" ]; then
  echo "overlay-init: configuring network"
  chroot . ip link set lo up
  chroot . ip addr add ${GUEST_IP}/${GUEST_MASK} dev eth0
  chroot . ip link set eth0 up
  chroot . ip route add default via ${GUEST_GW}
  echo "nameserver ${GUEST_DNS}" > etc/resolv.conf
  echo "overlay-init: network configured - IP: ${GUEST_IP}"
fi

# Set PATH for proper binary resolution
export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
export HOME='/root'

echo "overlay-init: launching entrypoint"
echo "overlay-init: workdir=${WORKDIR:-/} entrypoint=${ENTRYPOINT} cmd=${CMD}"

# Change to workdir
cd ${WORKDIR:-/}

# Execute entrypoint with cmd as arguments
# Using chroot + exec to run as PID 1 in the new root
# When it exits, the VM stops (like Docker containers)
exec chroot /overlay/newroot ${ENTRYPOINT} ${CMD}
`
}

