#!/usr/bin/env bash
set -euo pipefail

# Configuration
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/data"

# Check arguments
if [ $# -lt 1 ]; then
  echo "Usage: $0 <vm-id> [--compress]"
  echo "Example: $0 5"
  echo "Example: $0 5 --compress"
  echo ""
  echo "VM ID should be 1-10"
  echo "Options:"
  echo "  --compress  Compress snapshot with LZ4 (slower snapshot, faster restore on slow disks)"
  exit 1
fi

VM_NUM="$1"
COMPRESS_FLAG="${2:-}"

# Validate VM number
if ! [[ "$VM_NUM" =~ ^[0-9]+$ ]] || [ "$VM_NUM" -lt 1 ] || [ "$VM_NUM" -gt 10 ]; then
  echo "[ERROR] Invalid VM ID: $VM_NUM"
  echo "VM ID must be between 1 and 10"
  exit 1
fi

VM_ID="guest-$VM_NUM"
VM_DIR="$BASE_DIR/guests/$VM_ID"
SOCKET="$VM_DIR/ch.sock"
SNAPSHOT_DIR="$VM_DIR/snapshots/snapshot-latest"

echo "========================================"
echo "Standby VM: $VM_ID"
echo "========================================"

# Start overall timing
STANDBY_START=$(date +%s%3N)

# Check if VM directory exists
if [ ! -d "$VM_DIR" ]; then
  echo "[ERROR] VM directory not found: $VM_DIR"
  echo "Please run ./scripts/setup-vms.sh first."
  exit 1
fi

# Check if VM is running
if [ ! -S "$SOCKET" ]; then
  echo "[ERROR] VM $VM_ID is not running (socket not found)"
  exit 1
fi

# Check if we can communicate with the VM
if ! sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
  echo "[ERROR] Cannot communicate with VM $VM_ID"
  echo "Socket exists but VM is not responding"
  exit 1
fi

# Create snapshot directory
echo "[INFO] Preparing snapshot directory..."
rm -rf "$SNAPSHOT_DIR" || true
mkdir -p "$SNAPSHOT_DIR"

# Try to reduce memory before snapshot (virtio-mem hot-unplug)
echo "[INFO] Attempting to reduce memory to 1GB for snapshot..."
MEMORY_RESIZE_START=$(date +%s%3N)
TARGET_SIZE="1073741824"  # 1GB in bytes

# Resize memory down to 1GB (virtio-mem will unplug what it can safely)
RESIZE_OUTPUT=$(sudo ch-remote --api-socket "$SOCKET" resize --memory "$TARGET_SIZE" 2>&1) || true

MEMORY_RESIZE_END=$(date +%s%3N)
MEMORY_RESIZE_TIME=$((MEMORY_RESIZE_END - MEMORY_RESIZE_START))

# Check what was actually achieved
ACTUAL_SIZE=$(sudo ch-remote --api-socket "$SOCKET" info 2>/dev/null | jq -r '.config.memory.size' || echo "unknown")
ACTUAL_SIZE_MB=$((ACTUAL_SIZE / 1048576))

echo "[INFO] Memory resize completed in $((MEMORY_RESIZE_TIME))ms"
echo "[INFO] Memory size: ${ACTUAL_SIZE_MB}MB (target was 1024MB)"

if [ "$ACTUAL_SIZE_MB" -gt 1024 ]; then
  echo "[WARN] Could not reduce to target - guest using more than 1GB"
  echo "[WARN] Snapshot will be larger than optimal"
fi

# TODO: why do we need this sleep?
# For some reason, if I immediately pause the VM then snapshot,
# the memory isn't yet reduced to 1GB in terms of snapshot size.
sleep 2

# Pause the VM
echo "[INFO] Pausing VM..."
PAUSE_START=$(date +%s%3N)
if ! sudo ch-remote --api-socket "$SOCKET" pause; then
  echo "[ERROR] Failed to pause VM"
  exit 1
fi
PAUSE_END=$(date +%s%3N)
PAUSE_TIME=$((PAUSE_END - PAUSE_START))
echo "[INFO] VM paused successfully ($((PAUSE_TIME))ms)"

# Create snapshot
echo "[INFO] Creating snapshot..."
SNAPSHOT_START=$(date +%s%3N)
SNAPSHOT_URL="file://$SNAPSHOT_DIR"
if ! sudo ch-remote --api-socket "$SOCKET" snapshot "$SNAPSHOT_URL"; then
  echo "[ERROR] Failed to create snapshot"
  echo "[INFO] Attempting to resume VM..."
  sudo ch-remote --api-socket "$SOCKET" resume || true
  exit 1
fi
SNAPSHOT_END=$(date +%s%3N)
SNAPSHOT_TIME=$((SNAPSHOT_END - SNAPSHOT_START))
echo "[INFO] Snapshot created successfully ($((SNAPSHOT_TIME))ms)"

# Optionally compress memory-ranges (only if --compress flag provided)
COMPRESS_TIME=0
ORIGINAL_SIZE_MB=0
COMPRESSED_SIZE_MB=0
RATIO="1.0"

if [ "$COMPRESS_FLAG" = "--compress" ]; then
  echo "[INFO] Compressing snapshot with LZ4 (fast mode)..."
  COMPRESS_START=$(date +%s%3N)
  MEMORY_FILE="$SNAPSHOT_DIR/memory-ranges"

  if [ -f "$MEMORY_FILE" ]; then
    # Get original size
    ORIGINAL_SIZE=$(sudo stat -c%s "$MEMORY_FILE")
    ORIGINAL_SIZE_MB=$((ORIGINAL_SIZE / 1048576))
    
    # Compress with LZ4 fast mode (-1), remove original (need sudo - file owned by root)
    sudo lz4 -1 --rm "$MEMORY_FILE" "$MEMORY_FILE.lz4"
    
    COMPRESS_END=$(date +%s%3N)
    COMPRESS_TIME=$((COMPRESS_END - COMPRESS_START))
    
    # Get compressed size and ratio
    COMPRESSED_SIZE=$(sudo stat -c%s "$MEMORY_FILE.lz4")
    COMPRESSED_SIZE_MB=$((COMPRESSED_SIZE / 1048576))
    RATIO=$(awk "BEGIN {printf \"%.1f\", $ORIGINAL_SIZE / $COMPRESSED_SIZE}")
    
    echo "[INFO] Compressed ${ORIGINAL_SIZE_MB}MB → ${COMPRESSED_SIZE_MB}MB (${RATIO}x, $((COMPRESS_TIME))ms)"
  else
    echo "[WARN] memory-ranges file not found, skipping compression"
  fi
else
  echo "[INFO] Skipping compression (use --compress flag to enable)"
fi

# Get cloud-hypervisor PID by finding which process has the overlay disk open
echo "[INFO] Stopping cloud-hypervisor process..."
OVERLAY_DISK="$VM_DIR/overlay.raw"
CH_PID=$(sudo lsof -t "$OVERLAY_DISK" 2>/dev/null | head -1)

if [ -z "$CH_PID" ]; then
  echo "[ERROR] Could not find cloud-hypervisor PID!"
  echo "[ERROR] No process has $OVERLAY_DISK open"
  echo "[ERROR] The VM might already be stopped or check manually:"
  echo "  ps aux | grep cloud-hypervisor | grep $VM_ID"
  exit 1
fi

echo "[INFO] Found cloud-hypervisor process (PID: $CH_PID)"
sudo kill "$CH_PID"

# Wait for process to die
for i in {1..10}; do
  if ! sudo kill -0 "$CH_PID" 2>/dev/null; then
    echo "[INFO] Process terminated"
    break
  fi
  sleep 0.5
done

# Force kill if still alive
if sudo kill -0 "$CH_PID" 2>/dev/null; then
  echo "[WARN] Process still alive, force killing..."
  sudo kill -9 "$CH_PID"
  sleep 1
fi

# Wait for file locks to be released
echo "[INFO] Waiting for disk locks to be released..."
sleep 2

echo "[INFO] Cloud-hypervisor process stopped (PID: $CH_PID)"

# Remove socket
sudo rm -f "$SOCKET" || true

# Get TAP device name from config and remove it
echo "[INFO] Cleaning up TAP device..."
TAP=$(jq -r '.tap' "$VM_DIR/config.json" 2>/dev/null || echo "")

if [ -n "$TAP" ] && ip link show "$TAP" &>/dev/null; then
  echo "[INFO] Removing TAP device $TAP..."
  sudo ip link set "$TAP" down 2>/dev/null || true
  sudo ip link delete "$TAP" 2>/dev/null || true
  echo "[INFO] TAP device $TAP removed"
else
  echo "[INFO] TAP device not found or already removed"
fi

# Log the operation
echo "[INFO] Logging standby operation..."
TIMESTAMP=$(date -Iseconds)
STANDBY_END=$(date +%s%3N)
TOTAL_TIME=$((STANDBY_END - STANDBY_START))

# Get snapshot size
SNAPSHOT_SIZE=$(du -sh "$SNAPSHOT_DIR" 2>/dev/null | cut -f1 || echo "unknown")

echo "$TIMESTAMP - VM $VM_ID entered standby - Memory: ${ACTUAL_SIZE_MB}MB, Snapshot: $SNAPSHOT_SIZE" >> "$VM_DIR/standby.log"

# Format time helper
format_time() {
  local ms=$1
  local sec=$((ms / 1000))
  local msec=$((ms % 1000))
  printf "%d.%03d" $sec $msec
}

echo ""
echo "========================================"
echo "Standby Complete!"
echo "========================================"
echo "VM: $VM_ID"
echo "Snapshot: $SNAPSHOT_DIR"
echo "Snapshot size: $SNAPSHOT_SIZE"
echo "Time: $TIMESTAMP"
echo ""
echo "Timing Breakdown:"
echo "  Memory resize:  $(format_time $MEMORY_RESIZE_TIME)s (→ ${ACTUAL_SIZE_MB}MB)"
echo "  VM pause:       $(format_time $PAUSE_TIME)s"
echo "  Snapshot save:  $(format_time $SNAPSHOT_TIME)s"
if [ "$COMPRESS_FLAG" = "--compress" ] && [ "$COMPRESS_TIME" -gt 0 ]; then
  echo "  LZ4 compress:   $(format_time $COMPRESS_TIME)s (${ORIGINAL_SIZE_MB}MB → ${COMPRESSED_SIZE_MB}MB, ${RATIO}x)"
fi
echo "  Process stop:   (included above)"
echo "  ─────────────────────────────────"
echo "  Total time:     $(format_time $TOTAL_TIME)s"
echo ""
echo "To restore: ./scripts/restore-vm.sh $VM_NUM"
echo "========================================"

