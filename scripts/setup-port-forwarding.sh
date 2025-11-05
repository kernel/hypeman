#!/usr/bin/env bash
set -euo pipefail

# Configuration
UPLINK="${UPLINK:-eth0}"  # Can be overridden with env var
SUBNET_BASE="192.168.100"
HOST_PORT_START=8080
GUEST_PORT=8080

echo "========================================"
echo "Setting up Port Forwarding for WebRTC"
echo "========================================"
echo "Uplink: $UPLINK"
echo "Forwarding: localhost:8080-8089 -> VMs:8080"
echo ""

# Setup DNAT rules for each VM
for i in {1..10}; do
  HOST_PORT=$((HOST_PORT_START + i - 1))  # 8080, 8081, ..., 8089
  GUEST_IP="${SUBNET_BASE}.$((9 + i))"     # 192.168.100.10-19
  
  # Check if rule already exists
  if ! sudo iptables -t nat -C PREROUTING -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "${GUEST_IP}:${GUEST_PORT}" 2>/dev/null; then
    echo "[INFO] Adding port forward: localhost:$HOST_PORT -> $GUEST_IP:$GUEST_PORT"
    sudo iptables -t nat -A PREROUTING -p tcp --dport "$HOST_PORT" -j DNAT --to-destination "${GUEST_IP}:${GUEST_PORT}"
  else
    echo "[INFO] Port forward already exists: localhost:$HOST_PORT -> $GUEST_IP:$GUEST_PORT"
  fi
  
  # Also add rule for accessing from the host itself (localhost)
  if ! sudo iptables -t nat -C OUTPUT -p tcp --dport "$HOST_PORT" -d 127.0.0.1 -j DNAT --to-destination "${GUEST_IP}:${GUEST_PORT}" 2>/dev/null; then
    sudo iptables -t nat -A OUTPUT -p tcp --dport "$HOST_PORT" -d 127.0.0.1 -j DNAT --to-destination "${GUEST_IP}:${GUEST_PORT}"
  fi
done

echo ""
echo "========================================"
echo "Port Forwarding Configured!"
echo "========================================"
echo ""
echo "Access VMs via WebRTC:"
for i in {1..10}; do
  HOST_PORT=$((HOST_PORT_START + i - 1))
  GUEST_IP="${SUBNET_BASE}.$((9 + i))"
  echo "  VM $i (guest-$i): http://localhost:$HOST_PORT -> $GUEST_IP:$GUEST_PORT"
done
echo ""
echo "Test with: curl http://localhost:8080"
echo "========================================"

