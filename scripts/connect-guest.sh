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

# Check if VM exists
if [ ! -d "$VM_DIR" ]; then
  echo "[ERROR] VM not found: $VM_ID"
  exit 1
fi

# Load config
if [ -f "$VM_DIR/config.json" ]; then
  GUEST_IP=$(jq -r '.ip' "$VM_DIR/config.json")
else
  echo "[ERROR] Config not found for $VM_ID"
  exit 1
fi

# Check if VM is running
SOCKET="$VM_DIR/ch.sock"
if [ ! -S "$SOCKET" ] || ! sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
  echo "[ERROR] VM $VM_ID is not running"
  echo "Start it with: ./scripts/start-all-vms.sh"
  exit 1
fi

# Calculate WebRTC port (assuming port forwarding is set up)
HOST_PORT=$((8080 + VM_NUM - 1))  # VM 1 -> 8080, VM 2 -> 8081, etc.
GUEST_PORT=8080

echo "========================================"
echo "Connect to VM: $VM_ID"
echo "========================================"
echo ""
echo "Guest IP: $GUEST_IP"
echo ""
echo "WebRTC Access (if port forwarding is enabled):"
echo "  URL: http://localhost:$HOST_PORT"
echo "  (Forwards to $GUEST_IP:$GUEST_PORT)"
echo ""
echo "Direct Network Access:"
echo "  Guest IP: $GUEST_IP"
echo "  WebRTC Port: $GUEST_PORT"
echo ""
echo "To enable port forwarding:"
echo "  ./scripts/setup-port-forwarding.sh"
echo ""
echo "To test connectivity:"
echo "  ping $GUEST_IP"
echo "  curl http://$GUEST_IP:$GUEST_PORT"
echo ""
echo "To view logs:"
echo "  ./scripts/logs-vm.sh $VM_NUM"
echo "========================================"

