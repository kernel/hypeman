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

echo "[INFO] Stopping VM: $VM_ID"

# Check if VM is running
if [ ! -S "$SOCKET" ]; then
  echo "[INFO] VM $VM_ID is not running"
  exit 0
fi

# Check if we can communicate with the VM
if ! sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
  echo "[WARN] VM $VM_ID socket exists but not responding"
  echo "[INFO] Cleaning up stale socket..."
  sudo rm -f "$SOCKET"
  exit 0
fi

# Try graceful shutdown via API
echo "[INFO] Sending shutdown command..."
if sudo ch-remote --api-socket "$SOCKET" shutdown 2>/dev/null; then
  echo "[INFO] Shutdown command sent, waiting for VM to stop..."
  
  # Wait up to 30 seconds for graceful shutdown
  for i in {1..60}; do
    if ! sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
      echo "[INFO] VM stopped gracefully"
      sudo rm -f "$SOCKET" 2>/dev/null || true
      exit 0
    fi
    sleep 0.5
  done
  
  echo "[WARN] VM did not stop gracefully, force killing..."
fi

# Force kill
PID=$(sudo lsof -t "$SOCKET" 2>/dev/null || echo "")
if [ -n "$PID" ]; then
  echo "[INFO] Force stopping cloud-hypervisor (PID: $PID)..."
  sudo kill -9 "$PID" || true
  sleep 1
fi

# Clean up socket
sudo rm -f "$SOCKET" 2>/dev/null || true

echo "[INFO] VM $VM_ID stopped"

