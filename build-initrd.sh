set -xe

IMAGE='onkernel/chromium-headful:74625c3'

docker pull $IMAGE

cid=$(docker create $IMAGE)
rm -rf rootfs || true
mkdir -p rootfs
docker export "$cid" | tar -C rootfs -xf -
docker rm "$cid"

# Uncompressed initrd, faster to boot up
# Unikraft also does uncompressed
find rootfs | cpio -H newc -o > initrd
