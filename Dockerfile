# ==============================================================================
# Stage 1: Build the Rust Supervisor on Ubuntu 22.04 for glibc compatibility
# ==============================================================================
FROM ubuntu:22.04 AS builder

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    ca-certificates \
    build-essential \
    pkg-config \
    libssl-dev \
    && rm -rf /var/lib/apt/lists/*

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable
ENV PATH="/root/.cargo/bin:${PATH}"

WORKDIR /build

COPY Cargo.toml Cargo.lock ./
COPY src/ ./src/

# Release profile already sets strip = true and panic = abort (see Cargo.toml).
# --locked keeps the build on exactly the dependency versions in Cargo.lock.
RUN cargo build --release --locked

# ==============================================================================
# Stage 2: Minimal Runtime with userspace WARP tunnels and opencode2api engine
# ==============================================================================
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive
ENV HOST=0.0.0.0
ENV PORT=8080
ENV OPENCODE2API_PORT=18088
ENV WARP_ENABLED=true
# First SOCKS5 port of the pool; identity i (0-based) listens on BASE + i.
ENV WARP_BASE_SOCKS_PORT=40001
ENV WARP_IDENTITIES=10
ENV WARP_STATE_DIR=/app/warp
ENV WARP_PROTOCOL=wireguard
ENV WARP_AUTO_ROTATE_429=true
ENV WARP_AUTO_ROTATE_403=true
ENV ENGINE_BIN=/usr/local/bin/bun
ENV ENGINE_SCRIPT=/app/engine/zen-proxy.mjs
ENV ENGINE_CONFIG=/app/zen-proxy.json
ENV BUN_VERSION=1.4.4
ENV WGCF_VERSION=v2.3.1
ENV WIREPROXY_VERSION=v1.1.3
ENV USQUE_VERSION=v4.2.1

# Runtime deps only: wireproxy and usque are userspace tools, so no tun device,
# no NET_ADMIN and no dbus/warp-svc daemon are needed.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    procps \
    unzip \
    && rm -rf /var/lib/apt/lists/*

# 1. Install wgcf (WARP identity) and wireproxy (userspace WireGuard -> SOCKS5)
RUN ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in \
         amd64) \
           WG_ARCH=amd64; WP_ARCH=amd64; \
           WGCF_SHA256="4fb7a136aa470ece11d17949c7db07b040ec9c604742dc8680bcf1889f85d0c8"; \
           WP_SHA256="e88c1d090740373fc606c1bafd81d9a5eadc642cce5667616e20e9d7a444f51c" ;; \
         arm64) \
           WG_ARCH=arm64; WP_ARCH=arm64; \
           WGCF_SHA256="710ab2d8ac145d81a4cf0c11bdb0e05914f77ae2a45f67b87e718d84b4cb7301"; \
           WP_SHA256="370e00bd2167960d1ecd1c3c1439715bbaa94a0a110a2040468670c9af6021b6" ;; \
         *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /usr/local/bin/wgcf \
        "https://github.com/ViRb3/wgcf/releases/download/${WGCF_VERSION}/wgcf_${WGCF_VERSION#v}_linux_${WG_ARCH}" \
    && echo "${WGCF_SHA256}  /usr/local/bin/wgcf" | sha256sum -c - \
    && curl -fsSL -o /tmp/wireproxy.tar.gz \
        "https://github.com/windtf/wireproxy/releases/download/${WIREPROXY_VERSION}/wireproxy_linux_${WP_ARCH}.tar.gz" \
    && echo "${WP_SHA256}  /tmp/wireproxy.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/wireproxy.tar.gz -C /usr/local/bin wireproxy \
    && rm -f /tmp/wireproxy.tar.gz \
    && chmod +x /usr/local/bin/wgcf /usr/local/bin/wireproxy

# 2. Install usque (Cloudflare WARP via MASQUE HTTP/3 protocol)
RUN ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in \
         amd64) USQUE_SHA256="4117e20695078af9c11edecd1a826c009bbc7ea0b7f64458612b4198910bc313" ;; \
         arm64) USQUE_SHA256="c88b061c2a567f30813d7505637d1fb6fe7ec5898b5b61fd05122409fa5ad925" ;; \
         *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/usque.zip \
        "https://github.com/Diniboy1123/usque/releases/download/${USQUE_VERSION}/usque_${USQUE_VERSION#v}_linux_${ARCH}.zip" \
    && echo "${USQUE_SHA256}  /tmp/usque.zip" | sha256sum -c - \
    && unzip -q -o /tmp/usque.zip -d /tmp \
    && mv /tmp/usque /usr/local/bin/usque \
    && chmod +x /usr/local/bin/usque \
    && rm -rf /tmp/usque*

# 3. Install Bun runtime
RUN ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in \
         amd64) BUN_ARCH="x64"; BUN_SHA256="bf8ccbfb2b0b80159e0f9a6d15c55317add8077534dbc6130a3edb52fccb395c" ;; \
         arm64) BUN_ARCH="aarch64"; BUN_SHA256="baa6bb36f51d6e58561e02bbea825038a9df454f673d6d776e8bbf86f8c7850e" ;; \
         *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/bun.zip "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-${BUN_ARCH}.zip" \
    && echo "${BUN_SHA256}  /tmp/bun.zip" | sha256sum -c - \
    && unzip -q -o /tmp/bun.zip -d /tmp \
    && mv /tmp/bun-linux-${BUN_ARCH}/bun /usr/local/bin/bun \
    && chmod +x /usr/local/bin/bun \
    && rm -rf /tmp/bun*

WORKDIR /app
RUN mkdir -p /app/warp /app/engine

# 4. Copy Zen Proxy engine assets
COPY engine/ /app/engine/

# 5. Copy Rust supervisor binary from builder
COPY --from=builder /build/target/release/quirkproxy /usr/local/bin/quirkproxy
RUN chmod +x /usr/local/bin/quirkproxy

EXPOSE 8080

# Health check hits the supervisor, which reports engine and tunnel state.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/quirkproxy"]
