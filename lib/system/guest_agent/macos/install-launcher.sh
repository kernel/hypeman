#!/bin/sh
# Install the launcher that runs hypeman's guest agent from the read-only agent
# disk hypeman attaches to macOS guests. Run as root in the base image, for
# example from a Packer provisioner. A guest agent installed in the image itself
# is removed, so the agent always comes from the disk.
set -eu
launchctl bootout system/com.kernel.hypeman.guest-agent 2>/dev/null || true
rm -f /Library/LaunchDaemons/com.kernel.hypeman.guest-agent.plist /usr/local/sbin/hypeman-guest-agent
install -d -o root -g wheel -m 0755 /usr/local/libexec
cat > /usr/local/libexec/hypeman-agent-launcher <<'SH'
#!/bin/sh
# Run hypeman's guest agent from its read-only agent disk, mounting it if macOS
# has not yet. Only the disk hypeman attaches is trusted: a fixed internal device,
# not a disk image a guest user attached under the same name.
volume=/Volumes/HypemanAgent
install -d -o root -g wheel -m 0755 /var/run/hypeman
for i in $(seq 1 120); do
  if [ -x "$volume/hypeman-guest-agent" ]; then
    info=$(/usr/sbin/diskutil info "$volume" 2>/dev/null)
    if echo "$info" | grep -Eq '^ *Device Location: +Internal$' &&
      echo "$info" | grep -Eq '^ *Removable Media: +Fixed$' &&
      ! echo "$info" | grep -Eq '^ *Protocol: +Disk Image$'; then
      exec "$volume/hypeman-guest-agent"
    fi
    echo "refusing $volume: it is not hypeman's attached agent disk" >&2
    exit 1
  fi
  /usr/sbin/diskutil mount readOnly HypemanAgent >/dev/null 2>&1
  sleep 1
done
echo "hypeman agent disk not found" >&2
exit 1
SH
chown root:wheel /usr/local/libexec/hypeman-agent-launcher
chmod 0755 /usr/local/libexec/hypeman-agent-launcher
cat > /Library/LaunchDaemons/com.kernel.hypeman.agent-launcher.plist <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.kernel.hypeman.agent-launcher</string>
  <key>ProgramArguments</key><array><string>/usr/local/libexec/hypeman-agent-launcher</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>UserName</key><string>root</string>
  <key>GroupName</key><string>wheel</string>
  <key>StandardOutPath</key><string>/var/log/hypeman-guest-agent.log</string>
  <key>StandardErrorPath</key><string>/var/log/hypeman-guest-agent.log</string>
</dict>
</plist>
PLIST
chown root:wheel /Library/LaunchDaemons/com.kernel.hypeman.agent-launcher.plist
chmod 0644 /Library/LaunchDaemons/com.kernel.hypeman.agent-launcher.plist
