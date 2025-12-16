#!/bin/bash
#
# gpu-reset.sh - Reset GPU state after failed passthrough tests or hangs
#
# This script handles common GPU recovery scenarios:
# 1. Killing any stuck cloud-hypervisor processes holding the GPU
# 2. Unbinding from vfio-pci if still bound
# 3. Clearing driver_override
# 4. Triggering driver probe to rebind to nvidia driver
# 5. Restarting nvidia-persistenced
#
# Usage:
#   sudo ./gpu-reset.sh                    # Reset all NVIDIA GPUs
#   sudo ./gpu-reset.sh 0000:a2:00.0       # Reset specific GPU by PCI address
#
# Must be run as root.

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if running as root
if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root (sudo)"
    exit 1
fi

# Get PCI address from argument or find all NVIDIA GPUs
if [[ -n "$1" ]]; then
    PCI_ADDRESSES=("$1")
else
    # Find all NVIDIA GPUs (vendor 10de)
    PCI_ADDRESSES=()
    for dev in /sys/bus/pci/devices/*; do
        if [[ -f "$dev/vendor" ]]; then
            vendor=$(cat "$dev/vendor" 2>/dev/null)
            class=$(cat "$dev/class" 2>/dev/null)
            # Check for NVIDIA vendor (0x10de) and display/3D controller class (0x03xxxx)
            if [[ "$vendor" == "0x10de" && "$class" == 0x03* ]]; then
                addr=$(basename "$dev")
                PCI_ADDRESSES+=("$addr")
            fi
        fi
    done
fi

if [[ ${#PCI_ADDRESSES[@]} -eq 0 ]]; then
    log_error "No NVIDIA GPUs found"
    exit 1
fi

log_info "Found ${#PCI_ADDRESSES[@]} GPU(s) to reset: ${PCI_ADDRESSES[*]}"

# Step 1: Kill any cloud-hypervisor processes that might be holding GPUs
log_info "Step 1: Checking for stuck cloud-hypervisor processes..."
if pgrep -f "cloud-hypervisor" > /dev/null 2>&1; then
    log_warn "Found cloud-hypervisor processes, killing them..."
    pkill -9 -f "cloud-hypervisor" 2>/dev/null || true
    sleep 2
    if pgrep -f "cloud-hypervisor" > /dev/null 2>&1; then
        log_error "Failed to kill cloud-hypervisor processes"
        ps aux | grep cloud-hypervisor | grep -v grep
    else
        log_info "Killed cloud-hypervisor processes"
    fi
else
    log_info "No cloud-hypervisor processes found"
fi

# Process each GPU
for PCI_ADDR in "${PCI_ADDRESSES[@]}"; do
    log_info "Processing GPU at $PCI_ADDR..."
    
    DEVICE_PATH="/sys/bus/pci/devices/$PCI_ADDR"
    
    if [[ ! -d "$DEVICE_PATH" ]]; then
        log_error "Device $PCI_ADDR not found at $DEVICE_PATH"
        continue
    fi
    
    # Get current driver
    CURRENT_DRIVER=""
    if [[ -L "$DEVICE_PATH/driver" ]]; then
        CURRENT_DRIVER=$(basename "$(readlink "$DEVICE_PATH/driver")")
    fi
    log_info "  Current driver: ${CURRENT_DRIVER:-none}"
    
    # Step 2: If bound to vfio-pci, unbind
    if [[ "$CURRENT_DRIVER" == "vfio-pci" ]]; then
        log_info "  Step 2: Unbinding from vfio-pci..."
        echo "$PCI_ADDR" > /sys/bus/pci/drivers/vfio-pci/unbind 2>/dev/null || true
        sleep 1
    else
        log_info "  Step 2: Not bound to vfio-pci, skipping unbind"
    fi
    
    # Step 3: Clear driver_override
    log_info "  Step 3: Clearing driver_override..."
    if [[ -f "$DEVICE_PATH/driver_override" ]]; then
        OVERRIDE=$(cat "$DEVICE_PATH/driver_override" 2>/dev/null)
        if [[ -n "$OVERRIDE" && "$OVERRIDE" != "(null)" ]]; then
            log_info "    Current override: $OVERRIDE"
            echo > "$DEVICE_PATH/driver_override" 2>/dev/null || true
            log_info "    Cleared driver_override"
        else
            log_info "    No driver_override set"
        fi
    fi
    
    # Step 4: Trigger driver probe to rebind to nvidia
    log_info "  Step 4: Triggering driver probe..."
    echo "$PCI_ADDR" > /sys/bus/pci/drivers_probe 2>/dev/null || true
    sleep 2
    
    # Check new driver
    NEW_DRIVER=""
    if [[ -L "$DEVICE_PATH/driver" ]]; then
        NEW_DRIVER=$(basename "$(readlink "$DEVICE_PATH/driver")")
    fi
    log_info "  New driver: ${NEW_DRIVER:-none}"
    
    if [[ "$NEW_DRIVER" == "nvidia" ]]; then
        log_info "  ✓ GPU successfully rebound to nvidia driver"
    elif [[ -z "$NEW_DRIVER" ]]; then
        log_warn "  GPU has no driver bound - may need manual intervention or reboot"
    else
        log_warn "  GPU bound to $NEW_DRIVER (expected nvidia)"
    fi
done

# Step 5: Restart nvidia-persistenced
log_info "Step 5: Restarting nvidia-persistenced..."
if systemctl is-active nvidia-persistenced > /dev/null 2>&1; then
    log_info "  nvidia-persistenced is already running"
else
    if systemctl start nvidia-persistenced 2>/dev/null; then
        log_info "  Started nvidia-persistenced"
    else
        log_warn "  Failed to start nvidia-persistenced (may not be installed or GPU not ready)"
    fi
fi

# Final verification
log_info ""
log_info "=== Final GPU State ==="
for PCI_ADDR in "${PCI_ADDRESSES[@]}"; do
    echo ""
    lspci -nnks "$PCI_ADDR" 2>/dev/null || echo "Could not query $PCI_ADDR"
done

echo ""
log_info "=== nvidia-smi ==="
if command -v nvidia-smi &> /dev/null; then
    nvidia-smi 2>&1 | head -20 || log_warn "nvidia-smi failed (GPU may need more time or reboot)"
else
    log_warn "nvidia-smi not found"
fi

echo ""
log_info "GPU reset complete!"
log_info "If GPUs are still in a bad state, a system reboot may be required."

