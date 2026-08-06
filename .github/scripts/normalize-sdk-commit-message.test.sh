#!/usr/bin/env bash
set -euo pipefail

source "$(dirname "$0")/normalize-sdk-commit-message.sh"

cases=(
  'untyped|Add builders|feat: Add builders'
  'unknown type|api: add builders|feat: api: add builders'
  'scoped|fix(api): correct status|fix(api): correct status'
  'no space|fix!:breaking endpoint removal|fix!:breaking endpoint removal'
  'empty scope|feat(): add endpoint|feat: feat(): add endpoint'
  'nested scope|feat(api(v2)): add endpoint|feat: feat(api(v2)): add endpoint'
  'slash breaking type|api/v2!: remove endpoint|feat!: api/v2!: remove endpoint'
  'dot breaking type|api.sdk!:remove endpoint|feat!: api.sdk!:remove endpoint'
  'scoped breaking type|api(cache)!: replace cache|feat!: api(cache)!: replace cache'
)

for test_case in "${cases[@]}"; do
  IFS='|' read -r name input expected <<< "$test_case"
  actual=$(normalize_sdk_commit_message "$input")
  if [[ "$actual" != "$expected" ]]; then
    printf '%s: expected %q, got %q\n' "$name" "$expected" "$actual" >&2
    exit 1
  fi
done
