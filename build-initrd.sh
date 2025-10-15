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

exec >/dev/console 2>&1

echo "init: launching wrapper"
/wrapper.sh
EC=$?
echo "init: wrapper exited with code $EC"

# keep PID1 alive so logs stay visible
sleep infinity
EOF
chmod +x rootfs/init

# Uncompressed initrd, faster to boot up
# Unikraft also does uncompressed
rm initrd || true
cd rootfs
find . | cpio -H newc -o > ../initrd
