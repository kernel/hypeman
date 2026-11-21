#!/usr/bin/env bash
# Shows that a cold start whose stored Cloud Hypervisor build was dropped now
# boots on the starter default instead of failing. No VM is launched: the start
# path runs against a fake VMM starter, so only version resolution is exercised.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The on-disk metadata an instance created before the upgrade carries. The Go
# test below builds the same fixture, so a dropped build (v51.1) is what the
# start path sees.
cat >"$tmp/metadata.json" <<'JSON'
{
  "Id": "version-fallback",
  "Name": "version-fallback",
  "HypervisorType": "version-fallback-test",
  "HypervisorVersion": "v51.1",
  "KernelVersion": "ch-v6.12.9"
}
JSON
cat "$tmp/metadata.json"

go test ./lib/instances/ \
	-run 'TestResolveStartVersion|TestStartAndBootVMFallsBackForRemovedVersion' \
	-count=1 -v 2>&1 | tee "$tmp/out" | grep -E '^(=== RUN|--- (PASS|FAIL)|PASS|FAIL|ok)|StartVM version'

grep -q 'StartVM version: "v51.2"' "$tmp/out"
grep -q 'unsupported, using default' "$tmp/out"
echo "ok: removed version v51.1 resolved to v51.2 on the start path"
