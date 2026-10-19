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

4. **Refusal gate (`src/gate.rs`)**:
   - classifies upstream `403` bodies into quota / abuse / shape / geo / auth / version.
   - only `quota`, `abuse` and `unknown` are rotation-eligible; the rest would
     fail identically on a new egress and are returned to the client as-is.

5. **In-flight guard (`src/warp.rs`)**:
   - each streamed response holds an RAII guard until its final byte.
   - operator-triggered rotations wait up to 30s for the tunnel to drain;
     request-triggered rotations skip the wait, because the triggering request
     has already failed.

## Commands

- `make build` — Build Docker image `quirkproxy:v1`.
- `make run` — Run stack with docker compose.
- `make test` — Test endpoint `/healthz`.
- `make clean` — Stop and remove containers.
