#!/usr/bin/env bash
# Lock control-plane-internal services off the public NIC.
#
# cube-api (:3000) has NO native auth and cubemaster (:8089) is the cluster
# scheduler — neither should be reachable from the public internet. Clients go
# through the gateway (:8088); compute nodes reach cubemaster over the WireGuard
# tunnel (wg0, 10.10.0.0/24), NOT the public interface. So we DROP inbound
# 3000/8089 on the public NIC only. Loopback and wg0 are unaffected.
#
# Idempotent: safe to run repeatedly (used as a boot oneshot via
# cube-firewall.service). Set CUBE_PUBLIC_IF if the public NIC is not enp35s0.
set -euo pipefail

PUB_IF="${CUBE_PUBLIC_IF:-enp35s0}"

for port in 3000 8089; do
  if ! iptables -C INPUT -i "$PUB_IF" -p tcp --dport "$port" -j DROP 2>/dev/null; then
    iptables -A INPUT -i "$PUB_IF" -p tcp --dport "$port" -j DROP
  fi
done

# NOTE: WireGuard UDP :51820 is intentionally left OPEN on the public NIC — it
# is the encrypted entry point compute nodes use to join the tunnel.
