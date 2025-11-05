#!/usr/bin/env bash
set -euo pipefail

# Configuration
NUM_VMS=10
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/data"
SUBNET_BASE="192.168.100"

echo "========================================"
echo "Starting $NUM_VMS VMs"
echo "========================================"

# Verify data directory exists
if [ ! -d "$BASE_DIR/guests" ]; then
  echo "[ERROR] VM data directory not found!"
  echo "Please run ./scripts/setup-vms.sh first."
  exit 1
fi

# Verify images exist
if [ ! -f "$BASE_DIR/system/vmlinux" ]; then
  echo "[ERROR] Kernel not found at $BASE_DIR/system/vmlinux"
  exit 1
fi

if [ ! -f "$BASE_DIR/system/initrd" ]; then
  echo "[ERROR] Initrd not found at $BASE_DIR/system/initrd"
  exit 1
fi

if [ ! -f "$BASE_DIR/images/chromium-headful/v1/rootfs.ext4" ]; then
  echo "[ERROR] Rootfs not found at $BASE_DIR/images/chromium-headful/v1/rootfs.ext4"
  exit 1
fi

ROOTFS="$BASE_DIR/images/chromium-headful/v1/rootfs.ext4"
KERNEL="$BASE_DIR/system/vmlinux"
INITRD="$BASE_DIR/system/initrd"

# Start each VM
for i in $(seq 1 $NUM_VMS); do
  VM_ID="guest-$i"
  VM_DIR="$BASE_DIR/guests/$VM_ID"
  
  # Load config
  if [ ! -f "$VM_DIR/config.json" ]; then
    echo "[ERROR] Config not found for $VM_ID"
    continue
  fi
  
  GUEST_IP=$(jq -r '.ip' "$VM_DIR/config.json")
  MAC=$(jq -r '.mac' "$VM_DIR/config.json")
  TAP=$(jq -r '.tap' "$VM_DIR/config.json")
  SOCKET="$VM_DIR/ch.sock"
  LOG_FILE="$VM_DIR/logs/console.log"
  OVERLAY_DISK="$VM_DIR/overlay.raw"
  CONFIG_DISK="$VM_DIR/config.ext4"
  
  # Check if VM is already running
  if [ -S "$SOCKET" ]; then
    if sudo ch-remote --api-socket "$SOCKET" info &>/dev/null; then
      echo "[SKIP] VM $i ($VM_ID) is already running"
      continue
    else
      # Socket exists but VM not responding, clean it up
      echo "[WARN] Stale socket found for $VM_ID, removing..."
      sudo rm -f "$SOCKET"
    fi
  fi
  
  echo ""
  echo "[INFO] Starting VM $i ($VM_ID)..."
  echo "       IP: $GUEST_IP"
  echo "       TAP: $TAP"
  echo "       Socket: $SOCKET"
  echo "       Log: $LOG_FILE"
  
  # Verify TAP device exists
  if ! ip link show "$TAP" &>/dev/null; then
    echo "[ERROR] TAP device $TAP not found! Run ./scripts/setup-vms.sh first."
    continue
  fi
  
  # Start cloud-hypervisor in background
  # Note: Using nohup and & to run in background, redirecting output to avoid terminal clutter
  sudo nohup cloud-hypervisor \
    --kernel "$KERNEL" \
    --initramfs "$INITRD" \
    --cmdline 'console=ttyS0' \
    --cpus boot=2 \
    --memory size=2048M \
    --disk path="$ROOTFS",readonly=on path="$OVERLAY_DISK" path="$CONFIG_DISK",readonly=on \
    --net "tap=$TAP,ip=$GUEST_IP,mask=255.255.255.0,mac=$MAC" \
    --serial "file=$LOG_FILE" \
    --console off \
    --api-socket "$SOCKET" \
    > "$VM_DIR/ch-stdout.log" 2>&1 &
  
  CH_PID=$!
  echo "       Started with PID: $CH_PID"
  
  # Give it a moment to start
  sleep 0.5
  
  # Verify it's running
  if ! kill -0 $CH_PID 2>/dev/null; then
    echo "[ERROR] VM $i failed to start! Check $VM_DIR/ch-stdout.log"
  else
    echo "       VM $i started successfully!"
  fi
done

echo ""
echo "========================================"
echo "VM Startup Complete!"
echo "========================================"
echo ""
echo "Use ./scripts/list-vms.sh to see running VMs"
echo "Use ./scripts/ssh-vm.sh <vm-id> to SSH into a VM (password: root)"
echo "Use ./scripts/logs-vm.sh <vm-id> to view VM logs"
echo "Use ./scripts/connect-guest.sh <vm-id> to access a VM"
echo ""
echo "To test standby/restore:"
echo "  ./scripts/standby-vm.sh 5"
echo "  ./scripts/restore-vm.sh 5"
echo "========================================"

