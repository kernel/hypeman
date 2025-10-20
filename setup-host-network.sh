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
if ! sudo iptables -C FORWARD -i "$UPLINK" -o "$BR" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; then
  echo "[INFO] Adding FORWARD rule for RELATED,ESTABLISHED traffic from $UPLINK to $BR..."
  sudo iptables -A FORWARD -i "$UPLINK" -o "$BR" -m state --state RELATED,ESTABLISHED -j ACCEPT
  echo "[INFO] FORWARD rule (RELATED,ESTABLISHED) added"
else
  echo "[INFO] FORWARD rule (RELATED,ESTABLISHED) already exists, skipping"
fi
if ! sudo iptables -C FORWARD -i "$BR" -o "$UPLINK" -j ACCEPT 2>/dev/null; then
  echo "[INFO] Adding FORWARD rule for traffic from $BR to $UPLINK..."
  sudo iptables -A FORWARD -i "$BR" -o "$UPLINK" -j ACCEPT
  echo "[INFO] FORWARD rule added"
else
  echo "[INFO] FORWARD rule already exists, skipping"
fi

echo "[SUCCESS] Host network ready: bridge=$BR (${BR_IP}), NAT via $UPLINK"
