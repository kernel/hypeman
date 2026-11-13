#!/bin/sh
# Checks the image before it is published: hypeman's agent launcher is installed,
# no agent is baked into the image, and Chrome starts headless and answers CDP.
# Runs as the build user, leaving no browser profile behind.
set -eu
test -x /usr/local/libexec/hypeman-agent-launcher
test -f /Library/LaunchDaemons/com.kernel.hypeman.agent-launcher.plist
test ! -e /usr/local/sbin/hypeman-guest-agent

chrome="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
test -x "$chrome"
profile=$(mktemp -d)
"$chrome" --headless=new --user-data-dir="$profile" --remote-debugging-port=9222 --no-first-run about:blank >/dev/null 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null; wait $pid 2>/dev/null; rm -rf "$profile"' EXIT
for i in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:9222/json/version; then
    echo
    exit 0
  fi
  sleep 1
done
echo "Chrome did not answer on its debugging port" >&2
exit 1
