#!/usr/bin/env bash
set -euo pipefail

test -e /dev/kvm
grep -Eq '(^flags|^Features).* (vmx|svm)( |$)' /proc/cpuinfo
systemctl is-active --quiet hypeman
token="$(hypeman-create-token validation 1h)"
api_port="${HYPEMAN_API_PORT:-8080}"
curl -fsS -H "Authorization: Bearer ${token}" "http://127.0.0.1:${api_port}/health" >/dev/null
echo "hypeman aws validation passed"
