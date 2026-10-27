#!/usr/bin/env bash
# Runs one shard of the Linux test suite. CI runs every shard in parallel.
#
# Usage: test-shard.sh SHARD COUNT
#        test-shard.sh weights < go-test-json > scripts/test-shard-weights.txt
#
# The suite is split into COUNT shards, instances-1 to instances-<COUNT-1> and
# other. Shard other runs every package except lib/instances, the slowest one.
# The top-level tests of lib/instances are spread over all COUNT shards, other
# included, using the expected seconds in scripts/test-shard-weights.txt:
# longest first, each onto the shard whose expected time ends up lowest. A
# shard's sequential tests add up. Its parallel tests overlap, so they count as
# the longest of them or their total over PARALLEL_OVERLAP, whichever is more.
# Tests missing from the weights file count as DEFAULT_WEIGHT, so new tests are
# still assigned.
#
# Before running, each shard checks that the -run patterns of all shards
# together select every lib/instances test exactly once.
set -euo pipefail

# Weights are in tenths of a second, so every shard does the same integer
# arithmetic whichever host and awk it runs on.
DEFAULT_WEIGHT=1
PARALLEL_OVERLAP=8
# The weights file leaves out tests faster than this.
LISTED_WEIGHT=10

weights_file="$(dirname "$0")/test-shard-weights.txt"
tags=(-tags containers_image_openpgp)
instances_pkg="$(go list "${tags[@]}" ./lib/instances)"

if [ "${1:-}" = weights ]; then
  # Reads `go test -json` output of the whole suite. go test runs packages in
  # parallel, so "other" is the slowest package outside lib/instances.
  awk -v pkg="$instances_pkg" -v listed="$LISTED_WEIGHT" '
    function str(name) {
      if (!match($0, "\"" name "\":\"[^\"]*\"")) return ""
      return substr($0, RSTART + length(name) + 4, RLENGTH - length(name) - 5)
    }
    function num(name) {
      if (!match($0, "\"" name "\":[0-9.]+")) return -1
      return substr($0, RSTART + length(name) + 3, RLENGTH - length(name) - 3) + 0
    }
    {
      action = str("Action"); test = str("Test"); elapsed = num("Elapsed")
      if (str("Package") == pkg) {
        if (test == "" || index(test, "/")) next
        if (action == "pause") parallel[test] = 1
        if (action ~ /^(pass|fail|skip)$/ && elapsed >= 0) secs[test] = elapsed
      } else if (test == "" && action ~ /^(pass|fail)$/ && elapsed > other) {
        other = elapsed
      }
    }
    END {
      for (t in secs) found = 1
      if (!found) { print "no lib/instances test results in input" > "/dev/stderr"; exit 1 }
      print "# Expected seconds per lib/instances test, used by scripts/test-shard.sh to"
      print "# balance CI shards. Regenerate with `make test-shard-weights`."
      printf "# Tests under %.1fs are left out; \"other\" is the time of the other packages.\n", listed / 10
      printf "other %.1f\n", other
      for (t in secs)
        if (secs[t] * 10 >= listed)
          printf "%s %.1f %s\n", t, secs[t], (t in parallel ? "parallel" : "sequential") | "LC_ALL=C sort"
    }
  '
  exit
fi

shard="${1:?usage: test-shard.sh SHARD COUNT}"
count="${2:?usage: test-shard.sh SHARD COUNT}"
index="${shard#instances-}"
if [ "$shard" = other ]; then
  index="$count"
elif [[ "$shard" != instances-* || ! "$index" =~ ^[1-9][0-9]*$ ]] || ((index >= count)); then
  echo "unknown shard $shard (want other or instances-1..instances-$((count - 1)))" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
go test "${tags[@]}" -c -o "$tmp/instances.test" ./lib/instances
# Fix the collation so every shard computes the same assignment.
"$tmp/instances.test" -test.list '.*' | LC_ALL=C sort > "$tmp/all"

awk -v default_weight="$DEFAULT_WEIGHT" '
  FILENAME == ARGV[1] {
    if ($1 ~ /^Test/) { weight[$1] = int($2 * 10 + 0.5); parallel[$1] = ($3 == "parallel") }
    next
  }
  { print ($1 in weight ? weight[$1] : default_weight), ($1 in parallel ? parallel[$1] : 0), $1 }
' "$weights_file" "$tmp/all" | LC_ALL=C sort -k1,1nr -k3,3 > "$tmp/weighted"

# Shard i is instances-i, and shard COUNT is other, which starts with the time
# of the other packages.
awk -v count="$count" -v overlap="$PARALLEL_OVERLAP" \
  -v other="$(awk '$1 == "other" { print int($2 * 10 + 0.5) }' "$weights_file")" '
  # Expected time of a shard, multiplied by overlap to stay in integers.
  function cost(seq, psum, pmax) {
    return seq * overlap + (pmax * overlap > psum ? pmax * overlap : psum)
  }
  BEGIN {
    for (i = 1; i <= count; i++) seq[i] = psum[i] = pmax[i] = 0
    seq[count] = other
  }
  {
    w = $1; best = 0
    for (i = 1; i <= count; i++) {
      s = seq[i]; ps = psum[i]; pm = pmax[i]
      if ($2) { ps += w; if (w > pm) pm = w } else s += w
      c = cost(s, ps, pm)
      if (best == 0 || c < best_cost) { best = i; best_cost = c; best_s = s; best_ps = ps; best_pm = pm }
    }
    seq[best] = best_s; psum[best] = best_ps; pmax[best] = best_pm
    print best, $3
  }
' "$tmp/weighted" > "$tmp/assigned"

pattern() {
  echo "^($(awk -v shard="$1" '$1 == shard { print $2 }' "$tmp/assigned" | paste -sd'|'))\$"
}

for ((i = 1; i <= count; i++)); do
  "$tmp/instances.test" -test.list "$(pattern "$i")"
done | LC_ALL=C sort > "$tmp/selected"
if ! diff -u "$tmp/all" "$tmp/selected"; then
  echo "lib/instances shards do not select every test exactly once" >&2
  exit 1
fi
tests="$(awk -v shard="$index" '$1 == shard' "$tmp/assigned" | wc -l)"
echo "lib/instances: shard $shard runs $tests of $(wc -l < "$tmp/all") tests"

timeout="${TEST_TIMEOUT:-300s}"
status=0
if ((tests > 0)); then
  start=$SECONDS
  # Run the binary built above from the package directory, as go test would.
  if (cd "$(go list -f '{{.Dir}}' "${tags[@]}" ./lib/instances)" &&
    "$tmp/instances.test" -test.paniconexit0 -test.timeout="$timeout" ${VERBOSE:+-test.v} \
      -test.run "$(pattern "$index")"); then
    result=ok
  else
    status=$?
    result=FAIL
  fi
  printf '%s\t%s\t%ds\n' "$result" "$instances_pkg" "$((SECONDS - start))"
fi
if [ "$shard" = other ]; then
  mapfile -t pkgs < <(go list "${tags[@]}" ./... | grep -vxF "$instances_pkg")
  go test "${tags[@]}" -timeout="$timeout" ${VERBOSE:+-v} "${pkgs[@]}" || status=$?
fi
exit "$status"
