#!/usr/bin/env bash
# Proves the CloudFormation user-data installer tag lookup survives `set -o pipefail`.
#
# Run against deploy/aws/cloudformation/template.yaml: extracts the real user data,
# undoes the `${!x}` -> `${x}` CloudFormation escape, and executes the tag block plus
# its fail-closed guard under `set -euo pipefail` with a fake curl on PATH.
#
#   old shape: releases JSON piped into a reader that exits at the first match
#              -> curl dies with SIGPIPE / exit 23 -> pipefail aborts bootstrap
#   new shape: tag read from the releases/latest redirect -> no reader, no SIGPIPE
#
# usage: bash proof/curl_pipefail.sh
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
template="$repo_root/deploy/aws/cloudformation/template.yaml"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failures=0

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

# --- fake curl ---------------------------------------------------------------------
# Models both behaviours the lookup can hit: a JSON body on stdout that a reader may
# stop consuming, and the redirect target printed by -w. Real curl ignores SIGPIPE and
# reports the broken pipe as exit 23, which is what this reproduces.
mkdir -p "$work/bin"
cat >"$work/bin/curl" <<'FAKE'
#!/usr/bin/env bash
set -uo pipefail
if [ "${FAKE_FORBID:-0}" = "1" ]; then
  echo "fake curl: unexpected network call: $*" >&2
  exit 99
fi
for arg in "$@"; do
  case "$arg" in
    *'%{url_effective}'*)
      if [ -n "${FAKE_EFFECTIVE_URL:-}" ]; then
        printf '%s' "$FAKE_EFFECTIVE_URL"
      fi
      exit "${FAKE_EFFECTIVE_STATUS:-0}"
      ;;
  esac
done
trap '' PIPE
if ! printf '%s\n' "${FAKE_JSON_HEAD:?}"; then
  echo "curl: (23) Failure writing output to destination" >&2
  exit 23
fi
n=0
while [ "$n" -lt 4096 ]; do
  n=$((n + 1))
  # The tag_name key already went out, so an early-exit reader closes the pipe here.
  if ! printf '%s' "${FAKE_JSON_PAD:?}"; then
    echo "curl: (23) Failure writing output to destination" >&2
    exit 23
  fi
done
exit 0
FAKE
chmod 755 "$work/bin/curl"

json_head='{
  "url": "https://api.github.com/repos/kernel/hypeman/releases/1",
  "tag_name": "v0.9.16",
  "name": "Hypeman v0.9.16",'
json_pad="$(printf '%0.sx' $(seq 1 128))"

export FAKE_JSON_HEAD="$json_head" FAKE_JSON_PAD="$json_pad"
export FAKE_EFFECTIVE_URL="https://github.com/kernel/hypeman/releases/tag/v0.9.16"

# --- extract the real user data from the template ----------------------------------
python3 - "$template" >"$work/userdata.sh" <<'PY'
import sys

lines = open(sys.argv[1]).read().splitlines()
start = next(i for i, line in enumerate(lines) if line.strip() == "Fn::Base64: !Sub |")
indent = next(
    len(line) - len(line.lstrip()) for line in lines[start + 1:] if line.strip()
)
body = []
for line in lines[start + 1:]:
    if line.strip() and len(line) - len(line.lstrip()) < indent:
        break
    body.append(line[indent:] if len(line) > indent else "")
sys.stdout.write("\n".join(body).rstrip() + "\n")
PY
# CloudFormation emits `${!name}` as a literal `${name}` for the instance to expand.
sed 's/\${!/\${/g' "$work/userdata.sh" >"$work/boot.sh"

extract() { # $1 first line, $2 last line, $3 output
  awk -v first="$1" -v last="$2" '
    !on && index($0, first) == 1 { on = 1 }
    on { print }
    on && $0 == last { exit }
  ' "$work/boot.sh" >"$3"
  test -s "$3"
}

extract 'if [ "${HypemanVersion}" = "latest" ]; then' 'esac' "$work/tag_block.sh"
extract 'case "${installer_tag}" in' 'esac' "$work/tag_guard.sh"

# Pre-fix lookup, kept verbatim to show what the pipeline did under pipefail.
cat >"$work/tag_block_old.sh" <<'OLD'
if [ "${HypemanVersion}" = "latest" ]; then
  installer_tag="$(curl -fsSL https://api.github.com/repos/kernel/hypeman/releases/latest | grep -m1 '"tag_name"' | cut -d'"' -f4)"
