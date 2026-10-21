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
ENV WARP_IDENTITIES=3
ENV WARP_STATE_DIR=/app/warp
ENV WARP_AUTO_ROTATE_429=true
ENV WARP_AUTO_ROTATE_403=true
ENV OPENCODE2API_BIN=/usr/local/bin/opencode2api
ENV OPENCODE2API_CONFIG=/app/config.json
ENV OPENCODE2API_VERSION=v1.4.3
ENV WGCF_VERSION=v2.3.1
ENV WIREPROXY_VERSION=v1.1.3

# Runtime deps only: wireproxy is userspace WireGuard, so no tun device,
# no NET_ADMIN and no dbus/warp-svc daemon are needed.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    procps \
    && rm -rf /var/lib/apt/lists/*

# 1. Install wgcf (WARP identity) and wireproxy (userspace WireGuard -> SOCKS5)
RUN ARCH=$(dpkg --print-architecture) \
    && case "$ARCH" in \
         amd64) WG_ARCH=amd64; WP_ARCH=amd64 ;; \
         arm64) WG_ARCH=arm64; WP_ARCH=arm64 ;; \
         *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /usr/local/bin/wgcf \
        "https://github.com/ViRb3/wgcf/releases/download/${WGCF_VERSION}/wgcf_${WGCF_VERSION#v}_linux_${WG_ARCH}" \
    && curl -fsSL -o /tmp/wireproxy.tar.gz \
        "https://github.com/windtf/wireproxy/releases/download/${WIREPROXY_VERSION}/wireproxy_linux_${WP_ARCH}.tar.gz" \
    && tar -xzf /tmp/wireproxy.tar.gz -C /usr/local/bin wireproxy \
    && rm -f /tmp/wireproxy.tar.gz \
    && chmod +x /usr/local/bin/wgcf /usr/local/bin/wireproxy

# 2. Download official opencode2api release binary
RUN ARCH=$(dpkg --print-architecture) \
    && echo "Downloading opencode2api ${OPENCODE2API_VERSION} for ${ARCH}..." \
    && curl -fsSL "https://github.com/jasonxu114514/opencode2api/releases/download/${OPENCODE2API_VERSION}/opencode2api_${OPENCODE2API_VERSION}_linux_${ARCH}.tar.gz" | tar -xz -C /tmp \
    && mv /tmp/opencode2api_${OPENCODE2API_VERSION}_linux_${ARCH}/opencode2api /usr/local/bin/opencode2api \
    && chmod +x /usr/local/bin/opencode2api \
    && rm -rf /tmp/opencode2api*

WORKDIR /app
RUN mkdir -p /app/warp

# 3. Copy Rust supervisor binary from builder
COPY --from=builder /build/target/release/quirkproxy /usr/local/bin/quirkproxy
RUN chmod +x /usr/local/bin/quirkproxy

EXPOSE 8080

# Health check hits the supervisor, which reports engine and tunnel state.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/quirkproxy"]
