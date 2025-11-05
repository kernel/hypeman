#!/usr/bin/env bash
set -euo pipefail

# --- config ---
BR=vmbr0
SUBNET=192.168.100.0/24
BR_IP=192.168.100.1/24         # host's gateway IP on the bridge
# Replace with your real uplink iface (route -n or ip r | grep default)
UPLINK=eth0

echo "[INFO] Starting host network setup..."
echo "[INFO] Configuration: bridge=$BR, subnet=$SUBNET, uplink=$UPLINK"

# 1) bridge
echo "[INFO] Setting up bridge interface '$BR'..."
if ! ip link show "$BR" &>/dev/null; then
  echo "[INFO] Creating bridge '$BR'..."
  sudo ip link add "$BR" type bridge
  echo "[INFO] Bridge '$BR' created successfully"
else
  echo "[INFO] Bridge '$BR' already exists, skipping creation"
fi
echo "[INFO] Bringing up bridge '$BR'..."
sudo ip link set "$BR" up || true
echo "[INFO] Bridge '$BR' is up"

# 2) assign host IP on bridge (idempotent)
echo "[INFO] Assigning IP address '$BR_IP' to bridge '$BR'..."
if ! ip -br addr show "$BR" | grep -q "${BR_IP%/*}"; then
  sudo ip addr add "$BR_IP" dev "$BR"
  echo "[INFO] IP address '$BR_IP' assigned to bridge '$BR'"
else
  echo "[INFO] IP address '$BR_IP' already assigned to bridge '$BR', skipping"
fi

# 3) IP forwarding
echo "[INFO] Enabling IP forwarding..."
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null
echo "[INFO] IP forwarding enabled"

# 4) NAT (iptables) — add only if missing
echo "[INFO] Configuring iptables NAT and forwarding rules..."
if ! sudo iptables -t nat -C POSTROUTING -s "$SUBNET" -o "$UPLINK" -j MASQUERADE 2>/dev/null; then
  echo "[INFO] Adding MASQUERADE rule for subnet $SUBNET on $UPLINK..."
  sudo iptables -t nat -A POSTROUTING -s "$SUBNET" -o "$UPLINK" -j MASQUERADE
  echo "[INFO] MASQUERADE rule added"
else
  echo "[INFO] MASQUERADE rule already exists, skipping"
fi

# 5) Guest isolation and security rules
echo "[INFO] Configuring guest isolation and forwarding rules..."

# Allow established connections from uplink to bridge
if ! sudo iptables -C FORWARD -i "$UPLINK" -o "$BR" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  echo "[INFO] Adding FORWARD rule for RELATED,ESTABLISHED traffic from $UPLINK to $BR..."
  sudo iptables -A FORWARD -i "$UPLINK" -o "$BR" -m state --state RELATED,ESTABLISHED -j ACCEPT
  echo "[INFO] FORWARD rule (RELATED,ESTABLISHED) added"
else
  echo "[INFO] FORWARD rule (RELATED,ESTABLISHED) already exists, skipping"
fi

# Allow new outbound connections from bridge to uplink
if ! sudo iptables -C FORWARD -i "$BR" -o "$UPLINK" -m state --state NEW,RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  echo "[INFO] Adding FORWARD rule for outbound traffic from $BR to $UPLINK..."
  sudo iptables -A FORWARD -i "$BR" -o "$UPLINK" -m state --state NEW,RELATED,ESTABLISHED -j ACCEPT
  echo "[INFO] FORWARD rule added"
else
  echo "[INFO] FORWARD rule already exists, skipping"
fi

# Drop all other forwarded traffic (guest-to-guest isolation)
# This prevents VMs from talking to each other
# Note: Layer 2 isolation is handled per-TAP with bridge_slave isolated on
echo "[INFO] Checking FORWARD policy..."
CURRENT_POLICY=$(sudo iptables -L FORWARD | grep "^Chain FORWARD" | awk '{print $4}' | tr -d '()')
if [ "$CURRENT_POLICY" != "DROP" ]; then
  echo "[INFO] Setting FORWARD policy to DROP for guest isolation..."
  sudo iptables -P FORWARD DROP
  echo "[INFO] FORWARD policy set to DROP"
else
  echo "[INFO] FORWARD policy already set to DROP, skipping"
fi

echo ""
echo "[SUCCESS] Host network ready!"
echo "  Bridge: $BR (${BR_IP})"
echo "  NAT via: $UPLINK"
echo "  Guest isolation: ENABLED"
echo ""
echo "Next: Run scripts/setup-vms.sh to create VM TAP devices and configs"
