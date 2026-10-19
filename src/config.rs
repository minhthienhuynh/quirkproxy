use std::env;

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

        let opencode_api_port = env::var("OPENCODE2API_PORT")
            .ok()
            .and_then(|p| p.parse().ok())
            .unwrap_or(18088);

        let upstream_api = env::var("UPSTREAM_API")
            .unwrap_or_else(|_| format!("http://127.0.0.1:{}", opencode_api_port));

        let warp_socks_port = env::var("WARP_SOCKS_PORT")
            .ok()
            .and_then(|p| p.parse().ok())
            .unwrap_or(40000);

        Self {
            host,
            port,
            opencode_api_port,
            upstream_api,
            warp_socks_port,
            warp_enabled: env::var("WARP_ENABLED")
                .map(|v| v == "1" || v.eq_ignore_ascii_case("true"))
                .unwrap_or(true),
            warp_auto_rotate_429: env::var("WARP_AUTO_ROTATE_429")
                .map(|v| v == "1" || v.eq_ignore_ascii_case("true"))
                .unwrap_or(true),
            warp_auto_rotate_403: env::var("WARP_AUTO_ROTATE_403")
                .map(|v| v == "1" || v.eq_ignore_ascii_case("true"))
                .unwrap_or(true),
            max_retries: env::var("MAX_RETRIES")
                .ok()
                .and_then(|r| r.parse().ok())
                .unwrap_or(3),
            opencode2api_bin: env::var("OPENCODE2API_BIN")
                .unwrap_or_else(|_| "/usr/local/bin/opencode2api".to_string()),
            opencode2api_config: env::var("OPENCODE2API_CONFIG")
                .unwrap_or_else(|_| "/app/config.json".to_string()),
            server_key: env::var("SERVER_KEY").unwrap_or_else(|_| "local-test-key".to_string()),
        }
    }
}
