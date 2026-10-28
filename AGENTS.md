# AGENTS.md

Instructions for AI agents working in this repository.

## Architecture

QuirkProxy is a Rust supervisor and reverse proxy wrapping `opencode2api` (core engine) and a userspace Cloudflare WARP egress pool:

1. **Rust Supervisor (`src/`)**:
   - Manages child process lifecycles: N `wireproxy` tunnels and `opencode2api`.
   - Listens on `:8080` (Axum + Tokio + Reqwest) and proxies all incoming traffic to `opencode2api` on `:18088`.
   - When `opencode2api` returns `HTTP 429` (Rate Limit) or a rotation-eligible `HTTP 403`, the supervisor recycles one tunnel in the background and retries the request, which lands on a different exit.
   - Handles graceful SIGTERM / SIGINT shutdown, keeping every child process reaped.

2. **Core Engine (`opencode2api`)**:
   - Unmodified official Go binary from `jasonxu114514/opencode2api`.
   - Handles 100% of OpenCode protocol translation, Bun uTLS fingerprinting, session IDs, and tool calling schemas.
   - Configured with one `"socks5://127.0.0.1:<port>#pool"` entry per tunnel. The engine builds one anonymous node per proxy and cycles `#pool` transports across retry attempts, so a burned exit is skipped instead of retried.

3. **WARP tunnels (`wireproxy` / `usque`)**:
   - Each identity is a separate WARP registration; `wireproxy` speaks WireGuard in userspace, or `usque` speaks MASQUE over HTTP/3 (UDP:443). Both expose SOCKS5 on loopback.
   - No `tun0`, no `NET_ADMIN`, no `/dev/net/tun`, no host route changes.
   - For WireGuard, `wgcf register` refuses to overwrite an existing account, so `re_register()` deletes `wgcf-account.toml` / `wgcf-profile.conf` first. For MASQUE, stale `config.json` is removed before `usque register`.
   - Profiles live in `WARP_STATE_DIR` (a volume). Losing the volume means new identities and new egress IPs on next boot.

4. **Refusal gate (`src/gate.rs`)**:
   - classifies upstream `403` bodies into quota / abuse / shape / geo / auth / version.
   - only `quota`, `abuse` and `unknown` are rotation-eligible; the rest would
     fail identically on a new egress and are returned to the client as-is.

5. **In-flight guard (`src/warp.rs`)**:
   - each streamed response holds an RAII guard until its final byte, and the
     guard is taken *before* the upstream call so requests still waiting on the
     upstream count as in flight as well.
   - operator-triggered rotations wait up to 30s for the pool to drain;
     request-triggered recycles skip the wait, because the triggering request
     has already failed.

6. **Startup ordering matters**:
   - `WarpPool::start()` blocks until at least one tunnel answers an egress
     probe (max 30s) before the engine is spawned. The engine fetches its model
     catalog once at startup; if it runs while every SOCKS5 port is still
     closed, the catalog stays empty until the next refresh window.
   - the readiness probe runs in its own task, so the supervisor keeps
     answering restart requests while a tunnel is still handshaking, and a
     recycle that arrives in that window is no longer lost.
   - duplicate egress detection waits until every tunnel's probe has finished
     before comparing addresses.

## Commands

- `make build` — Build Docker image `quirkproxy:v1.2`.
- `make run` — Run stack with docker compose.
- `make test` — Test endpoint `/healthz`.
- `make clean` — Stop and remove containers.
