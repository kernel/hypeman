set -xe

IMAGE='onkernel/chromium-headful-test:latest'
DIR=$(pwd)

cd ~/kernel-images-private/images/chromium-headful
./build-docker.sh
cd $DIR

# docker pull $IMAGE

cid=$(docker create $IMAGE)
rm -rf rootfs || true
mkdir -p rootfs
docker export "$cid" | tar -C rootfs -xf -
docker rm "$cid"

# NOTE: Is wrapper running as PID 1 in unikraft?
# Or maybe not? I was having trouble starting the
# wrapper as PID 1.
cat > rootfs/init <<'EOF'
#!/bin/sh
set -x

echo "init: start" > /dev/kmsg

# All mounts are handled by overlay init - skip them entirely

# Redirect stdout/stderr to console for all subsequent commands
exec >/dev/console 2>&1

echo "init: launching wrapper" > /dev/kmsg

# TODO: envs should not happen during build
# Hardcoded environment variables
export CHROMIUM_FLAGS="--disable-dev-shm-usage --disable-features=AcceptCHFrame,AutoExpandDetailsElement,AvoidUnnecessaryBeforeUnloadCheckSync,CertificateTransparencyComponentUpdater,DestroyProfileOnBrowserClose,DialMediaRouteProvider,GlobalMediaControls,HttpsUpgrades,ImprovedCookieControls,IsolateOrigins,LazyFrameLoading,MediaRouter,PaintHolding,PasswordManagerEnabled,PlzDedicatedWorker,site-per-process,Translate --disable-gpu --no-sandbox --no-zygote --remote-allow-origins=* --start-maximized --test-type"
export DISPLAY_NUM="1"
export ENABLE_WEBRTC="true"
export HEIGHT="768"
export HOME="/"
# export NEKO_ICESERVERS=...
export RUN_AS_ROOT="true"
export WIDTH="1024"
export WITH_KERNEL_IMAGES_API="true"

# TODO: network config should not be hardcoded in build
ip link set lo up
ifconfig eth0 192.168.100.10 netmask 255.255.255.0 up
route add default gw 192.168.100.1
echo "nameserver 1.1.1.1" > /etc/resolv.conf

# Need to set these envs for the wrapper to work right
whoami
export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
export HOME='/root'
/wrapper.sh
EC=$?
echo "init: wrapper exited with code $EC" > /dev/kmsg

# If wrapper failed (non-zero), give you a rescue shell instead of reboot/panic
if [ "$EC" -ne 0 ]; then
  echo "init: dropping into interactive shell for debugging..." > /dev/kmsg
  /bin/sh -i
else
  echo "init: wrapper succeeded, sleeping to keep PID1 alive" > /dev/kmsg
  sleep infinity
fi
EOF
chmod +x rootfs/init

# Create disk image from rootfs instead of initrd
DISK_SIZE="4G"  # Adjust size as needed
rm -f rootfs.ext4 || true
truncate -s $DISK_SIZE rootfs.ext4
mkfs.ext4 -d rootfs rootfs.ext4 -F

echo "Created rootfs.ext4 disk image ($(du -h rootfs.ext4 | cut -f1))"

# ============================================
# Build minimal initramfs from busybox for overlay setup
# ============================================

echo "Building initramfs from busybox..."
busybox_cid=$(docker create busybox:latest)
rm -rf initramfs-overlay || true
mkdir -p initramfs-overlay
docker export "$busybox_cid" | tar -C initramfs-overlay -xf -
docker rm "$busybox_cid"

# Create overlay init script that sets up readonly disk + tmpfs overlay
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

# Mount readonly base filesystem from disk
mkdir -p /lower
mount -o ro /dev/vda /lower

echo "overlay-init: mounted readonly rootfs from /dev/vda" > /dev/kmsg

# Create memory overlay (using 2G, adjust as needed)
mount -t tmpfs -o size=2G none /tmp
mkdir -p /tmp/upper /tmp/work /tmp/newroot

mount -t overlay \
  -o lowerdir=/lower,upperdir=/tmp/upper,workdir=/tmp/work \
  overlay /tmp/newroot

echo "overlay-init: created tmpfs overlay" > /dev/kmsg

# Move mounts to new root (all properly set up now)
cd /tmp/newroot
mkdir -p proc sys dev
mount --move /proc proc
mount --move /sys sys
mount --move /dev dev

echo "overlay-init: switching root to overlay" > /dev/kmsg

# Switch to overlay root and run the app init
exec switch_root . /init </dev/console >/dev/console 2>&1
EOF

chmod +x initramfs-overlay/init

# Package as initramfs
rm -f initrd || true
cd initramfs-overlay
find . | cpio -H newc -o > ../initrd
cd ..

echo "Created initrd from busybox ($(du -h initrd | cut -f1))"
