#!/usr/bin/env bash
set -euo pipefail

# Configuration
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/data"
SUBNET_BASE="192.168.100"

# Check arguments
if [ $# -lt 1 ]; then
  echo "Usage: $0 <vm-id> [ssh-args]"
  echo "Example: $0 5"
  echo "Example: $0 5 -L 8080:localhost:8080"
  echo ""
  echo "VM ID should be 1-10"
  echo "Default password: root"
  exit 1
fi

VM_NUM="$1"
shift  # Remove first argument, keep the rest for SSH

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
  echo "Run ./scripts/setup-vms.sh first"
  exit 1
fi

# Load config to get IP
if [ -f "$VM_DIR/config.json" ]; then
  GUEST_IP=$(jq -r '.ip' "$VM_DIR/config.json")
else
  # Fallback calculation
  GUEST_IP="${SUBNET_BASE}.$((9 + VM_NUM))"
fi

# Check if VM is running
SOCKET="$VM_DIR/ch.sock"
if [ ! -S "$SOCKET" ] || ! sudo ch-remote --api-socket "$SOCKET" info &>/dev/null 2>&1; then
  echo "[ERROR] VM $VM_ID is not running"
  echo "Start it with: ./scripts/start-all-vms.sh"
  exit 1
fi

echo "Connecting to VM $VM_ID ($GUEST_IP)..."
echo "Password: root"
echo ""

# SSH to the VM with any additional arguments passed
# -o StrictHostKeyChecking=no to avoid host key prompts on first connect
# -o UserKnownHostsFile=/dev/null to avoid saving host keys
ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@"$GUEST_IP" "$@"

