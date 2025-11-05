#!/usr/bin/env bash
set -euo pipefail

echo "========================================"
echo "Stopping All VMs"
echo "========================================"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Stop each VM
for i in {1..10}; do
  echo ""
  "$SCRIPT_DIR/stop-vm.sh" "$i"
done

echo ""
echo "========================================"
echo "All VMs stopped"
echo "========================================"

