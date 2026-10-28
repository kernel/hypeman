#!/bin/sh
set -eu
# This example performs no downloads and inherits the pinned base toolchain.
[ "$(/usr/bin/uname -m)" = arm64 ]
/usr/bin/xcodebuild -version >/dev/null
/usr/bin/install -d -m 0755 /usr/local/share/hypeman
printf '%s\n' 'macos-base-pipeline-v1' > /usr/local/share/hypeman/base-pipeline-marker
/bin/chmod 0644 /usr/local/share/hypeman/base-pipeline-marker
