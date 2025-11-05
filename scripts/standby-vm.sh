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
echo "Standby VM: $VM_ID"
echo "========================================"

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

# Pause the VM
echo "[INFO] Pausing VM..."
if ! sudo ch-remote --api-socket "$SOCKET" pause; then
  echo "[ERROR] Failed to pause VM"
  exit 1
fi
echo "[INFO] VM paused successfully"

# Create snapshot
echo "[INFO] Creating snapshot..."
SNAPSHOT_URL="file://$SNAPSHOT_DIR"
if ! sudo ch-remote --api-socket "$SOCKET" snapshot "$SNAPSHOT_URL"; then
  echo "[ERROR] Failed to create snapshot"
  echo "[INFO] Attempting to resume VM..."
  sudo ch-remote --api-socket "$SOCKET" resume || true
  exit 1
fi
echo "[INFO] Snapshot created successfully"

# Get cloud-hypervisor PID
CH_PID=$(sudo lsof -t "$SOCKET" 2>/dev/null || echo "")

# Delete/kill the VMM
echo "[INFO] Stopping cloud-hypervisor process..."
if [ -n "$CH_PID" ]; then
  sudo kill "$CH_PID"
  # Wait for process to die
  for i in {1..10}; do
    if ! kill -0 "$CH_PID" 2>/dev/null; then
      break
    fi
    sleep 0.5
  done
  
  # Force kill if still alive
  if kill -0 "$CH_PID" 2>/dev/null; then
    echo "[WARN] Process still alive, force killing..."
    sudo kill -9 "$CH_PID" || true
  fi
  
  echo "[INFO] Cloud-hypervisor process stopped (PID: $CH_PID)"
else
  echo "[WARN] Could not find cloud-hypervisor PID"
fi

# Remove socket
sudo rm -f "$SOCKET" || true

# Log the operation
echo "[INFO] Logging standby operation..."
TIMESTAMP=$(date -Iseconds)
echo "$TIMESTAMP - VM $VM_ID entered standby" >> "$VM_DIR/standby.log"

echo ""
echo "========================================"
echo "Standby Complete!"
echo "========================================"
echo "VM: $VM_ID"
echo "Snapshot: $SNAPSHOT_DIR"
echo "Time: $TIMESTAMP"
echo ""
echo "To restore: ./scripts/restore-vm.sh $VM_NUM"
echo "========================================"

