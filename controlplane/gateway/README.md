# cube gateway — multi-tenant auth + metering plane

A small stdlib-only Go reverse proxy that turns a single CubeSandbox node into a
multi-tenant substrate. It sits in front of `cube-api` (`:3000`) and:

- **Authenticates** each request by API key → tenant (`X-API-KEY` or `Authorization: Bearer`).
- **Enforces quotas** on `POST /sandboxes`: per-tenant max concurrent sandboxes, and clamps `cpuCount` / `memoryMB` to the tenant plan.
- **Tags** every sandbox with `metadata.tenant` for downstream filtering.
- **Meters** sandbox lifetime (`create` → `destroy`) into a JSONL ledger that billing aggregates: sandbox-seconds, vCPU-seconds, GB-seconds.

Everything else is transparently proxied, so the stock `cubesandbox` / `e2b`
SDKs work unchanged — point them at the gateway and use the tenant key.

```
client (E2B_API_URL=gateway, key=tenant) ─▶ gateway :8088 ─▶ cube-api :3000 ─▶ CubeMaster/Cubelet
                                               │
                                               └─▶ usage.jsonl ─▶ (billing)
```

## Build

```bash
CGO_ENABLED=0 go build -o cube-gateway .
# cross-compile for a linux node:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cube-gateway .
```

## Run

```bash
cp tenants.example.json tenants.json   # edit keys/quotas
GATEWAY_ADMIN_TOKEN=secret ./cube-gateway \
  -addr :8088 -upstream http://127.0.0.1:3000 \
  -tenants tenants.json -usage /var/lib/cube-gateway/usage.jsonl
```

Config via flags or env: `GATEWAY_ADDR`, `CUBE_UPSTREAM`, `GATEWAY_TENANTS`,
`GATEWAY_USAGE_LOG`, `GATEWAY_ADMIN_TOKEN`.

## Use (tenant side)

```python
import os
from cubesandbox import Sandbox
os.environ["CUBE_API_URL"]     = "http://<gateway-host>:8088"
os.environ["CUBE_PROXY_NODE_IP"] = "<node-ip>"   # data plane still hits cube-proxy directly
os.environ["X-API-KEY"]        = "e2b_tenant_alpha_..."   # tenant key
with Sandbox.create(template="py313") as sb:
    print(sb.commands.run("python3 --version").stdout)
```

## Admin

```bash
curl -H "X-Admin-Token: secret" http://localhost:8088/admin/usage
curl -H "X-Admin-Token: secret" http://localhost:8088/admin/tenants
```

`/admin/usage` returns per-tenant `sandbox_seconds`, `vcpu_seconds`,
`gb_seconds` (closed + still-open intervals) and current open counts — the raw
inputs for metered billing.

## Scope / limitations (MVP)

- Control plane only: data-plane exec (`commands.run`, files) flows through
  `cube-proxy` directly, so metering is by **sandbox wall-time**, not per-exec.
- Pause/resume is not yet discounted — billed create→destroy.
- Tenants are a static JSON file; quota state (`usage.jsonl`) survives restarts
  via replay. A DB-backed tenant store + Stripe push are the next steps.
- Network isolation (per-tenant CubeVS egress policy) is a separate component.
