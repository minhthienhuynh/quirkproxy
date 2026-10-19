# AGENTS.md

Instructions for AI agents working in this repository.

## Architecture

QuirkProxy is a Rust-based supervisor and reverse proxy wrapping `opencode2api` (core engine) and Cloudflare WARP (SOCKS5 egress):

1. **Rust Supervisor (`src/`)**:
   - Manages child process lifecycles: `warp-svc` and `opencode2api`.
   - Listens on `:8080` (Axum + Tokio + Reqwest) and proxies all incoming traffic to `opencode2api` on `:18088`.
   - When `opencode2api` returns `HTTP 429` (Rate Limit) or `HTTP 403` (Abuse / FreeTierError), the supervisor invokes `warp-cli registration new` to draw a fresh Cloudflare egress IP, and retries the request transparently.
   - Handles graceful SIGTERM / SIGINT shutdown, ensuring all child processes are killed properly.

2. **Core Engine (`opencode2api`)**:
   - Unmodified official Go binary from `jasonxu114514/opencode2api`.
   - Handles 100% of OpenCode protocol translation, Bun uTLS fingerprinting, session IDs, and tool calling schemas.
   - Configured with `"proxies": ["socks5://127.0.0.1:40000#pool"]` so it never penalizes proxies with failure cooldowns and draws fresh TCP/CONNECT tunnels per request.

3. **Cloudflare WARP Client (`warp-svc`)**:
   - Runs in `mode proxy` listening on loopback port `40000`.
   - Never modifies host network interfaces or routing tables.

## Commands

- `make build` — Build Docker image `quirkproxy:v1`.
- `make run` — Run stack with docker compose.
- `make test` — Test endpoint `/healthz`.
- `make clean` — Stop and remove containers.
