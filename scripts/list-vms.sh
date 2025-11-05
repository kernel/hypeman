#!/usr/bin/env bash
set -euo pipefail

# Configuration
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/data"

echo "========================================"
echo "VM Status"
echo "========================================"
printf "%-10s %-10s %-15s %-8s %-10s\n" "VM ID" "STATUS" "IP" "PID" "UPTIME"
echo "------------------------------------------------------------------------"

if [ ! -d "$BASE_DIR/guests" ]; then
  echo "No VMs found. Run ./scripts/setup-vms.sh first."
  exit 0
fi

for i in {1..10}; do
  VM_ID="guest-$i"
  VM_DIR="$BASE_DIR/guests/$VM_ID"
  
  if [ ! -d "$VM_DIR" ]; then
    continue
  fi
  
  SOCKET="$VM_DIR/ch.sock"
  
  # Load config
  if [ -f "$VM_DIR/config.json" ]; then
    GUEST_IP=$(jq -r '.ip' "$VM_DIR/config.json" 2>/dev/null || echo "unknown")
  else
    GUEST_IP="unknown"
  fi
  
  # Check if VM is running
  if [ -S "$SOCKET" ]; then
    if sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
      STATUS="RUNNING"
      
      # Get PID
      PID=$(sudo lsof -t "$SOCKET" 2>/dev/null || echo "?")
      
      # Get uptime
      if [ "$PID" != "?" ]; then
        UPTIME=$(ps -p "$PID" -o etime= 2>/dev/null | tr -d ' ' || echo "?")
      else
        UPTIME="?"
      fi
    else
      STATUS="STOPPED"
      PID="-"
      UPTIME="-"
      # Clean up stale socket
      sudo rm -f "$SOCKET" 2>/dev/null || true
    fi
  else
    STATUS="STOPPED"
    PID="-"
    UPTIME="-"
  fi
  
  printf "%-10s %-10s %-15s %-8s %-10s\n" "$VM_ID" "$STATUS" "$GUEST_IP" "$PID" "$UPTIME"
done

echo "------------------------------------------------------------------------"
echo ""
echo "Commands:"
echo "  Start all: ./scripts/start-all-vms.sh"
echo "  Stop VM:   ./scripts/stop-vm.sh <vm-id>"
echo "  Logs:      ./scripts/logs-vm.sh <vm-id>"
echo "  Standby:   ./scripts/standby-vm.sh <vm-id>"
echo "========================================"

