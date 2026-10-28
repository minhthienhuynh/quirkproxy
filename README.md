# QuirkProxy v1.3.0

Ultra-lightweight Rust Supervisor & Auto-Healing Reverse Proxy for **OpenCode2API** with a **multi-identity Cloudflare WARP egress pool**.

## Highlights

- **Zero-Maintenance Core Engine**: Powered by upstream `opencode2api` (Go binary, v1.4.5). Automatically supports all upstream OpenCode gates (Bun uTLS fingerprinting, descending session IDs, genuine byte-matched tool schemas, and multi-protocol translation for OpenAI Chat, Responses, and Anthropic Messages).
- **Parallel WARP Egress Pool**: Instead of one identity rotated serially, the supervisor runs N independent WireGuard tunnels, each a separate WARP registration with its own egress IP. The engine receives one `socks5://...#pool` entry per tunnel, so a `429` on one exit retries on the *next* exit immediately instead of waiting ~30s for a re-registration that often returns the same address.
- **Userspace Tunnelling**: Tunnels run through `wireproxy`, a userspace WireGuard client. No `tun0`, no `NET_ADMIN`, no `/dev/net/tun`, no route changes — the host network stack and any host VPN/DNS/SSH setup stay untouched.
- **Background Recycle**: When an exit burns out, one tunnel is re-registered in the background while the others keep serving. The request path never blocks on a registration. The tunnel is picked and claimed atomically, so concurrent refusals never recycle the same tunnel or drop one of the recycles.
- **Refusal-Aware Rotation**: A `403` is not one condition. The supervisor classifies it into `quota` / `abuse` / `shape` / `geo` / `auth` / `version` and only recycles a tunnel for causes a new egress can actually fix.
- **Stream-Safe**: Every in-flight response holds a guard until its last byte, so a manual rotation drains the pool first instead of truncating another client's stream mid-token. The guard is taken before the upstream call, so requests still waiting on the upstream count as in flight too.
- **Duplicate Detection**: Cloudflare sometimes issues the same exit address to two registrations; the pool detects a duplicate at startup and recycles one. The comparison waits until every tunnel's readiness probe has finished, so a tunnel that has not produced an address yet is not mistaken for a unique one.
- **Single Container**: `wgcf`, `wireproxy`, `opencode2api` and the Rust supervisor live in one image. No sidecars, no extra proxy nodes, no dedupe arbiter.
- **Multi-Arch**: Dual native support for both `linux/amd64` and `linux/arm64` published as a multi-platform manifest list.

## Architecture

```
[ Client / 9Router / OmniRoute ]
             |
             v :8080 (PORT)
+--------------------------------------------------------------+
| Container: quirkproxy (no NET_ADMIN, no /dev/net/tun)        |
|                                                              |
|   [ Rust Supervisor (Axum) ]                                 |
|        |  - Reverse proxy :8080 -> :18088                    |
|        |  - On 429/403: recycle a tunnel in the background   |
|        |  - Retry lands on a different exit                  |
|        |                                                     |
|        +--> [ opencode2api :18088 ]                          |
|        |         |  "proxies": [40001#pool, 40002#pool, ...] |
|        |         v                                           |
|        +--> [ wireproxy x N (userspace WireGuard) ]          |
|                  :40001 --+                                  |
|                  :40002 --+--> WARP --> distinct egress IPs  |
|                  :40003 --+                                  |
+-----------------+--------------------------------------------+
                  v
         [ OpenCode Upstream ]
```

### Port Matrix

| Scope | Default Port | Environment Variable | Description |
| :--- | :--- | :--- | :--- |
| **Container (Public)** | `8080` | `PORT` / `QUIRKPROXY_PORT` | Main entrypoint served by the Rust supervisor |
| **Host Mapping** | `18080` | `QUIRK_HOST_PORT` | Port published to the VPS host (`127.0.0.1:18080`) |
| **Internal Engine** | `18088` | `OPENCODE2API_PORT` | Loopback port for the `opencode2api` core engine |
| **Tunnel Pool** | `40001..` | `WARP_BASE_SOCKS_PORT` | First SOCKS5 port; identity *i* uses base + *i* |

### Environment Variables

