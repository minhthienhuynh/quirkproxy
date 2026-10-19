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

RUN cargo build --release && \
    strip target/release/quirkproxy 2>/dev/null || true

# ==============================================================================
# Stage 2: Minimal Runtime with Cloudflare WARP and opencode2api Core Engine
# ==============================================================================
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive
ENV HOST=0.0.0.0
ENV PORT=8080
ENV OPENCODE2API_PORT=18088
ENV WARP_ENABLED=true
ENV WARP_SOCKS_PORT=40000
ENV WARP_AUTO_ROTATE_429=true
ENV WARP_AUTO_ROTATE_403=true
ENV OPENCODE2API_BIN=/usr/local/bin/opencode2api
ENV OPENCODE2API_CONFIG=/app/config.json
ENV OPENCODE2API_VERSION=v1.4.2

# 1. Install base utilities and Cloudflare WARP client
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    gnupg \
    lsb-release \
    dbus \
    ca-certificates \
    procps \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL https://pkg.cloudflareclient.com/pubkey.gpg | gpg --yes --dearmor --output /usr/share/keyrings/cloudflare-warp-archive-keyring.gpg \
    && echo "deb [signed-by=/usr/share/keyrings/cloudflare-warp-archive-keyring.gpg] https://pkg.cloudflareclient.com/ $(lsb_release -cs) main" | tee /etc/apt/sources.list.d/cloudflare-client.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends cloudflare-warp \
    && rm -rf /var/lib/apt/lists/*

# 2. Download official opencode2api release binary
RUN ARCH=$(dpkg --print-architecture) \
    && echo "Downloading opencode2api ${OPENCODE2API_VERSION} for ${ARCH}..." \
    && curl -sL "https://github.com/jasonxu114514/opencode2api/releases/download/${OPENCODE2API_VERSION}/opencode2api_${OPENCODE2API_VERSION}_linux_${ARCH}.tar.gz" | tar -xz -C /tmp \
    && mv /tmp/opencode2api_${OPENCODE2API_VERSION}_linux_${ARCH}/opencode2api /usr/local/bin/opencode2api \
    && chmod +x /usr/local/bin/opencode2api \
    && rm -rf /tmp/opencode2api*

WORKDIR /app

# 3. Copy Rust supervisor binary from builder
COPY --from=builder /build/target/release/quirkproxy /usr/local/bin/quirkproxy
RUN chmod +x /usr/local/bin/quirkproxy

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/quirkproxy"]
