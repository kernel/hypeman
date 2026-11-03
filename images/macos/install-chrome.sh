#!/bin/sh
# Install Google Chrome from Google's stable DMG. The download URL is not
# versioned, so the app is checked by its code signature: it must be signed by
# Google's Developer ID team and pass Gatekeeper.
set -eu
team=EQHXZ8M8AV
work=$(mktemp -d)
mount="$work/mount"
trap 'hdiutil detach -quiet "$mount" 2>/dev/null || true; rm -rf "$work"' EXIT
curl -fsSL -o "$work/chrome.dmg" https://dl.google.com/chrome/mac/universal/stable/GGRO/googlechrome.dmg
hdiutil attach -quiet -nobrowse -readonly -mountpoint "$mount" "$work/chrome.dmg"
app="$mount/Google Chrome.app"
codesign --verify --deep --strict "$app"
codesign -dv "$app" 2>&1 | grep -qx "TeamIdentifier=$team"
spctl --assess --type execute "$app"
rm -rf "/Applications/Google Chrome.app"
ditto "$app" "/Applications/Google Chrome.app"
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --version
