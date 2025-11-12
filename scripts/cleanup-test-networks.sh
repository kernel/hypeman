#!/usr/bin/env bash
set -euo pipefail

echo "Cleaning up stale test network resources..."

# Count resources before
bridges_before=$(ip link show | grep -E "^[0-9]+: t[0-9a-f]{3}:" | wc -l)
taps_before=$(ip link show | grep -E "^[0-9]+: tap-" | wc -l)

echo "Found $bridges_before test bridges, $taps_before TAP devices"

if [ "$bridges_before" -eq 0 ] && [ "$taps_before" -eq 0 ]; then
	echo "No stale resources found"
	exit 0
fi

# Delete test TAP devices (tap-*)
if [ "$taps_before" -gt 0 ]; then
	echo "Removing TAP devices..."
	ip link show | grep -E "^[0-9]+: tap-" | awk '{print $2}' | sed 's/:$//' | while read tap; do
		echo "  Removing TAP: $tap"
		sudo ip link delete "$tap" 2>/dev/null || true
	done
fi

# Delete test bridges (t[0-9a-f]{3})
if [ "$bridges_before" -gt 0 ]; then
	echo "Removing test bridges..."
	ip link show | grep -E "^[0-9]+: t[0-9a-f]{3}:" | awk '{print $2}' | sed 's/:$//' | while read br; do
		echo "  Removing bridge: $br"
		sudo ip link delete "$br" 2>/dev/null || true
	done
fi

# Count after
bridges_after=$(ip link show | grep -E "^[0-9]+: t[0-9a-f]{3}:" | wc -l)
taps_after=$(ip link show | grep -E "^[0-9]+: tap-" | wc -l)

echo ""
echo "Cleanup complete!"
if [ "$bridges_after" -gt 0 ] || [ "$taps_after" -gt 0 ]; then
	echo "Remaining: $bridges_after bridges, $taps_after TAPs"
else
	echo "All test resources cleaned up"
fi

