#!/bin/bash
set -e

# Paths
KERNEL_DIR="/var/lib/hypeman/system/kernel/ch-6.12.8-kernel-1-202511182/x86_64"
INITRD_DIR="/var/lib/hypeman/system/initrd/v2.0.2-dev/x86_64"
KERNEL="$KERNEL_DIR/vmlinux"
INITRD="$INITRD_DIR/initrd"
SOCKET="/tmp/repro-vsock.sock"
LOG_FILE="repro-console.log"

# Disk images
ROOTFS_IMG="repro-rootfs.img"
OVERLAY_IMG="repro-overlay.img"
CONFIG_IMG="repro-config.img"

# Clean up
rm -f "$SOCKET" "$LOG_FILE" "$ROOTFS_IMG" "$OVERLAY_IMG" "$CONFIG_IMG"

# Create dummy disks
echo "Creating dummy disks..."
# Rootfs (100M)
dd if=/dev/zero of="$ROOTFS_IMG" bs=1M count=100
mkfs.ext4 -F "$ROOTFS_IMG"

# Overlay (100M)
dd if=/dev/zero of="$OVERLAY_IMG" bs=1M count=100
mkfs.ext4 -F "$OVERLAY_IMG"

# Config (10M)
dd if=/dev/zero of="$CONFIG_IMG" bs=1M count=10
mkfs.ext4 -F "$CONFIG_IMG"
# Add a dummy config.sh
mkdir -p /tmp/repro-config
if sudo mount -o loop "$CONFIG_IMG" /tmp/repro-config; then
    echo "#!/bin/sh" | sudo tee /tmp/repro-config/config.sh
    echo "echo 'Repro config loaded'" | sudo tee -a /tmp/repro-config/config.sh
    echo "export ENTRYPOINT='/bin/sh'" | sudo tee -a /tmp/repro-config/config.sh
    echo "export CMD='-c \"echo Hello from Guest && sleep 3600\"'" | sudo tee -a /tmp/repro-config/config.sh
    sudo chmod +x /tmp/repro-config/config.sh
    sudo umount /tmp/repro-config
else
    echo "Failed to mount config img, proceeding with empty config"
fi

# Check artifacts
if [ ! -f "$KERNEL" ]; then
    echo "Kernel not found at $KERNEL"
    exit 1
fi

if [ ! -f "$INITRD" ]; then
    echo "Initrd not found at $INITRD"
    exit 1
fi

echo "Starting Cloud Hypervisor..."
echo "Kernel: $KERNEL"
echo "Initrd: $INITRD"
echo "Socket: $SOCKET"

# Start Cloud Hypervisor in background
cloud-hypervisor \
    --kernel "$KERNEL" \
    --initramfs "$INITRD" \
    --cmdline "console=ttyS0 panic=1" \
    --disk path="$ROOTFS_IMG",readonly=on path="$OVERLAY_IMG" path="$CONFIG_IMG",readonly=on \
    --cpus boot=1 \
    --memory size=512M \
    --console off \
    --serial tty \
    --vsock cid=3,socket="$SOCKET" \
    > "$LOG_FILE" 2>&1 &

CH_PID=$!
echo "Cloud Hypervisor running with PID $CH_PID"
echo "Logs at $LOG_FILE"
echo "Waiting for VM to boot..."

# Wait for socket
for i in $(seq 1 10); do
    if [ -S "$SOCKET" ]; then
        echo "Socket created."
        break
    fi
    sleep 1
done

if [ ! -S "$SOCKET" ]; then
    echo "Socket creation failed."
    kill $CH_PID
    exit 1
fi

echo "VM should be booting. You can now connect to $SOCKET using socat or Go tools."
echo "Example: sudo socat - UNIX-CONNECT:$SOCKET"
echo "Then type: CONNECT 2222"
echo ""
echo "To stop: kill $CH_PID"

