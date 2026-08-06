#!/usr/bin/env bash

normalize_sdk_commit_message() {
  local msg=$1
  local conventional='^(feat|fix|perf|revert)(\([^()]+\))?!?:[[:space:]]*'
  local breaking='^[^()!:[:space:]]+(\([^()]+\))?!:[[:space:]]*'

  if [[ "$msg" =~ $conventional ]]; then
    printf '%s\n' "$msg"
  elif [[ "$msg" =~ $breaking ]]; then
    printf 'feat!: %s\n' "$msg"
  else
    printf 'feat: %s\n' "$msg"
  fi
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  set -euo pipefail
  normalize_sdk_commit_message "${1:?commit message required}"
fi
