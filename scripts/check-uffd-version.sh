#!/usr/bin/env bash
set -euo pipefail

base_ref="${1:-origin/main}"

if ! git rev-parse --verify "${base_ref}^{commit}" >/dev/null 2>&1; then
  echo "base ref not found: ${base_ref}" >&2
  exit 1
fi

diff_range="${base_ref}...HEAD"
if ! git merge-base "${base_ref}" HEAD >/dev/null 2>&1; then
  diff_range="${base_ref}..HEAD"
fi

changed_runtime_files="$(
  git diff --name-only "${diff_range}" -- \
    lib/uffdpager \
    lib/hypervisor/firecracker/config.go \
    lib/instances/firecracker_uffd.go \
    lib/instances/guest_resume_network.go \
    lib/instances/resume_network_handoff.go \
    cmd/uffd-pager |
    grep -Ev '(^lib/uffdpager/VERSION$|(^|/)README\.md$|_test\.go$)' || true
)"

# process.go contains both UFFD restore wiring and unrelated Firecracker restore
# orchestration. Only require a pager version bump when changed lines touch the
# UFFD-facing restore contract.
process_uffd_changes="$(
  git diff --name-only -G 'uffd|UFFD|SnapshotMemory' "${diff_range}" -- \
    lib/hypervisor/firecracker/process.go || true
)"
if [ -n "${process_uffd_changes}" ]; then
  changed_runtime_files="$(
    printf '%s\n%s\n' "${changed_runtime_files}" "${process_uffd_changes}" |
      sed '/^$/d' |
      sort -u
  )"
fi

if [ -z "${changed_runtime_files}" ]; then
  exit 0
fi

if git diff --quiet "${diff_range}" -- lib/uffdpager/VERSION; then
  echo "UFFD pager runtime files changed without updating lib/uffdpager/VERSION:" >&2
  echo "${changed_runtime_files}" >&2
  exit 1
fi

exit 0