| Variable | Default | Meaning |
| :--- | :--- | :--- |
| `WARP_IDENTITIES` | `5` | Number of parallel WARP identities. `0` disables the pool. |
| `WARP_PROTOCOL` | `wireguard` | Tunnel egress protocol: `wireguard` (userspace `wireproxy`) or `masque` (HTTP/3 Connect-IP `usque`) |
| `WARP_BASE_SOCKS_PORT` | `40001` | First SOCKS5 port of the pool |
| `WARP_STATE_DIR` | `/app/warp` | Where per-identity WireGuard profiles are stored |
| `WARP_AUTO_ROTATE_429` | `true` | Recycle a tunnel on upstream `429` |
| `WARP_AUTO_ROTATE_403` | `true` | Recycle a tunnel on a rotation-eligible `403` |
| `MAX_RETRIES` | `1` with a multi-identity pool, else `3` | Upstream attempts per client request. The engine hops between pool nodes on its own, so an extra supervisor-level attempt only multiplies the leg count and the latency. |
| `UPSTREAM_API` | `http://127.0.0.1:18088` | Base URL of the `opencode2api` engine |
| `SERVER_KEY` | `local-test-key` | Bearer token for `POST /v1/rotate` |

> **Change `SERVER_KEY` before exposing the service.** The default is a
> well-known value, and the container listens on the shared `dokploy-network`,
> so any sibling container can reach `POST /v1/rotate` with it. The token is
> compared in constant time, but that only removes the timing side channel.

## Quick Start

```yaml
services:
  quirkproxy:
    image: ghcr.io/minhthienhuynh/quirkproxy:latest
    container_name: quirkproxy
    restart: unless-stopped
    ports:
      - "127.0.0.1:${QUIRK_HOST_PORT:-18080}:${PORT:-8080}"
    environment:
      - WARP_IDENTITIES=${WARP_IDENTITIES:-5}
      - SERVER_KEY=${SERVER_KEY:-local-test-key}
    volumes:
      - warp-identities:/app/warp
    # No cap_add / devices required.

volumes:
  warp-identities:
```

The `warp-identities` volume matters: identities are registered once on first
boot and reused afterwards. Without it, every container restart would register
new identities and rotate all egress IPs.

### Endpoints

- `GET /healthz` — Liveness & status. `status` is `healthy` (engine ready), `warming` (engine reachable, catalog still loading) or `degraded` (engine unreachable). Reports `inflight`, `engine_models`, and `warp.{identities,healthy,tunnels[]}` where each tunnel carries its own `ip` / `healthy` / `recycling`. There is no top-level egress IP: with a pool there is no single answer.
- `POST /v1/rotate` — Rotate every tunnel. Requires `Authorization: Bearer <SERVER_KEY>`. Drains in-flight streams first.
- `GET /v1/models` — Proxied to opencode2api.
- `POST /v1/chat/completions` — Proxied with automatic refusal-aware retry.
- `POST /v1/responses` — Proxied to the opencode2api Responses endpoint.
- `POST /v1/messages` — Proxied to the opencode2api Anthropic endpoint.

## Rotation policy

| Upstream refusal | Recycles a tunnel? | Why |
| :--- | :--- | :--- |
| `429` rate limit | yes | per-address quota |
| `403` quota / usage limit | yes | per-address quota |
| `403` abuse | yes | address flagged |
| `403` unrecognised | yes | most likely an exhausted address; one wasted recycle is cheaper than a hard failure |
| `403` request shape | **no** | headers/tools/session id — no address makes it pass |
| `403` region block | **no** | a new registration stays in the same colo |
| `403` auth / version | **no** | not an egress problem |

A recycle picks a tunnel that is free (not already re-registering, not inside
the 60 s cooldown) and claims it in the same step, so two concurrent `429`s
never end up recycling the same tunnel or silently dropping one of the
recycles.

## Turning the pool off

`WARP_ENABLED=false` (or `WARP_IDENTITIES=0`) disables the pool entirely: no
tunnel is started, and the engine is configured **without** SOCKS proxies so
it talks to the upstream directly. Pointing the engine at SOCKS endpoints that
nothing listens on would make every upstream call fail with
`all proxies are unavailable` instead.

## Development

```bash
cargo fmt --all -- --check
cargo clippy --all-targets -- -D warnings
cargo test --release
docker build -t quirkproxy:v1.2 .
```

CI runs exactly those four steps on every push and PR.
