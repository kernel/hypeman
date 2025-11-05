#!/usr/bin/env bash
set -euo pipefail

# Configuration
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/data"

# Check arguments
if [ $# -lt 1 ]; then
  echo "Usage: $0 <vm-id> [follow]"
  echo "Example: $0 5"
  echo "Example: $0 5 -f  # Follow logs"
  echo ""
  echo "VM ID should be 1-10"
  exit 1
fi

VM_NUM="$1"
FOLLOW_FLAG="${2:-}"

# Validate VM number
if ! [[ "$VM_NUM" =~ ^[0-9]+$ ]] || [ "$VM_NUM" -lt 1 ] || [ "$VM_NUM" -gt 10 ]; then
  echo "[ERROR] Invalid VM ID: $VM_NUM"
  echo "VM ID must be between 1 and 10"
  exit 1
fi

VM_ID="guest-$VM_NUM"
VM_DIR="$BASE_DIR/guests/$VM_ID"
LOG_FILE="$VM_DIR/logs/console.log"

# Check if log file exists
if [ ! -f "$LOG_FILE" ]; then
  echo "[ERROR] Log file not found: $LOG_FILE"
  echo "VM $VM_ID may not have been started yet."
  exit 1
fi

echo "========================================"
echo "Logs for VM: $VM_ID"
echo "Log file: $LOG_FILE"
echo "========================================"
echo ""

# Show logs
if [ "$FOLLOW_FLAG" = "-f" ] || [ "$FOLLOW_FLAG" = "--follow" ]; then
  tail -f "$LOG_FILE"
else
  tail -n 100 "$LOG_FILE"
  echo ""
  echo "Use '$0 $VM_NUM -f' to follow logs in real-time"
fi

