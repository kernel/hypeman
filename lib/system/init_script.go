package system

// GenerateInitScript returns the comprehensive init script for initrd
// This consolidates ALL init logic - no modifications to OCI images needed
//
// The script:
// 1. Mounts essential filesystems (proc, sys, dev)
// 2. Sets up overlay filesystem (lowerdir=rootfs, upperdir=overlay disk)
// 3. Mounts and sources config disk (/dev/vdc)
// 4. Configures networking (if enabled)
// 5. Executes container entrypoint
func GenerateInitScript(version InitrdVersion) string {
	return `#!/bin/sh
set -xe

echo "overlay-init: START (` + string(version) + `)" > /dev/kmsg

# Create mount points
mkdir -p /proc /sys /dev

# Mount essential filesystems
# devtmpfs handles /dev population (null, zero, vsock, etc.) automatically
mount -t proc none /proc
mount -t sysfs none /sys  
mount -t devtmpfs none /dev

# Setup PTY support (needed for exec-agent and interactive shells)
mkdir -p /dev/pts /dev/shm
mount -t devpts devpts /dev/pts
chmod 1777 /dev/shm

echo "overlay-init: mounted proc/sys/dev" > /dev/kmsg

# Redirect all output to serial console
exec >/dev/ttyS0 2>&1

echo "overlay-init: redirected to serial console"

# Wait for block devices to be ready
sleep 0.5

# Mount readonly rootfs from /dev/vda (ext4 filesystem)
mkdir -p /lower
mount -t ext4 -o ro /dev/vda /lower
echo "overlay-init: mounted rootfs from /dev/vda"

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

# Prepare new root mount points
# We use bind mounts instead of move so that the original /dev remains populated
# for processes running in the initrd namespace (like exec-agent).
mkdir -p /overlay/newroot/proc
mkdir -p /overlay/newroot/sys
mkdir -p /overlay/newroot/dev

mount --bind /proc /overlay/newroot/proc
mount --bind /sys /overlay/newroot/sys
mount --bind /dev /overlay/newroot/dev

echo "overlay-init: bound mounts to new root"

# Set up /dev symlinks for process substitution inside the container
chroot /overlay/newroot ln -sf /proc/self/fd /dev/fd 2>/dev/null || true
chroot /overlay/newroot ln -sf /proc/self/fd/0 /dev/stdin 2>/dev/null || true
chroot /overlay/newroot ln -sf /proc/self/fd/1 /dev/stdout 2>/dev/null || true
chroot /overlay/newroot ln -sf /proc/self/fd/2 /dev/stderr 2>/dev/null || true

# Configure network inside the container view
if [ -n "${GUEST_IP:-}" ]; then
  echo "overlay-init: configuring network"
  chroot /overlay/newroot ip link set lo up
  chroot /overlay/newroot ip addr add ${GUEST_IP}/${GUEST_MASK} dev eth0
  chroot /overlay/newroot ip link set eth0 up
  chroot /overlay/newroot ip route add default via ${GUEST_GW}
  echo "nameserver ${GUEST_DNS}" > /overlay/newroot/etc/resolv.conf
  echo "overlay-init: network configured - IP: ${GUEST_IP}"
fi

# Set PATH for initrd tools
export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
export HOME='/root'

# Start vsock exec agent
# It runs in the initrd namespace but can chroot as needed (or commands run in initrd)
echo "overlay-init: starting exec agent"
/usr/local/bin/exec-agent &

echo "overlay-init: launching entrypoint"
echo "overlay-init: workdir=${WORKDIR:-/} entrypoint=${ENTRYPOINT} cmd=${CMD}"

set +e

# Construct the command string carefully
# ENTRYPOINT and CMD are shell-safe quoted strings from config.sh
eval "chroot /overlay/newroot /bin/sh -c \"cd ${WORKDIR:-/} && exec ${ENTRYPOINT} ${CMD}\"" &
APP_PID=$!

echo "overlay-init: container app started (PID $APP_PID)"

# Wait for app to exit
wait $APP_PID
APP_EXIT=$?

echo "overlay-init: app exited with code $APP_EXIT"

	# Keep VM alive if app crashes, to allow debugging via exec-agent
	if [ $APP_EXIT -ne 0 ]; then
		echo "overlay-init: CRITICAL - App exited with error."
	fi

	# FALLBACK: Launch interactive shell on console
	# This allows manual debugging if attaching to the console
	echo "overlay-init: Launching interactive shell on ttyS0..."
	setsid cttyhack /bin/sh < /dev/ttyS0 > /dev/ttyS0 2>&1

	# If shell exits (it shouldn't), loop forever
	while true; do sleep 3600; done

	exit $APP_EXIT
	`
}

