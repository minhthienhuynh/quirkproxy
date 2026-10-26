use std::env;

/// Reads a boolean env var, accepting `1/true/yes/on` (case-insensitive).
fn env_bool(key: &str, default: bool) -> bool {
    match env::var(key) {
        Ok(v) => matches!(
            v.trim().to_ascii_lowercase().as_str(),
            "1" | "true" | "yes" | "on"
        ),
        Err(_) => default,
    }
}

/// Reads a numeric env var, falling back to `default` on absence or parse error.
fn env_parse<T: std::str::FromStr>(key: &str, default: T) -> T {
    env::var(key)
        .ok()
        .and_then(|v| v.trim().parse().ok())
        .unwrap_or(default)
}

#[derive(Clone, Debug)]
pub struct Config {
    /// Public listen host (default: 0.0.0.0)
    pub host: String,
    /// Public listen port (default: 8080)
    pub port: u16,
    /// Internal loopback port for opencode2api core engine (default: 18088)
    pub opencode_api_port: u16,
    /// Upstream API base URL (default: http://127.0.0.1:18088)
    pub upstream_api: String,
    /// First SOCKS5 port of the WARP tunnel pool (default: 40001).
    /// Tunnel `i` (0-based) listens on `warp_base_socks_port + i`.
    pub warp_base_socks_port: u16,
    /// Number of independent WARP identities/tunnels to run (default: 3).
    /// Set to 0 to disable WARP entirely.
    pub warp_identities: usize,
    /// Whether Cloudflare WARP tunnels are enabled (default: true)
    pub warp_enabled: bool,
    /// Auto-rotate a WARP identity when receiving HTTP 429 Rate Limit (default: true)
    pub warp_auto_rotate_429: bool,
    /// Auto-rotate a WARP identity when receiving HTTP 403 Abuse / FreeTierError (default: true)
    pub warp_auto_rotate_403: bool,
    /// Directory holding per-identity WireGuard profiles (default: /app/warp)
    pub warp_state_dir: String,
    /// Max retry attempts after IP rotation (default: 3)
    pub max_retries: usize,
    /// Binary path for opencode2api
    pub opencode2api_bin: String,
    /// Config path for opencode2api
    pub opencode2api_config: String,
    /// Local gateway server key
    pub server_key: String,
}

impl Config {
    pub fn from_env() -> Self {
        let host = env::var("HOST")
            .or_else(|_| env::var("QUIRKPROXY_HOST"))
            .unwrap_or_else(|_| "0.0.0.0".to_string());

        let port = env::var("PORT")
            .or_else(|_| env::var("QUIRKPROXY_PORT"))
            .ok()
            .and_then(|p| p.parse().ok())
            .unwrap_or(8080);

        let opencode_api_port = env_parse("OPENCODE2API_PORT", 18088);
        let upstream_api = env::var("UPSTREAM_API")
            .unwrap_or_else(|_| format!("http://127.0.0.1:{}", opencode_api_port));

        // WARP_IDENTITIES=0 is a deliberate "no tunnels" switch, so it must not
        // fall back to the default the way a missing variable would.
        let warp_identities = env_parse::<usize>("WARP_IDENTITIES", 5);

        // The engine builds one anonymous node per pool entry and hops
        // between them on its own retries. A supervisor-level retry on top of
        // that multiplies the leg count (engine legs x supervisor attempts)
        // and the latency without reaching a different exit faster, so the
        // default is one attempt as soon as the pool has more than one
        // identity. A single-identity pool has no engine-side hopping, so the
        // supervisor retry is the only retry there.
        let default_max_retries = if warp_identities > 1 { 1 } else { 3 };

        Self {
            host,
            port,
            opencode_api_port,
            upstream_api,
            warp_base_socks_port: env_parse("WARP_BASE_SOCKS_PORT", 40001),
            warp_identities,
            warp_enabled: env_bool("WARP_ENABLED", true) && warp_identities > 0,
            warp_auto_rotate_429: env_bool("WARP_AUTO_ROTATE_429", true),
            warp_auto_rotate_403: env_bool("WARP_AUTO_ROTATE_403", true),
            warp_state_dir: env::var("WARP_STATE_DIR").unwrap_or_else(|_| "/app/warp".to_string()),
            max_retries: env_parse("MAX_RETRIES", default_max_retries).max(1),
            opencode2api_bin: env::var("OPENCODE2API_BIN")
                .unwrap_or_else(|_| "/usr/local/bin/opencode2api".to_string()),
            opencode2api_config: env::var("OPENCODE2API_CONFIG")
                .unwrap_or_else(|_| "/app/config.json".to_string()),
            server_key: env::var("SERVER_KEY").unwrap_or_else(|_| "local-test-key".to_string()),
        }
    }

    /// SOCKS5 port for tunnel `index` (0-based).
    pub fn tunnel_port(&self, index: usize) -> u16 {
        self.warp_base_socks_port + index as u16
    }

    /// Every SOCKS5 proxy URL, in the `#pool` form the engine understands.
    ///
    /// The list is empty when the pool is disabled: the engine then talks to
    /// the upstream directly. Handing it dead SOCKS endpoints instead makes
    /// every upstream call fail with `proxy_error`, because the engine keeps
    /// routing through the configured proxies no matter how many of them
    /// refuse the connection.
    pub fn proxy_urls(&self) -> Vec<String> {
        if !self.warp_enabled || self.warp_identities == 0 {
            return Vec::new();
        }
        (0..self.warp_identities)
            .map(|i| format!("socks5://127.0.0.1:{}#pool", self.tunnel_port(i)))
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tunnel_ports_are_contiguous() {
        let cfg = Config {
            host: "0.0.0.0".into(),
            port: 8080,
            opencode_api_port: 18088,
            upstream_api: "http://127.0.0.1:18088".into(),
            warp_base_socks_port: 40001,
            warp_identities: 3,
            warp_enabled: true,
            warp_auto_rotate_429: true,
            warp_auto_rotate_403: true,
            warp_state_dir: "/app/warp".into(),
            max_retries: 3,
            opencode2api_bin: "opencode2api".into(),
            opencode2api_config: "/app/config.json".into(),
            server_key: "k".into(),
        };
        assert_eq!(cfg.tunnel_port(0), 40001);
        assert_eq!(cfg.tunnel_port(2), 40003);
        assert_eq!(
            cfg.proxy_urls(),
            vec![
                "socks5://127.0.0.1:40001#pool",
                "socks5://127.0.0.1:40002#pool",
                "socks5://127.0.0.1:40003#pool",
            ]
        );
    }

    #[test]
    fn no_identities_means_no_proxies() {
        let mut cfg = Config::from_env();
        cfg.warp_identities = 0;
        assert!(cfg.proxy_urls().is_empty());
    }

    #[test]
    fn disabled_pool_means_no_proxies() {
        let mut cfg = Config::from_env();
        cfg.warp_enabled = false;
        cfg.warp_identities = 3;
        // The engine must not be pointed at SOCKS endpoints that nothing
        // listens on: with WARP disabled every upstream call would fail with
        // `proxy_error` instead of going out directly.
        assert!(cfg.proxy_urls().is_empty());
    }
}
