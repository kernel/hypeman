#!/usr/bin/env bash
set -euo pipefail

IMAGE='onkernel/chromium-headful-test:latest'
KERNEL_VERSION='ch-release-v6.12.8-20250613'
DIR=$(pwd)

echo "========================================"
echo "Building Cloud Hypervisor POC Images"
echo "========================================"

# ============================================
# Download kernel if not present
# ============================================
if [ ! -f "vmlinux" ]; then
  echo "[INFO] Downloading kernel $KERNEL_VERSION..."
  wget -q https://github.com/cloud-hypervisor/linux/releases/download/$KERNEL_VERSION/vmlinux-x86_64 -O vmlinux
  echo "[INFO] Kernel downloaded successfully"
else
  echo "[INFO] Kernel already exists, skipping download"
fi

# ============================================
# Build Docker image
# ============================================
echo "[INFO] Building Docker image..."
cd ~/kernel-images-private/images/chromium-headful
./build-docker.sh
cd "$DIR"

# ============================================
# Extract rootfs from Docker image
# ============================================
echo "[INFO] Extracting rootfs from Docker image..."
cid=$(docker create $IMAGE)
rm -rf rootfs || true
mkdir -p rootfs
docker export "$cid" | tar -C rootfs -xf -

# Save metadata
echo "[INFO] Saving Docker metadata..."
docker inspect "$cid" > /tmp/docker-metadata.json
docker rm "$cid"

# ============================================
# Generate rootfs init script with config disk support
# ============================================
echo "[INFO] Generating rootfs init script..."
cat > rootfs/init <<'EOF'
#!/bin/sh
set -x

echo "init: start" > /dev/kmsg

# All mounts are handled by overlay init - skip them entirely

# Redirect stdout/stderr to serial console for logging
exec >/dev/ttyS0 2>&1

echo "init: mounting config disk" > /dev/kmsg

# Mount config disk and source configuration
mkdir -p /mnt/config
mount -o ro /dev/vdc /mnt/config

if [ -f /mnt/config/config.sh ]; then
  echo "init: sourcing config from /mnt/config/config.sh" > /dev/kmsg
  . /mnt/config/config.sh
else
  echo "init: ERROR - config.sh not found on config disk!" > /dev/kmsg
  /bin/sh -i
  exit 1
fi

echo "init: configuring network" > /dev/kmsg

# Configure network from config variables
ip link set lo up
ifconfig eth0 ${GUEST_IP} netmask ${GUEST_MASK} up
route add default gw ${GUEST_GW}
echo "nameserver ${GUEST_DNS}" > /etc/resolv.conf

echo "init: network configured - IP: ${GUEST_IP}" > /dev/kmsg

# Set PATH for proper binary resolution
export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
export HOME='/root'

echo "init: starting SSH server" > /dev/kmsg

# Create SSH directory
mkdir -p /var/run/sshd
mkdir -p /root/.ssh
chmod 700 /root/.ssh

# Set root password (can be overridden in config)
# POC ONLY
echo "root:root" | chpasswd

# Generate host keys if they don't exist
if [ ! -f /etc/ssh/ssh_host_rsa_key ]; then
  ssh-keygen -A
fi

# Start SSH daemon
/usr/sbin/sshd

echo "init: SSH server started" > /dev/kmsg
echo "init: launching entrypoint from ${WORKDIR:-/}" > /dev/kmsg
echo "init: entrypoint=${ENTRYPOINT} cmd=${CMD}" > /dev/kmsg

# Change to workdir (default to / if empty)
cd ${WORKDIR:-/}

# Execute entrypoint with cmd as arguments (like Docker does)
# Using exec replaces this shell with the entrypoint, making it PID 1
# When it exits, the VM will stop (just like a Docker container)
exec ${ENTRYPOINT} ${CMD}
EOF
chmod +x rootfs/init

