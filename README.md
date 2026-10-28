# QuirkProxy v2.0.0

Ultra-lightweight Rust Supervisor & Auto-Healing Reverse Proxy powered by **Zen Proxy (Bun Engine)** with a **multi-identity Cloudflare WARP egress pool**.

## Highlights

- **BoringSSL Native TLS Fingerprint**: Powered by `zen-proxy` executed under **Bun runtime**. Matches 100% of OpenCode official CLI's TLS fingerprint to eliminate upstream bot detection and 429 quota-zeroing penalties.
- **OpenCode 2.1.4 Wire Compatibility**: Full support for descending session IDs (`ses_...`), official 12-tool manifests, environment blocks (`<env>`), and multi-protocol translation (OpenAI Chat Completions, Responses, and Anthropic Messages for Claude Code CLI).
- **Parallel WARP Egress Pool**: The supervisor runs N independent tunnels (userspace MASQUE HTTP/3 or WireGuard), each a separate WARP registration with its own egress IP. Requests cycle across the pool and hop exits on rate limits.
- **Targeted Rotation & Auto-Healing (/v1/rotate/next)**: Automatically detects dead, unhealthy, banned, or exhausted tunnels and replaces only the damaged egress while preserving active warm tunnels.
- **Userspace Tunnelling**: Tunnels run through `usque` (MASQUE over HTTP/3) or `wireproxy`. No `tun0`, no `NET_ADMIN`, no `/dev/net/tun`, no route changes — the host network stack and any host VPN/DNS/SSH setup stay untouched.
- **Stream-Safe**: In-flight responses hold a connection drain guard so tunnel recycling never truncates client streams mid-token.
- **Single Container**: Bun, `usque`, `wireproxy`, and the Rust supervisor live in one image. No sidecars, no extra proxy nodes, no dedupe arbiter.
- **Multi-Arch**: Dual native support for both `linux/amd64` and `linux/arm64` published as a multi-platform manifest list.

## Architecture

```
[ Client / Hermes / 9Router / OmniRoute / Claude Code ]
             |
             v :8080 (PORT)
+--------------------------------------------------------------+
| Container: quirkproxy (no NET_ADMIN, no /dev/net/tun)        |
|                                                              |
|   [ Rust Supervisor (Axum) ]                                 |
|        |  - Reverse proxy :8080 -> :18088                    |
|        |  - Watchdog & Targeted Rotation (/v1/rotate/next)   |
|        |  - Preserves warm tunnels, auto-heals dead ones     |
|        |                                                     |
|        +--> [ Zen Proxy on Bun :18088 ]                      |
|        |         |  BoringSSL TLS Fingerprint                |
|        |         |  Auto Wire Spec 2.1.4                     |
|        |         v                                           |
|        +--> [ usque / wireproxy x N (HTTP Egress Pool) ]     |
|                  :40001 --+                                  |
|                  :40002 --+--> Cloudflare WARP --> Egress IPs|
|                  :40010 --+                                  |
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
| `WARP_IDENTITIES` | `10` | Number of parallel WARP identities. `0` disables the pool. |
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
      - WARP_IDENTITIES=${WARP_IDENTITIES:-10}
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
- `POST /v1/rotate` — Rotate every tunnel in the pool. Requires `Authorization: Bearer $SERVER_KEY`. Drains in-flight streams first.
- `POST /v1/rotate/next` (or `POST /v1/rotate?mode=next`) — Targeted replacement: detects, drains, and recycles dead (`unhealthy`), `banned`, or `quota_exhausted` tunnels while keeping healthy tunnels active. If all tunnels are active and healthy, returns a `noop` response preserving warm IP quota unless forced via `?force=true`. Supports explicit targeting via `?tunnel=<index>`.
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
