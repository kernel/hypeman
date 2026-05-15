#!/usr/bin/env bash
set -euo pipefail

test -e /dev/kvm
grep -Eq '(^flags|^Features).* (vmx|svm)( |$)' /proc/cpuinfo
systemctl is-active --quiet hypeman
token="$(hypeman-create-token validation 1h)"
curl -fsS -H "Authorization: Bearer ${token}" http://127.0.0.1:8080/health >/dev/null
echo "hypeman aws validation passed"