# ============================================
# Create rootfs disk image
# ============================================
echo "[INFO] Creating rootfs disk image..."
DISK_SIZE="4G"
rm -f rootfs.ext4 || true
truncate -s $DISK_SIZE rootfs.ext4
mkfs.ext4 -d rootfs rootfs.ext4 -F -q

echo "[INFO] Created rootfs.ext4 disk image ($(du -h rootfs.ext4 | cut -f1))"

# ============================================
# Build minimal initramfs from busybox for overlay setup
# ============================================
echo "[INFO] Building initramfs from busybox..."
busybox_cid=$(docker create busybox:latest)
rm -rf initramfs-overlay || true
mkdir -p initramfs-overlay
docker export "$busybox_cid" | tar -C initramfs-overlay -xf -
docker rm "$busybox_cid"

# Create overlay init script with disk-based overlay and config disk support
cat > initramfs-overlay/init <<'EOF'
#!/bin/sh
set -xe
echo "overlay-init: start" > /dev/kmsg

# Mount essentials
mount -t proc none /proc
mount -t sysfs none /sys  
mount -t devtmpfs none /dev

# Setup /dev properly BEFORE moving it
mkdir -p /dev/pts /dev/shm
mount -t devpts devpts /dev/pts
chmod 1777 /dev/shm

echo "overlay-init: mounted proc/sys/dev with pts/shm" > /dev/kmsg

# Mount readonly base filesystem from disk (/dev/vda)
mkdir -p /lower
mount -o ro /dev/vda /lower

echo "overlay-init: mounted readonly rootfs from /dev/vda" > /dev/kmsg

# Mount writable overlay disk from /dev/vdb (disk-based for faster restore)
mkdir -p /overlay
mount -t ext4 /dev/vdb /overlay

echo "overlay-init: mounted writable overlay disk from /dev/vdb" > /dev/kmsg

# Prepare overlay directories on the disk
mkdir -p /overlay/upper /overlay/work /overlay/newroot

# Build overlay filesystem
mount -t overlay \
  -o lowerdir=/lower,upperdir=/overlay/upper,workdir=/overlay/work \
  overlay /overlay/newroot

echo "overlay-init: created disk-based overlay" > /dev/kmsg

# Move mounts to new root
cd /overlay/newroot
mkdir -p proc sys dev
mount --move /proc proc
mount --move /sys sys
mount --move /dev dev

echo "overlay-init: switching root to overlay" > /dev/kmsg

# Switch to overlay root and run the app init
# Note: /dev/vdc (config disk) will be mounted by the rootfs init
# Don't redirect here - let the new init handle console setup
exec switch_root . /init
EOF

chmod +x initramfs-overlay/init

# Package as initramfs
echo "[INFO] Packaging initramfs..."
rm -f initrd || true
cd initramfs-overlay
find . | cpio -H newc -o 2>/dev/null > ../initrd
cd ..

echo "[INFO] Created initrd from busybox ($(du -h initrd | cut -f1))"

# ============================================
# Create data directory structure
# ============================================
echo "[INFO] Creating data directory structure..."
mkdir -p data/system
mkdir -p data/images/chromium-headful/v1

# Copy artifacts to data directory
echo "[INFO] Copying artifacts to data/..."
cp vmlinux data/system/
cp initrd data/system/
cp rootfs.ext4 data/images/chromium-headful/v1/
cp /tmp/docker-metadata.json data/images/chromium-headful/v1/metadata.json

echo ""
echo "========================================"
echo "Build Complete!"
echo "========================================"
echo "Artifacts created in data/:"
echo "  - data/system/vmlinux"
echo "  - data/system/initrd"
echo "  - data/images/chromium-headful/v1/rootfs.ext4"
echo "  - data/images/chromium-headful/v1/metadata.json"
echo ""
echo "Next steps:"
echo "  1. ./scripts/setup-host-network.sh  # Configure host networking"
echo "  2. ./scripts/setup-vms.sh           # Create 10 VM configs"
echo "  3. ./scripts/start-all-vms.sh       # Start all VMs"
echo "========================================"