else
  installer_tag="${HypemanVersion}"
fi
OLD

make_harness() { # $1 tag block, $2 output
  {
    echo '#!/usr/bin/env bash'
    echo 'set -euo pipefail'
    cat "$1"
    cat "$work/tag_guard.sh"
    echo 'echo "resolved=${installer_tag}"'
  } >"$2"
  chmod 755 "$2"
}

run_case() { # $1 label, $2 harness, $3 expected status (0|nonzero), $4 expected stdout substring
  local label="$1" harness="$2" want_status="$3" want_text="$4"
  local out="$work/$label.out" status=0
  PATH="$work/bin:$PATH" HypemanVersion="${HV:-latest}" FAKE_FORBID="${FAKE_FORBID:-0}" \
    bash "$harness" >"$out" 2>&1 || status=$?
  echo "--- $label (exit $status) ---"
  sed -e 's/^x\{16,\}/[body padding] /' "$out" | cut -c1-200 | sed 's/^/    /' 
  if [ "$want_status" = "0" ] && [ "$status" -ne 0 ]; then
    fail "$label exited $status, expected success"
    return
  fi
  if [ "$want_status" != "0" ] && [ "$status" -eq 0 ]; then
    fail "$label exited 0, expected an abort"
    return
  fi
  if [ -n "$want_text" ] && ! grep -qxF "$want_text" "$out"; then
    fail "$label did not print $want_text"
    return
  fi
  pass "$label"
}

# --- (a) old shape: broken pipe aborts the bootstrap ---------------------------------
make_harness "$work/tag_block_old.sh" "$work/harness_old.sh"
run_case old_latest_aborts "$work/harness_old.sh" nonzero ""

probe_status=0
set +e
(
  set -euo pipefail
  PATH="$work/bin:$PATH"
  tag="$(curl -fsSL https://api.github.com/repos/kernel/hypeman/releases/latest | grep -m1 '"tag_name"' | cut -d'"' -f4)"
  echo "resolved=$tag"
) >"$work/probe.out" 2>&1
probe_status=$?
set -e
echo "--- old pipeline under pipefail (exit $probe_status) ---"
sed -e 's/^x\{16,\}/[body padding] /' "$work/probe.out" | cut -c1-200 | sed 's/^/    /' 
if [ "$probe_status" -eq 23 ]; then
  pass "old pipeline exits 23 (broken pipe)"
elif [ "$probe_status" -ne 0 ]; then
  pass "old pipeline exits $probe_status (non-zero, aborts under pipefail)"
else
  fail "old pipeline exited 0; the repro is not exercising a broken pipe"
fi

# --- (b) new shape: latest resolves, pinned does not call out, failures stay fatal --
make_harness "$work/tag_block.sh" "$work/harness_new.sh"

HV=latest run_case new_latest_resolves "$work/harness_new.sh" 0 "resolved=v0.9.16"
HV=v1.2.3 FAKE_FORBID=1 run_case new_pinned_skips_lookup "$work/harness_new.sh" 0 "resolved=v1.2.3"
HV=latest FAKE_EFFECTIVE_URL="" FAKE_EFFECTIVE_STATUS=22 \
  run_case new_latest_failure_aborts "$work/harness_new.sh" nonzero ""
HV=latest FAKE_EFFECTIVE_URL="https://github.com/kernel/hypeman/releases/latest" \
  run_case new_latest_unresolved_aborts "$work/harness_new.sh" nonzero ""

# --- template shape assertions ------------------------------------------------------
if grep -F 'api.github.com/repos/kernel/hypeman/releases/latest' "$template" >/dev/null; then
  fail "template still queries the releases API with an early-exit reader"
else
  pass "template no longer pipes the releases API into an early-exit reader"
fi
if grep -F "curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/kernel/hypeman/releases/latest" "$template" >/dev/null; then
  pass "template reads the tag from the releases/latest redirect"
else
  fail "template is missing the redirect-based lookup"
fi
if grep -F 'installer checksum mismatch' "$template" >/dev/null &&
  grep -F 'sha256sum -c -' "$template" >/dev/null; then
  pass "checksum verification is still in place"
else
  fail "checksum verification went missing"
fi

if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
