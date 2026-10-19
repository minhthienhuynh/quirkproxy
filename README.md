# QuirkProxy v1

Ultra-lightweight Rust Supervisor & Auto-Healing Reverse Proxy for **OpenCode2API** with integrated Cloudflare WARP SOCKS5 IP rotation.

## Highlights

- **Zero-Maintenance Core Engine**: Powered by upstream `opencode2api` (Go 1.24+ binary, v1.4.2). Automatically supports all upstream OpenCode gates (Bun uTLS fingerprinting, descending session IDs, genuine byte-matched tool schemas, and multi-protocol translation for OpenAI Chat, Responses, and Anthropic Messages).
- **Auto-Healing IP Rotation in Rust**: Implemented in async Rust (Axum + Tokio + Reqwest) as a high-performance PID 1 supervisor. When upstream returns `HTTP 429` (Rate Limit) or a rotation-eligible `HTTP 403`, the supervisor immediately triggers Cloudflare WARP re-registration to draw a new `104.28.*.*` egress IP, and transparently retries the request.
- **Refusal-Aware Rotation**: A `403` is not one condition. The supervisor classifies the message into `quota` / `abuse` / `shape` / `geo` / `auth` / `version` and only rotates for the causes a new egress can actually fix. Rotating for a request-shape or region refusal spends a registration and ~5s while guaranteeing the same failure.
- **Stream-Safe Rotation**: Every in-flight response holds a guard until its last byte, so a manual or scheduled rotation waits for the tunnel to drain instead of truncating another client's stream mid-token. `GET /healthz` exposes the live `inflight` count.
- **Single Container Architecture**: Bundles `warp-svc`, `opencode2api`, and the Rust supervisor into a single Docker image without sidecars, extra proxy nodes, or dedupe arbiters.
- **Zero Host Network Disruption**: WARP runs strictly in `mode proxy` (SOCKS5 on internal port `127.0.0.1:40000`). It does not create `tun0` virtual network interfaces, does not modify host routing tables, and leaves VPN / DNS / SSH untouched.
- **High Concurrency & Minimal Footprint**: Rust supervisor consumes less than 15MB of RAM with sub-millisecond proxy latency.

## Architecture & Port Scheme

```
[ Client / 9Router / OmniRoute ]
             │
             ▼ :8080 (PORT)
┌───────────────────────────────────────────────────────────────┐
│ Container: quirkproxy                                         │
│                                                               │
│   [ Rust Supervisor (Axum) ]                                  │
│        │  - Reverse proxy :8080 -> :18088                     │
│        │  - Intercepts 429/403 -> rotates WARP -> retries     │
│        │                                                      │
│        ├──> [ opencode2api (:18088) ] (OPENCODE2API_PORT)     │
│        │         │                                            │
│        │         ▼ "proxies": ["socks5://127.0.0.1:40000#pool"]│
│        │                                                      │
│        └──> [ Cloudflare WARP (:40000) ] (WARP_SOCKS_PORT)    │
│                  │                                            │
│                  ▼ Egress IP 104.28.*.*                       │
└──────────────────┬────────────────────────────────────────────┘
                   ▼
         [ OpenCode Upstream ]
```

### Port Matrix

| Scope | Default Port | Environment Variable | Description |
| :--- | :--- | :--- | :--- |
| **Container (Public)** | `8080` | `PORT` / `QUIRKPROXY_PORT` | Main entrypoint served by the Rust supervisor |
| **Host Mapping** | `18080` | `QUIRK_HOST_PORT` | Port published to VPS host (`127.0.0.1:18080`) |
| **Internal Engine** | `18088` | `OPENCODE2API_PORT` | Loopback port for `opencode2api` core engine |
| **Internal WARP** | `40000` | `WARP_SOCKS_PORT` | Loopback SOCKS5 proxy port for `warp-svc` |

## Quick Start

### Docker Compose

```yaml
services:
  quirkproxy:
    image: quirkproxy:v1
    container_name: quirkproxy
    restart: unless-stopped
    ports:
      - "127.0.0.1:${QUIRK_HOST_PORT:-18080}:${PORT:-8080}"
    environment:
      - HOST=${HOST:-0.0.0.0}
      - PORT=${PORT:-8080}
      - OPENCODE2API_PORT=${OPENCODE2API_PORT:-18088}
      - WARP_SOCKS_PORT=${WARP_SOCKS_PORT:-40000}
      - WARP_ENABLED=${WARP_ENABLED:-true}
      - WARP_AUTO_ROTATE_429=${WARP_AUTO_ROTATE_429:-true}
      - WARP_AUTO_ROTATE_403=${WARP_AUTO_ROTATE_403:-true}
      - WARP_ACCEPT_TOS=1
      - SERVER_KEY=${SERVER_KEY:-local-test-key}
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun:/dev/net/tun
```

### Endpoints

- `GET /healthz` — Liveness & status probe. `status` is `healthy` (engine ready), `warming` (engine reachable but its model catalog is still loading) or `degraded` (engine unreachable). Also reports `inflight` and `engine_models`.
- `POST /v1/rotate` — Trigger a WARP IP rotation. Requires `Authorization: Bearer $SERVER_KEY`; waits for in-flight streams to drain first.
- `GET /v1/models` — Proxied directly to opencode2api.
- `POST /v1/chat/completions` — Proxied with automatic refusal-aware 429/403 retry.
- `POST /v1/responses` — Proxied to opencode2api Responses endpoint.
- `POST /v1/messages` — Proxied to opencode2api Anthropic endpoint.

## Rotation policy

| Upstream refusal | Rotates? | Why |
| :--- | :--- | :--- |
| `429` rate limit | yes | per-address quota |
| `403` quota / usage limit | yes | per-address quota |
| `403` abuse | yes | address flagged |
| `403` unrecognised | yes | most likely an exhausted address; one wasted rotation is cheaper than a hard failure |
| `403` request shape | **no** | headers/tools/session id — no address makes it pass |
| `403` region block | **no** | a new registration stays in the same colo |
| `403` auth / version | **no** | not an egress problem |

## Development

```bash
cargo fmt --all -- --check
cargo clippy --all-targets -- -D warnings
cargo test --release
docker build -t quirkproxy:v1 .
```

CI runs exactly those four steps on every push and PR.
