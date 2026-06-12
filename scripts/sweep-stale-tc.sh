#!/usr/bin/env bash
set -euo pipefail

# Sweep stale tc state (HTB classes + basic filters) from the hypeman bridge.
#
# hypeman adds one HTB class and one tc basic filter per rate-limited VM on
# the bridge. Leaked entries from failed or imprecise cleanup accumulate, and
# every packet leaving any VM walks the filter list linearly, so a polluted
# bridge slows all VM egress on the host.
#
# This script deletes classes/filters not tied to a live hype-* TAP device or
# a persisted classid file, mirroring CleanupOrphanedClasses in
# lib/network/bridge_linux.go. Candidates are snapshotted before the keep-sets
# so anything allocated mid-run is never considered stale.
#
# Dry run by default; pass --apply to delete (requires root / CAP_NET_ADMIN).
#
# Usage: sweep-stale-tc.sh [--apply] [--bridge vmbr0] [--data-dir /var/lib/hypeman]

BRIDGE=vmbr0
DATA_DIR=/var/lib/hypeman
APPLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --bridge) BRIDGE="$2"; shift 2 ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done

TC=${TC:-tc}
command -v "$TC" >/dev/null 2>&1 || TC=/usr/sbin/tc

ip link show "$BRIDGE" >/dev/null

# 1. Snapshot candidate classes (everything except the root class 1:1).
mapfile -t ALL_CLASSES < <("$TC" class show dev "$BRIDGE" | awk '$1=="class" && $2=="htb" && $3!="1:1" {print $3}')

# 2. Snapshot candidate filters: "handle flowid rt_iif" per filter. The header
# line carries handle/flowid; the following ematch line carries rt_iif.
mapfile -t FILTER_ROWS < <("$TC" filter show dev "$BRIDGE" parent 1: | awk '
  /^filter/ {
    if (handle != "") print handle, flowid, rtiif
    handle=""; flowid="-"; rtiif="-"
    for (i = 1; i <= NF; i++) {
      if ($i == "handle") handle = $(i+1)
      if ($i == "flowid") flowid = $(i+1)
    }
    next
  }
  match($0, /rt_iif eq [0-9]+/) { rtiif = substr($0, RSTART+10, RLENGTH-10) }
  END { if (handle != "") print handle, flowid, rtiif }
')

# 3. Keep-set: ifindexes of live hype-* TAPs.
declare -A LIVE_IFINDEX=()
while read -r idx _; do
  LIVE_IFINDEX[$idx]=1
done < <(ip -o link show | awk -F': ' '$2 ~ /^hype-/ {print $1}')

# 4. Keep-set: classids persisted by hypeman for instances that still exist.
declare -A KEEP=()
for f in "$DATA_DIR"/guests/*/classid; do
  [[ -f $f ]] || continue
  id=$(tr -d '[:space:]' < "$f")
  [[ -n $id ]] && KEEP[$id]=1
done

# Safety: if filters exist but none carry a parseable rt_iif ematch, the tc
# output format doesn't match expectations and everything would look stale.
if ((${#FILTER_ROWS[@]} > 0)); then
  parsed=0
  for row in "${FILTER_ROWS[@]}"; do
    read -r _ _ rtiif <<<"$row"
    [[ $rtiif != "-" ]] && parsed=$((parsed + 1))
  done
  if ((parsed == 0)); then
    echo "no rt_iif matches parsed from tc filter output — aborting" >&2
    exit 1
  fi
fi

# Filters anchored to a live TAP are authoritative: their flowid is the class
# actually in use, regardless of collision probing. Everything else is stale.
STALE_FILTERS=()
for row in "${FILTER_ROWS[@]}"; do
  read -r handle flowid rtiif <<<"$row"
  if [[ -n ${LIVE_IFINDEX[$rtiif]:-} ]]; then
    KEEP[${flowid#1:}]=1
  else
    STALE_FILTERS+=("$handle")
  fi
done

STALE_CLASSES=()
for classid in "${ALL_CLASSES[@]}"; do
  [[ -n ${KEEP[${classid#1:}]:-} ]] || STALE_CLASSES+=("$classid")
done

echo "bridge=$BRIDGE live_taps=${#LIVE_IFINDEX[@]} filters=${#FILTER_ROWS[@]} classes=${#ALL_CLASSES[@]} stale_filters=${#STALE_FILTERS[@]} stale_classes=${#STALE_CLASSES[@]}"

if [[ $APPLY -eq 0 ]]; then
  echo "dry run — pass --apply to delete"
  exit 0
fi

failed=0
for handle in "${STALE_FILTERS[@]}"; do
  "$TC" filter del dev "$BRIDGE" parent 1: protocol all prio 1 handle "$handle" basic ||
    { echo "failed to delete filter $handle" >&2; failed=$((failed + 1)); }
done

for classid in "${STALE_CLASSES[@]}"; do
  "$TC" qdisc del dev "$BRIDGE" parent "$classid" 2>/dev/null || true # child fq_codel, may not exist
  "$TC" class del dev "$BRIDGE" classid "$classid" ||
    { echo "failed to delete class $classid" >&2; failed=$((failed + 1)); }
done

echo "after: filters=$("$TC" filter show dev "$BRIDGE" parent 1: | grep -c flowid || true) classes=$("$TC" class show dev "$BRIDGE" | wc -l) failed=$failed"
