#!/usr/bin/env bash
set -euo pipefail

# Configuration
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/data"

# Check arguments
if [ $# -ne 2 ]; then
  echo "Usage: $0 <vol-id> <size-gb>"
  echo "Example: $0 vol-test 10"
  echo ""
  echo "Creates a persistent volume of specified size"
  exit 1
fi

VOL_ID="$1"
SIZE_GB="$2"

# Validate size is a number
if ! [[ "$SIZE_GB" =~ ^[0-9]+$ ]]; then
  echo "[ERROR] Size must be a positive integer (GB)"
  exit 1
fi

VOL_DIR="$BASE_DIR/volumes/$VOL_ID"

echo "========================================"
echo "Creating Volume: $VOL_ID"
echo "========================================"

# Check if volume already exists
if [ -d "$VOL_DIR" ]; then
  echo "[ERROR] Volume $VOL_ID already exists at $VOL_DIR"
  exit 1
fi

# Create volume directory
echo "[INFO] Creating volume directory..."
mkdir -p "$VOL_DIR"

DISK_PATH="$VOL_DIR/disk.raw"

# Create sparse raw disk
echo "[INFO] Creating ${SIZE_GB}GB sparse disk..."
truncate -s "${SIZE_GB}G" "$DISK_PATH"

# Format with ext4
echo "[INFO] Formatting disk with ext4..."
mkfs.ext4 -q "$DISK_PATH"

# Create metadata
TIMESTAMP=$(date -Iseconds)
cat > "$VOL_DIR/metadata.json" <<EOF
{
  "volume_id": "$VOL_ID",
  "size_gb": $SIZE_GB,
  "created_at": "$TIMESTAMP",
  "format": "ext4",
  "disk_path": "$DISK_PATH"
}
EOF

# Get actual size
ACTUAL_SIZE=$(du -h "$DISK_PATH" | cut -f1)

echo ""
echo "========================================"
echo "Volume Created!"
echo "========================================"
echo "Volume ID: $VOL_ID"
echo "Size: ${SIZE_GB}GB (virtual), $ACTUAL_SIZE (actual)"
echo "Path: $DISK_PATH"
echo "Created: $TIMESTAMP"
echo ""
echo "To attach to a VM, add the following to the cloud-hypervisor command:"
echo "  --disk path=$DISK_PATH"
echo ""
echo "Note: You'll need to restart the VM to attach a volume"
echo "========================================"

