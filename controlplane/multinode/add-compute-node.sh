#!/usr/bin/env bash
# Register a new compute node with the control plane.
#
# Run this ON THE CONTROL-PLANE box (box-1). It:
#   1. adds a [Peer] block for the new node to /etc/wireguard/wg0.conf
#   2. hot-applies it (wg set) so no tunnel flap for existing peers
#   3. prints the wg0.conf the new node should use, plus the one-click
#      compute-role install command to run on it.
#
# Usage:
#   ./add-compute-node.sh <node-index> <node-public-ip> <node-wg-pubkey>
#
# Example (box-2, public IP 5.6.7.8, its wg pubkey from `wg genkey|wg pubkey`):
#   ./add-compute-node.sh 2 5.6.7.8 <box2-wg-pubkey>
#
# node-index 2 -> wg IP 10.10.0.2, index 3 -> 10.10.0.3, etc.
set -euo pipefail

IDX="${1:?node-index required (2,3,...)}"
NODE_PUB_IP="${2:?node public IP required}"
NODE_WG_PUBKEY="${3:?node wireguard public key required}"

WG_CONF=/etc/wireguard/wg0.conf
WG_IF=wg0
CONTROL_WG_IP=10.10.0.1
NODE_WG_IP="10.10.0.${IDX}"
CUBEMASTER_PORT=8089

[[ -f "$WG_CONF" ]] || { echo "missing $WG_CONF — run the control-plane WireGuard setup first" >&2; exit 1; }
CONTROL_PUB_IP="$(curl -s -4 ifconfig.me || echo '<control-public-ip>')"
CONTROL_WG_PUBKEY="$(cat /etc/wireguard/box1.pub)"

# 1. append peer to persistent config (idempotent on pubkey)
if ! grep -q "$NODE_WG_PUBKEY" "$WG_CONF"; then
  cat >> "$WG_CONF" <<EOF

[Peer]
# compute node ${IDX} (${NODE_PUB_IP})
PublicKey = ${NODE_WG_PUBKEY}
AllowedIPs = ${NODE_WG_IP}/32
EOF
fi

# 2. hot-apply (no restart — keeps existing peers up)
wg set "$WG_IF" peer "$NODE_WG_PUBKEY" allowed-ips "${NODE_WG_IP}/32"

echo "==> peer ${IDX} added: ${NODE_WG_IP} (pub ${NODE_PUB_IP})"
echo
echo "=== 1. Put this at /etc/wireguard/wg0.conf ON THE NEW NODE (box-${IDX}) ==="
echo "     (replace PrivateKey with that node's own private key)"
cat <<EOF
[Interface]
Address = ${NODE_WG_IP}/24
PrivateKey = <box-${IDX}-private-key>

[Peer]
PublicKey = ${CONTROL_WG_PUBKEY}
Endpoint = ${CONTROL_PUB_IP}:51820
AllowedIPs = 10.10.0.0/24
PersistentKeepalive = 25
EOF
echo
echo "   then: systemctl enable --now wg-quick@wg0"
echo "   verify: ping -c2 ${CONTROL_WG_IP}"
echo
echo "=== 2. Then run the compute-role install ON THE NEW NODE ==="
cat <<EOF
ONE_CLICK_DEPLOY_ROLE=compute \\
  ONE_CLICK_CONTROL_PLANE_IP=${CONTROL_WG_IP} \\
  CUBE_SANDBOX_NODE_IP=${NODE_WG_IP} \\
  bash deploy/one-click/install-compute.sh
EOF
echo
echo "   ONE_CLICK_CONTROL_PLANE_IP=${CONTROL_WG_IP} routes the cubelet's"
echo "   meta_server_endpoint over the tunnel — not the firewalled public port."
