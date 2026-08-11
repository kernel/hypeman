#!/usr/bin/env bash
# Compare QEMU microvm and Firecracker boot latency and RSS. This is non-gating.
set -euo pipefail

samples="${1:-${SAMPLES:-5}}"
if ! [[ "$samples" =~ ^[1-9][0-9]*$ ]]; then
  echo "usage: $0 [positive sample count]" >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

echo "Comparing qemu-microvm and firecracker with ${samples} samples each."
echo "Requires Linux amd64, KVM, root-capable make test, and a QEMU binary advertising microvm."
HYPEMAN_HYPERVISOR_BOOT_BENCH=1 \
  HYPEMAN_HYPERVISOR_BOOT_BENCH_SAMPLES="$samples" \
  HYPEMAN_HYPERVISOR_BOOT_BENCH_TYPES="qemu-microvm,firecracker" \
  make test TEST=TestHypervisorBootComparisonReport TEST_TIMEOUT=20m VERBOSE=1
