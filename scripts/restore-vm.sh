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

LOG_FILE="$VM_DIR/logs/console.log"

echo "[INFO] Starting cloud-hypervisor for restore..."
echo "       Socket: $SOCKET"
echo "       Snapshot: $SNAPSHOT_DIR"

# Start cloud-hypervisor with just the API socket (no VM config yet)
# The restore will load everything from the snapshot
sudo nohup cloud-hypervisor \
  --api-socket "$SOCKET" \
  > "$VM_DIR/ch-restore-stdout.log" 2>&1 &

CH_PID=$!
echo "[INFO] Cloud-hypervisor started with PID: $CH_PID"

# Wait for socket to be ready
echo "[INFO] Waiting for API socket to be ready..."
for i in {1..30}; do
  if [ -S "$SOCKET" ]; then
    echo "[INFO] Socket ready"
    break
  fi
  sleep 0.5
done

if [ ! -S "$SOCKET" ]; then
  echo "[ERROR] Socket not created after 15 seconds"
  sudo kill "$CH_PID" 2>/dev/null || true
  exit 1
fi

# Give it a moment for the API to be fully ready
sleep 1

# Restore from snapshot
echo "[INFO] Restoring from snapshot..."
SNAPSHOT_URL="file://$SNAPSHOT_DIR"
if ! sudo ch-remote --api-socket "$SOCKET" restore "source_url=$SNAPSHOT_URL"; then
  echo "[ERROR] Failed to restore from snapshot"
  echo "[INFO] Cleaning up..."
  sudo kill "$CH_PID" 2>/dev/null || true
  sudo rm -f "$SOCKET"
  exit 1
fi

echo "[INFO] Snapshot restored successfully"

# Resume the VM
echo "[INFO] Resuming VM..."
if ! sudo ch-remote --api-socket "$SOCKET" resume; then
  echo "[ERROR] Failed to resume VM"
  echo "[INFO] Cleaning up..."
  sudo kill "$CH_PID" 2>/dev/null || true
  sudo rm -f "$SOCKET"
  exit 1
fi

echo "[INFO] VM resumed successfully"

# Log the operation
TIMESTAMP=$(date -Iseconds)
echo "$TIMESTAMP - VM $VM_ID restored from standby" >> "$VM_DIR/standby.log"

# Verify it's running
sleep 1
if sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
  echo ""
  echo "========================================"
  echo "Restore Complete!"
  echo "========================================"
  echo "VM: $VM_ID"
  echo "PID: $CH_PID"
  echo "Time: $TIMESTAMP"
  echo ""
  echo "View logs: ./scripts/logs-vm.sh $VM_NUM"
  echo "Check status: ./scripts/list-vms.sh"
  echo "========================================"
else
  echo "[ERROR] VM doesn't seem to be responding after restore"
  exit 1
fi

