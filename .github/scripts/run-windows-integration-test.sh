#!/usr/bin/env bash
set -euo pipefail

if (( $# != 3 )); then
  echo "usage: $0 <gate-env> <test-name> <package>" >&2
  exit 2
fi

gate_env=$1
test_name=$2
package=$3
run_prefix="ci-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
tmpdir=$(sudo mktemp -d /ci/wXXXXXX)
echo "WINDOWS_TEST_TMPDIR=$tmpdir" >> "$GITHUB_ENV"
test_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:$PATH"

cleanup() {
  local pids=()
  local pid
  while read -r pid; do
    if sudo grep -zFqx "HYPEMAN_UFFD_SYSTEMD_INSTANCE_PREFIX=$run_prefix" "/proc/$pid/environ" 2>/dev/null; then
      pids+=("$pid")
    fi
  done < <(pgrep -x 'qemu-system-.*|swtpm' || true)

  if (( ${#pids[@]} > 0 )); then
    sudo kill -TERM "${pids[@]}" 2>/dev/null || true
    sleep 2
    for pid in "${pids[@]}"; do
      if sudo kill -0 "$pid" 2>/dev/null; then
        sudo kill -KILL "$pid" 2>/dev/null || true
      fi
    done
  fi
  sudo rm -rf "$tmpdir"
}
trap cleanup EXIT

for attempt in 1 2 3; do
  cleanup
  sudo install -d -m 1777 "$tmpdir"
  if sudo env \
    "PATH=$test_path" \
    "TMPDIR=$tmpdir" \
    "CI=true" \
    "HYPEMAN_UFFD_SYSTEMD_INSTANCE_PREFIX=$run_prefix" \
    "$gate_env=1" \
    "HYPEMAN_WINDOWS_OVMF_CODE=$HYPEMAN_WINDOWS_OVMF_CODE" \
    "HYPEMAN_WINDOWS_OVMF_VARS=$HYPEMAN_WINDOWS_OVMF_VARS" \
    go test -count=1 -run "^${test_name}$" -timeout 2m "$package"; then
    exit 0
  fi
  cleanup
  test "$attempt" = 3 || sleep 5
done
exit 1
