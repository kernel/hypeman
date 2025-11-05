#!/usr/bin/env bash
set -euo pipefail

# Configuration
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/data"

# Check arguments
if [ $# -ne 1 ]; then
  echo "Usage: $0 <vm-id>"
  echo "Example: $0 5"
  echo ""
  echo "VM ID should be 1-10"
  exit 1
fi

VM_NUM="$1"

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
echo "Restore VM: $VM_ID"
echo "========================================"

# Start overall timing
RESTORE_START=$(date +%s%3N)

# Check if VM directory exists
if [ ! -d "$VM_DIR" ]; then
  echo "[ERROR] VM directory not found: $VM_DIR"
  exit 1
fi

# Check if snapshot exists
if [ ! -d "$SNAPSHOT_DIR" ]; then
  echo "[ERROR] Snapshot not found: $SNAPSHOT_DIR"
  echo "Please create a snapshot first with: ./scripts/standby-vm.sh $VM_NUM"
  exit 1
fi

# Check if VM is already running
if [ -S "$SOCKET" ]; then
  if sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
    echo "[ERROR] VM $VM_ID is already running"
    echo "Stop it first or use a different VM ID"
    exit 1
  else
    # Stale socket
    echo "[WARN] Removing stale socket..."
    sudo rm -f "$SOCKET"
  fi
fi

# Recreate TAP device for restore
echo "[INFO] Setting up TAP device for restore..."
TAP_START=$(date +%s%3N)

TAP=$(jq -r '.tap' "$VM_DIR/config.json" 2>/dev/null)
BRIDGE="vmbr0"

if [ -z "$TAP" ]; then
  echo "[ERROR] Could not read TAP device name from config.json"
  exit 1
fi

# Remove TAP if it exists in a bad state
if ip link show "$TAP" &>/dev/null; then
  echo "[INFO] Removing existing TAP device $TAP..."
  sudo ip link set "$TAP" down 2>/dev/null || true
  sudo ip link delete "$TAP" 2>/dev/null || true
fi

# Create fresh TAP device
echo "[INFO] Creating TAP device $TAP..."
sudo ip tuntap add "$TAP" mode tap user "$(whoami)"
sudo ip link set "$TAP" up
sudo ip link set "$TAP" master "$BRIDGE"
sudo ip link set "$TAP" type bridge_slave isolated on
echo "[INFO] TAP device $TAP ready"

TAP_END=$(date +%s%3N)
TAP_TIME=$((TAP_END - TAP_START))

LOG_FILE="$VM_DIR/logs/console.log"

echo "[INFO] Starting cloud-hypervisor for restore..."
echo "       Socket: $SOCKET"
echo "       Snapshot: $SNAPSHOT_DIR"

# Start cloud-hypervisor with just the API socket (no VM config yet)
# The restore will load everything from the snapshot
CH_START=$(date +%s%3N)

sudo nohup cloud-hypervisor \
  --api-socket "$SOCKET" \
  > "$VM_DIR/ch-restore-stdout.log" 2>&1 &

CH_PID=$!
echo "[INFO] Cloud-hypervisor started with PID: $CH_PID"

# Wait for socket to be ready (poll every 0.01s, timeout after 1s)
echo "[INFO] Waiting for API socket to be ready..."
SOCKET_READY=false
for i in {1..100}; do
  if [ -S "$SOCKET" ]; then
    echo "[INFO] Socket ready"
    SOCKET_READY=true
    break
  fi
  sleep 0.01
done

if [ "$SOCKET_READY" = false ]; then
  echo "[ERROR] Socket not created after 1 second"
  sudo kill "$CH_PID" 2>/dev/null || true
  exit 1
fi

CH_END=$(date +%s%3N)
CH_TIME=$((CH_END - CH_START))

# Decompress memory-ranges to tmpfs (memory) if compressed
MEMORY_FILE="$SNAPSHOT_DIR/memory-ranges"
COMPRESSED_FILE="$SNAPSHOT_DIR/memory-ranges.lz4"
TMPFS_SNAPSHOT="/dev/shm/ch-restore-$VM_ID"
DECOMPRESS_TIME=0

# Clean up any old tmpfs snapshot
rm -rf "$TMPFS_SNAPSHOT" 2>/dev/null || true

if [ -f "$COMPRESSED_FILE" ]; then
  echo "[INFO] Decompressing snapshot to tmpfs (memory)..."
  DECOMPRESS_START=$(date +%s%3N)
  
  # Create tmpfs directory for this restore
  mkdir -p "$TMPFS_SNAPSHOT"
  
  # Decompress directly to tmpfs (in memory, not disk!) - need sudo for root-owned file
  sudo lz4 -d "$COMPRESSED_FILE" "$TMPFS_SNAPSHOT/memory-ranges"
  
  # Copy other snapshot files to tmpfs (small files, fast)
  sudo cp "$SNAPSHOT_DIR/state.json" "$TMPFS_SNAPSHOT/"
  sudo cp "$SNAPSHOT_DIR/config.json" "$TMPFS_SNAPSHOT/"
  
  # Fix permissions so cloud-hypervisor can read
  sudo chmod 644 "$TMPFS_SNAPSHOT"/*
  
  DECOMPRESS_END=$(date +%s%3N)
  DECOMPRESS_TIME=$((DECOMPRESS_END - DECOMPRESS_START))
  
  COMPRESSED_SIZE=$(sudo stat -c%s "$COMPRESSED_FILE" 2>/dev/null || echo "0")
  COMPRESSED_SIZE_MB=$((COMPRESSED_SIZE / 1048576))
  echo "[INFO] Decompressed ${COMPRESSED_SIZE_MB}MB to memory in $((DECOMPRESS_TIME))ms"
  
  # Use tmpfs snapshot location
  SNAPSHOT_DIR_ACTUAL="$TMPFS_SNAPSHOT"
else
  echo "[INFO] Using uncompressed snapshot from disk"
  SNAPSHOT_DIR_ACTUAL="$SNAPSHOT_DIR"
fi

# Restore from snapshot (now in tmpfs/memory if compressed)
echo "[INFO] Restoring from snapshot..."
SNAPSHOT_START=$(date +%s%3N)

SNAPSHOT_URL="file://$SNAPSHOT_DIR_ACTUAL"
if ! sudo ch-remote --api-socket "$SOCKET" restore "source_url=$SNAPSHOT_URL"; then
  echo "[ERROR] Failed to restore from snapshot"
  echo "[INFO] Cleaning up..."
  sudo kill "$CH_PID" 2>/dev/null || true
  sudo rm -f "$SOCKET"
  rm -rf "$TMPFS_SNAPSHOT" 2>/dev/null || true
  exit 1
fi

SNAPSHOT_END=$(date +%s%3N)
SNAPSHOT_TIME=$((SNAPSHOT_END - SNAPSHOT_START))

echo "[INFO] Snapshot restored successfully"

# Resume the VM
echo "[INFO] Resuming VM..."
RESUME_START=$(date +%s%3N)

if ! sudo ch-remote --api-socket "$SOCKET" resume; then
  echo "[ERROR] Failed to resume VM"
  echo "[INFO] Cleaning up..."
  sudo kill "$CH_PID" 2>/dev/null || true
  sudo rm -f "$SOCKET"
  exit 1
fi

RESUME_END=$(date +%s%3N)
RESUME_TIME=$((RESUME_END - RESUME_START))

echo "[INFO] VM resumed successfully"

# Clean up tmpfs snapshot if used
if [ -d "$TMPFS_SNAPSHOT" ]; then
  echo "[INFO] Cleaning up tmpfs snapshot..."
  rm -rf "$TMPFS_SNAPSHOT"
fi

# Restore memory to full 4GB (virtio-mem hot-plug)
echo "[INFO] Restoring memory to 4GB..."
MEMORY_RESTORE_START=$(date +%s%3N)
TARGET_MEMORY="4294967296"  # 4GB in bytes

if sudo ch-remote --api-socket "$SOCKET" resize --memory "$TARGET_MEMORY" 2>&1; then
  MEMORY_RESTORE_END=$(date +%s%3N)
  MEMORY_RESTORE_TIME=$((MEMORY_RESTORE_END - MEMORY_RESTORE_START))
  
  # Verify memory was restored
  FINAL_SIZE=$(sudo ch-remote --api-socket "$SOCKET" info 2>/dev/null | jq -r '.config.memory.size' || echo "0")
  FINAL_SIZE_MB=$((FINAL_SIZE / 1048576))
  echo "[INFO] Memory restored to ${FINAL_SIZE_MB}MB ($((MEMORY_RESTORE_TIME))ms)"
else
  echo "[WARN] Could not restore memory to 4GB, VM running at reduced size"
  MEMORY_RESTORE_TIME=0
fi

# Log the operation
TIMESTAMP=$(date -Iseconds)
echo "$TIMESTAMP - VM $VM_ID restored from standby" >> "$VM_DIR/standby.log"

# Verify it's running (no sleep needed)
if sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
  RESTORE_END=$(date +%s%3N)
  TOTAL_TIME=$((RESTORE_END - RESTORE_START))
  
  # Convert milliseconds to seconds with 3 decimal places
  format_time() {
    local ms=$1
    local sec=$((ms / 1000))
    local msec=$((ms % 1000))
    printf "%d.%03d" $sec $msec
  }
  
  echo ""
  echo "========================================"
  echo "Restore Complete!"
  echo "========================================"
  echo "VM: $VM_ID"
  echo "PID: $CH_PID"
  echo "Time: $TIMESTAMP"
  echo ""
  echo "Timing Breakdown:"
  echo "  TAP device setup:      $(format_time $TAP_TIME)s"
  echo "  Cloud-hypervisor init: $(format_time $CH_TIME)s"
  echo "  LZ4 decompress→tmpfs:  $(format_time $DECOMPRESS_TIME)s"
  echo "  Snapshot restore:      $(format_time $SNAPSHOT_TIME)s"
  echo "  VM resume:             $(format_time $RESUME_TIME)s"
  echo "  Memory resize (→4GB):  $(format_time $MEMORY_RESTORE_TIME)s"
  echo "  ─────────────────────────────────"
  echo "  Total restore time:    $(format_time $TOTAL_TIME)s"
  echo ""
  echo "Memory: ${FINAL_SIZE_MB}MB / 4096MB"
  echo ""
  echo "View logs: ./scripts/logs-vm.sh $VM_NUM"
  echo "Check status: ./scripts/list-vms.sh"
  echo "========================================"
else
  echo "[ERROR] VM doesn't seem to be responding after restore"
  exit 1
fi

