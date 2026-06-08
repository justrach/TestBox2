# Multi-node scale-out & control-plane hardening

How to grow a single CubeSandbox box into a multi-node cluster, and the
hardening that makes the control plane safe to expose. Companion to the
`controlplane/gateway` (auth + metering) work.

## Topology

```
        ┌─────────────── box-1 = CONTROL PLANE ───────────────┐
client ─▶ gateway :8088 ─▶ cube-api :3000 ─▶ cubemaster :8089 ─▶ MySQL/redis (127.0.0.1)
        │  (auth/quota/meter)   (no native auth)   (scheduler)        (state)        │
        └─────────────────────────────┬──────────────────────────┘
                                            │ cubelet heartbeat + scheduling
                                            │ over WireGuard wg0 → 10.10.0.1:8089
        ┌────────────────────────────▼────────────────────────┐
        │  box-2 = COMPUTE NODE:  network-agent + cubelet → runs microVMs  │
        └────────────────────────────────────────────┘
```

A compute node runs **only** network-agent + cubelet — no MySQL/redis/cube-api.
The `control` vs `compute` split is built in: `install-compute.sh` just sets
`ONE_CLICK_DEPLOY_ROLE=compute`. The cubelet registers with cubemaster by
heartbeat; the scheduler (`scheduler_label: default-cluster`) then places
sandboxes across all registered nodes. The gateway needs **zero changes** — it
already fronts the whole cluster.

## How many nodes for "reliable"?

"Reliable" splits into two axes that need different node counts:

| Goal | Topology | Nodes |
|---|---|---|
| Prove scale-out works | 1 control + 1 compute | 2 |
| **Compute** fault tolerance (N+1) | 1 control + 2 compute | **3** |
| **No single point of failure** | 3 control (HA) + 2 compute | **~5** |

The control plane is a **single point of failure** today: cubemaster + MySQL +
redis + cube-api + gateway all live on box-1. No number of compute nodes fixes
that — if box-1 dies, nothing schedules. Reaching the ~5-node tier requires:

- **MySQL** → primary/replica or Galera quorum (or a managed DB)
- **redis** → sentinel/cluster quorum (or managed)
- **cubemaster** → active/passive failover *(unverified — may be single-scheduler by design; confirm before relying on it)*
- **gateway** → stateless except `usage.jsonl` is a **local file**; move metering to a shared DB before running >1 gateway behind a load balancer

The gateway-metering and DB-backed-tenants migrations are the same work, so the
3→5 jump pairs naturally with the billing roadmap.

## Control-plane hardening (`controlplane/firewall/`)

cube-api (:3000, **no native auth**) and cubemaster (:8089) must not be exposed.
`cube-firewall.sh` DROPs inbound 3000/8089 on the public NIC only — loopback,
wg0, and the gateway (:8088) stay reachable. Installed as a boot oneshot:

```bash
install -m744 controlplane/firewall/cube-firewall.sh /usr/local/sbin/cube-firewall.sh
install -m644 controlplane/firewall/cube-firewall.service /etc/systemd/system/cube-firewall.service
systemctl enable --now cube-firewall
```

Verify from off-box: `:8088` answers (401 without a key); `:3000` and `:8089`
time out.

## WireGuard tunnel (control-plane network)

Box-1↔box-N traffic crosses the public internet (no provider private net), so
cubemaster is reached over an encrypted tunnel rather than the exposed port.
Addressing: `10.10.0.0/24`, control = `10.10.0.1`, compute nodes = `.2, .3, ...`

One-time control-plane setup (already applied on box-1):

```bash
apt-get install -y wireguard wireguard-tools
wg genkey | tee /etc/wireguard/box1.key | wg pubkey > /etc/wireguard/box1.pub
# /etc/wireguard/wg0.conf: [Interface] Address=10.10.0.1/24 ListenPort=51820 PrivateKey=<box1.key>
systemctl enable --now wg-quick@wg0
```

`:51820/udp` is intentionally open on the public NIC — it's the encrypted join
point. cubemaster already binds `0.0.0.0:8089`, so it's reachable on wg0 with no
rebind.

## Add a compute node (runbook)

On the **new node**: install WireGuard, `wg genkey | wg pubkey` to get its
pubkey. Then on the **control-plane box**:

```bash
controlplane/multinode/add-compute-node.sh <index> <node-public-ip> <node-wg-pubkey>
# e.g. ./add-compute-node.sh 2 5.6.7.8 <box2-wg-pubkey>
```

This adds the peer (hot, no tunnel flap) and prints (1) the `wg0.conf` for the
new node and (2) the compute-role install command:

```bash
ONE_CLICK_DEPLOY_ROLE=compute \
  ONE_CLICK_CONTROL_PLANE_IP=10.10.0.1 \
  CUBE_SANDBOX_NODE_IP=10.10.0.2 \
  bash deploy/one-click/install-compute.sh
```

Pointing `ONE_CLICK_CONTROL_PLANE_IP` at the wg IP (`10.10.0.1`) routes the
cubelet's `meta_server_endpoint` over the tunnel, not the firewalled public port.

### Verify the node joined

```bash
wg show                       # on box-1: handshake with the new peer
# scheduler should now show 2 hosts; create a sandbox and confirm it can land on box-2:
curl -s -H 'X-API-KEY: <tenant-key>' http://127.0.0.1:8088/sandboxes
```

## Status

- [x] gateway runs under systemd (`cube-gateway.service`), survives reboot
- [x] firewall: 3000/8089 dropped on public NIC, boot-persistent
- [x] WireGuard endpoint on box-1 (`10.10.0.1/24`), ready for peers
- [x] add-node runbook + helper script
- [ ] provision box-2 and run the compute install (needs a real second box)
- [ ] HA control plane (MySQL/redis quorum, cubemaster failover, gateway metering → DB)
