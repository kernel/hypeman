package system

// GenerateInitScript returns the comprehensive init script for initrd
// This consolidates ALL init logic - no modifications to OCI images needed
//
// The script:
// 1. Mounts essential filesystems (proc, sys, dev)
// 2. Sets up overlay filesystem (lowerdir=rootfs, upperdir=overlay disk)
// 3. Mounts and sources config disk (/dev/vdc)
// 4. Loads NVIDIA kernel modules (if HAS_GPU=1 in config.sh)
// 5. Configures networking (if enabled)
// 6. Executes container entrypoint
//
// GPU support: When HAS_GPU=1 is set in the instance's config.sh, the init script
// will load NVIDIA kernel modules before launching the container entrypoint.
func GenerateInitScript() string {
	return `#!/bin/sh
set -xe

echo "overlay-init: START" > /dev/kmsg

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

# Load NVIDIA kernel modules for GPU passthrough (if HAS_GPU=1)
if [ "${HAS_GPU:-0}" = "1" ]; then
  echo "overlay-init: loading NVIDIA kernel modules for GPU passthrough"
  if [ -d /lib/modules ]; then
    # Find the kernel version directory
    KVER=$(ls /lib/modules/ 2>/dev/null | head -1)
    if [ -n "$KVER" ] && [ -d "/lib/modules/$KVER/kernel/drivers/gpu" ]; then
      # Load modules in order (dependencies first)
      insmod /lib/modules/$KVER/kernel/drivers/gpu/nvidia.ko 2>&1 || echo "overlay-init: nvidia.ko load failed"
      insmod /lib/modules/$KVER/kernel/drivers/gpu/nvidia-uvm.ko 2>&1 || echo "overlay-init: nvidia-uvm.ko load failed"
      insmod /lib/modules/$KVER/kernel/drivers/gpu/nvidia-modeset.ko 2>&1 || echo "overlay-init: nvidia-modeset.ko load failed"
      insmod /lib/modules/$KVER/kernel/drivers/gpu/nvidia-drm.ko modeset=1 2>&1 || echo "overlay-init: nvidia-drm.ko load failed"
      echo "overlay-init: NVIDIA modules loaded for kernel $KVER"
      
      # Use nvidia-modprobe to create device nodes with correct major/minor numbers.
      # nvidia-modprobe is the official NVIDIA utility that:
      # 1. Loads kernel modules if needed (already done above)
      # 2. Creates /dev/nvidiactl and /dev/nvidia0 with correct permissions
      # 3. Creates /dev/nvidia-uvm and /dev/nvidia-uvm-tools
      if [ -x /usr/bin/nvidia-modprobe ]; then
        echo "overlay-init: running nvidia-modprobe to create device nodes"
        /usr/bin/nvidia-modprobe 2>&1 || echo "overlay-init: nvidia-modprobe failed"
        /usr/bin/nvidia-modprobe -u -c=0 2>&1 || echo "overlay-init: nvidia-modprobe -u failed"
        echo "overlay-init: nvidia-modprobe completed"
        ls -la /dev/nvidia* 2>/dev/null || true
      else
        echo "overlay-init: nvidia-modprobe not found, falling back to manual mknod"
        # Fallback: Manual device node creation
        NVIDIA_MAJOR=$(awk '/nvidia-frontend|^[0-9]+ nvidia$/ {print $1}' /proc/devices 2>/dev/null | head -1)
        NVIDIA_UVM_MAJOR=$(awk '/nvidia-uvm/ {print $1}' /proc/devices 2>/dev/null)
        
        if [ -n "$NVIDIA_MAJOR" ]; then
          mknod -m 666 /dev/nvidiactl c $NVIDIA_MAJOR 255
          mknod -m 666 /dev/nvidia0 c $NVIDIA_MAJOR 0
          echo "overlay-init: created /dev/nvidiactl and /dev/nvidia0 (major $NVIDIA_MAJOR)"
        fi
        
        if [ -n "$NVIDIA_UVM_MAJOR" ]; then
          mknod -m 666 /dev/nvidia-uvm c $NVIDIA_UVM_MAJOR 0
          mknod -m 666 /dev/nvidia-uvm-tools c $NVIDIA_UVM_MAJOR 1
          echo "overlay-init: created /dev/nvidia-uvm* (major $NVIDIA_UVM_MAJOR)"
        fi
      fi
    else
      echo "overlay-init: NVIDIA modules not found in /lib/modules/$KVER"
    fi
  else
    echo "overlay-init: /lib/modules not found, skipping NVIDIA module loading"
  fi
  
  # Inject NVIDIA userspace driver libraries into container rootfs
  # This allows containers to use standard CUDA images without bundled drivers
  # See lib/devices/GPU.md for documentation
  if [ -d /usr/lib/nvidia ]; then
    echo "overlay-init: injecting NVIDIA driver libraries into container"
    
    DRIVER_VERSION=$(cat /usr/lib/nvidia/version 2>/dev/null || echo "unknown")
    LIB_DST="/overlay/newroot/usr/lib/x86_64-linux-gnu"
    BIN_DST="/overlay/newroot/usr/bin"
    
    mkdir -p "$LIB_DST" "$BIN_DST"
    
    # Copy all driver libraries and create symlinks
    for lib in /usr/lib/nvidia/*.so.*; do
      if [ -f "$lib" ]; then
        libname=$(basename "$lib")
        cp "$lib" "$LIB_DST/"
        
        # Create standard symlinks: libfoo.so.VERSION -> libfoo.so.1 -> libfoo.so
        base=$(echo "$libname" | sed 's/\.so\..*//')
        ln -sf "$libname" "$LIB_DST/${base}.so.1" 2>/dev/null || true
        ln -sf "${base}.so.1" "$LIB_DST/${base}.so" 2>/dev/null || true
      fi
    done
    
    # Copy nvidia-smi and nvidia-modprobe binaries
    for bin in nvidia-smi nvidia-modprobe; do
      if [ -x /usr/bin/$bin ]; then
        cp /usr/bin/$bin "$BIN_DST/"
      fi
    done
    
    # Update ldconfig cache so applications can find the libraries
    chroot /overlay/newroot ldconfig 2>/dev/null || true
    
    echo "overlay-init: NVIDIA driver libraries injected (version: $DRIVER_VERSION)"
  fi
fi

# Mount attached volumes (from config: VOLUME_MOUNTS="device:path:mode[:overlay_device] ...")
# Modes: ro (read-only), rw (read-write), overlay (base ro + per-instance overlay)
if [ -n "${VOLUME_MOUNTS:-}" ]; then
  echo "overlay-init: mounting volumes"
  for vol in $VOLUME_MOUNTS; do
    device=$(echo "$vol" | cut -d: -f1)
    path=$(echo "$vol" | cut -d: -f2)
    mode=$(echo "$vol" | cut -d: -f3)
    
    # Create mount point in overlay
    mkdir -p "/overlay/newroot${path}"
    
    if [ "$mode" = "overlay" ]; then
      # Overlay mode: mount base read-only, create overlayfs with per-instance writable layer
      overlay_device=$(echo "$vol" | cut -d: -f4)
      
      # Create temp mount points for base and overlay disk.
      # These persist for the lifetime of the VM but are NOT leaked - they exist inside
      # the ephemeral guest rootfs (which is itself an overlayfs) and are destroyed
      # when the VM terminates along with all guest state.
      base_mount="/mnt/vol-base-$(basename "$path")"
      overlay_mount="/mnt/vol-overlay-$(basename "$path")"
      mkdir -p "$base_mount" "$overlay_mount"
      
      # Mount base volume read-only (noload to skip journal recovery)
      mount -t ext4 -o ro,noload "$device" "$base_mount"
      
      # Mount overlay disk (writable)
      mount -t ext4 "$overlay_device" "$overlay_mount"
      mkdir -p "$overlay_mount/upper" "$overlay_mount/work"
      
      # Create overlayfs combining base (lower) and instance overlay (upper)
      mount -t overlay \
        -o "lowerdir=$base_mount,upperdir=$overlay_mount/upper,workdir=$overlay_mount/work" \
        overlay "/overlay/newroot${path}"
      
      echo "overlay-init: mounted volume $device at $path (overlay via $overlay_device)"
    elif [ "$mode" = "ro" ]; then
      # Read-only mount (noload to skip journal recovery for multi-attach safety)
      mount -t ext4 -o ro,noload "$device" "/overlay/newroot${path}"
      echo "overlay-init: mounted volume $device at $path (ro)"
    else
      # Read-write mount
      mount -t ext4 "$device" "/overlay/newroot${path}"
      echo "overlay-init: mounted volume $device at $path (rw)"
    fi
  done
fi

# Prepare new root mount points
# We use bind mounts instead of move so that the original /dev remains populated
# for processes running in the initrd namespace (like exec-agent).
mkdir -p /overlay/newroot/proc
mkdir -p /overlay/newroot/sys
mkdir -p /overlay/newroot/dev
mkdir -p /overlay/newroot/dev/pts

mount --bind /proc /overlay/newroot/proc
mount --bind /sys /overlay/newroot/sys
mount --bind /dev /overlay/newroot/dev
mount --bind /dev/pts /overlay/newroot/dev/pts

echo "overlay-init: bound mounts to new root"

# Set up /dev symlinks for process substitution inside the container
chroot /overlay/newroot ln -sf /proc/self/fd /dev/fd 2>/dev/null || true
chroot /overlay/newroot ln -sf /proc/self/fd/0 /dev/stdin 2>/dev/null || true
chroot /overlay/newroot ln -sf /proc/self/fd/1 /dev/stdout 2>/dev/null || true
chroot /overlay/newroot ln -sf /proc/self/fd/2 /dev/stderr 2>/dev/null || true

# Configure network from initrd (using busybox ip, not container's ip)
# Network interfaces are shared, so we can configure them from here
if [ -n "${GUEST_IP:-}" ]; then
  echo "overlay-init: configuring network"
  ip link set lo up
  ip addr add ${GUEST_IP}/${GUEST_CIDR} dev eth0
  ip link set eth0 up
  ip route add default via ${GUEST_GW}
  echo "nameserver ${GUEST_DNS}" > /overlay/newroot/etc/resolv.conf
  echo "overlay-init: network configured - IP: ${GUEST_IP}/${GUEST_CIDR}"
fi

# Set PATH for initrd tools
export PATH='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
export HOME='/root'

# Copy exec-agent into container rootfs and start it in container namespace
# This way the PTY and shell run in the same namespace, fixing signal handling
echo "overlay-init: copying exec-agent to container"
mkdir -p /overlay/newroot/usr/local/bin
cp /usr/local/bin/exec-agent /overlay/newroot/usr/local/bin/exec-agent

# Start vsock exec agent inside the container namespace
echo "overlay-init: starting exec agent in container namespace"
chroot /overlay/newroot /usr/local/bin/exec-agent &

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

# Wait for all background jobs (exec-agent runs forever, keeping init alive)
# This prevents kernel panic from killing init (PID 1)
wait`
}
