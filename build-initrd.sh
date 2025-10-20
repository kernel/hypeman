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
# Redirect all output (stdout+stderr) to the hypervisor console
set -x

echo "init: start" > /dev/kmsg

# Mount essentials; never fail if already mounted
mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
mount -t devtmpfs devtmpfs /dev 2>/dev/null || mount -t tmpfs tmpfs /dev || true
mkdir -p /dev/pts /dev/shm
mount -t devpts devpts /dev/pts 2>/dev/null || true
chmod 1777 /dev/shm

echo "init: done with mount essentials" > /dev/kmsg

# exec >/dev/console 2>&1
exec </dev/console >/dev/console 2>&1

echo "init: launching wrapper"

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
echo "init: wrapper exited with code $EC"

# If wrapper failed (non-zero), give you a rescue shell instead of reboot/panic
if [ "$EC" -ne 0 ]; then
  echo "init: dropping into interactive shell for debugging..."
  /bin/sh -i
else
  echo "init: wrapper succeeded, sleeping to keep PID1 alive"
  sleep infinity
fi
EOF
chmod +x rootfs/init

# Uncompressed initrd, faster to boot up
# Unikraft also does uncompressed
rm initrd || true
cd rootfs
find . | cpio -H newc -o > ../initrd
