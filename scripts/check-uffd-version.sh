#!/usr/bin/env bash
set -euo pipefail

base_ref="${1:-origin/main}"

if ! git rev-parse --verify "${base_ref}^{commit}" >/dev/null 2>&1; then
  echo "base ref not found: ${base_ref}" >&2
  exit 1
fi

changed_runtime_files="$(
  git diff --name-only "${base_ref}...HEAD" -- \
    lib/uffdpager \
    lib/hypervisor/firecracker/process.go \
    lib/hypervisor/firecracker/config.go \
    lib/instances/firecracker_uffd.go \
    lib/instances/guest_resume_network.go \
    lib/instances/resume_network_handoff.go \
    cmd/api/main.go |
    grep -Ev '(^lib/uffdpager/VERSION$|(^|/)README\.md$|_test\.go$)' || true
)"

if [ -z "${changed_runtime_files}" ]; then
  exit 0
fi

if git diff --quiet "${base_ref}...HEAD" -- lib/uffdpager/VERSION; then
  echo "UFFD pager runtime files changed without updating lib/uffdpager/VERSION:" >&2
  echo "${changed_runtime_files}" >&2
  exit 1
fi

exit 0
