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
    /// Internal SOCKS5 port for Cloudflare WARP in proxy mode (default: 40000)
    pub warp_socks_port: u16,
    /// Whether Cloudflare WARP is enabled (default: true)
    pub warp_enabled: bool,
    /// Auto-rotate WARP IP when receiving HTTP 429 Rate Limit (default: true)
    pub warp_auto_rotate_429: bool,
    /// Auto-rotate WARP IP when receiving HTTP 403 Abuse / FreeTierError (default: true)
    pub warp_auto_rotate_403: bool,
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

        Self {
            host,
            port,
            opencode_api_port,
            upstream_api,
            warp_socks_port: env_parse("WARP_SOCKS_PORT", 40000),
            warp_enabled: env_bool("WARP_ENABLED", true),
            warp_auto_rotate_429: env_bool("WARP_AUTO_ROTATE_429", true),
            warp_auto_rotate_403: env_bool("WARP_AUTO_ROTATE_403", true),
            max_retries: env_parse("MAX_RETRIES", 3).max(1),
            opencode2api_bin: env::var("OPENCODE2API_BIN")
                .unwrap_or_else(|_| "/usr/local/bin/opencode2api".to_string()),
            opencode2api_config: env::var("OPENCODE2API_CONFIG")
                .unwrap_or_else(|_| "/app/config.json".to_string()),
            server_key: env::var("SERVER_KEY").unwrap_or_else(|_| "local-test-key".to_string()),
        }
    }
}
