mod config;
mod gate;
mod proxy;
mod warp;

use anyhow::{Context, Result};
use axum::{
    routing::{get, post},
    Router,
};
use std::fs;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;
use tokio::process::Command;
use tokio::signal;
use tower_http::cors::CorsLayer;
use tower_http::trace::TraceLayer;
use tracing::{error, info, warn};

use config::Config;
use proxy::AppState;
use warp::{wait_for_shutdown, WarpPool};

/// How long to wait for the engine to answer /healthz after spawning it.
/// The engine returns 503 until its model catalog refresh succeeds, which
/// takes ~10-15s on a cold WARP tunnel; 60s leaves room for one retry.
const ENGINE_READY_TIMEOUT: Duration = Duration::from_secs(60);
/// Poll interval while waiting for the engine.
const ENGINE_READY_POLL: Duration = Duration::from_millis(500);
/// A child that survived this long is considered healthy enough to restart
/// immediately instead of backing off (crash loop vs. one-off crash).
const ENGINE_STABLE_UPTIME: Duration = Duration::from_secs(30);
/// Grace period for the engine to exit after SIGKILL on shutdown.
const SUPERVISOR_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(10);
/// Upper bound for the engine restart delay.
const ENGINE_MAX_BACKOFF: Duration = Duration::from_secs(30);

/// What the supervisor must do once the engine child exits.
#[derive(Debug, PartialEq, Eq)]
enum EngineExitAction {
    /// The run was long enough to count as stable: respawn immediately.
    RespawnNow,
    /// The run was a crash: sleep for this delay, then respawn.
    SleepThenRespawn(Duration),
}

/// Restart delay for the engine supervisor.
///
/// The delay starts at one second and doubles on every failed spawn or
/// short-lived child, capped at [`ENGINE_MAX_BACKOFF`]. Only a run that stayed
/// up for at least [`ENGINE_STABLE_UPTIME`] resets it, so an engine that dies
/// during startup backs off instead of being respawned in a tight loop.
#[derive(Debug)]
struct RestartBackoff {
    next: Duration,
}

impl RestartBackoff {
    fn new() -> Self {
        Self {
            next: Duration::from_secs(1),
        }
    }

    /// Delay to use before the next spawn attempt.
    #[cfg(test)]
    fn delay(&self) -> Duration {
        self.next
    }

    /// Records a child exit and reports what the supervisor should do next.
    fn on_child_exit(&mut self, uptime: Duration) -> EngineExitAction {
        if uptime >= ENGINE_STABLE_UPTIME {
            self.next = Duration::from_secs(1);
            EngineExitAction::RespawnNow
        } else {
            let delay = self.next;
            self.next = (self.next * 2).min(ENGINE_MAX_BACKOFF);
            EngineExitAction::SleepThenRespawn(delay)
        }
    }

    /// Records a failed spawn and returns the delay to sleep before retrying.
    fn on_spawn_failure(&mut self) -> Duration {
        let delay = self.next;
        self.next = (self.next * 2).min(ENGINE_MAX_BACKOFF);
        delay
    }
}

fn ensure_opencode2api_config(config: &Config) -> Result<()> {
    // The config is fully derived from env, so it must be regenerated on each boot.
    // Keeping a stale file (e.g. old listen port) silently breaks the engine.
    if let Some(parent) = Path::new(&config.opencode2api_config).parent() {
        fs::create_dir_all(parent)?;
    }

    let config_json = serde_json::json!({
        "listen": format!("127.0.0.1:{}", config.opencode_api_port),
        "server_keys": [&config.server_key],
        "zen_keys": [],
        "go_keys": [],
        "anonymous": true,
        // One entry per WARP identity. The engine's anonymous pool gives every
        // proxy its own node, so a 429 on one exit retries on the next instead
        // of re-registering and waiting.
        "proxies": config.proxy_urls(),
        "proxyfile": "",
        "upstream": {
            "zen": "https://opencode.ai/zen",
            "go": "https://opencode.ai/zen/go",
            "anonymous": "https://opencode.ai/inference"
        },
        "retry": {
            "max_attempts": 3,
            "timeout_seconds": 180
        },
        "models": {
            "refresh_seconds": 300,
            "free_only": true
        },
        "performance": {
            "max_idle_conns": 256,
            "max_idle_conns_per_host": 64,
            "idle_conn_timeout_seconds": 60,
            "connect_timeout_seconds": 10,
            "failure_cooldown_seconds": 15
        },
        "logging": {
            "level": "info",
            "ring_size": 1000
        },
        "webui": {
            "enabled": false
        }
    });

    fs::write(
        &config.opencode2api_config,
        serde_json::to_string_pretty(&config_json)?,
    )
    .context("failed to write opencode2api config.json")?;

    info!(
        "Generated opencode2api config at {}",
        config.opencode2api_config
    );
    Ok(())
}

/// Runs `opencode2api` as a managed child process, restarting it with
/// exponential backoff when it exits unexpectedly.
async fn supervise_engine(config: Config, shutdown: Arc<AtomicBool>) {
    let mut backoff = RestartBackoff::new();

    while !shutdown.load(Ordering::Relaxed) {
        info!(
            "Spawning opencode2api core engine (binary: {})...",
            config.opencode2api_bin
        );
        let child = Command::new(&config.opencode2api_bin)
            .args(["-config", &config.opencode2api_config])
            .kill_on_drop(true)
            .spawn();

        let mut child = match child {
            Ok(c) => c,
            Err(e) => {
                let delay = backoff.on_spawn_failure();
                error!(
                    "Failed to spawn opencode2api: {}. Retrying in {:?}",
                    e, delay
                );
                tokio::time::sleep(delay).await;
                continue;
            }
        };

        let started = std::time::Instant::now();

        tokio::select! {
            status = child.wait() => {
                let uptime = started.elapsed();
                warn!("opencode2api exited ({:?}) after {:?}", status, uptime);
                // Only a run that stayed up long enough resets the backoff.
                // A child that dies quickly keeps growing the delay.
                match backoff.on_child_exit(uptime) {
                    EngineExitAction::RespawnNow => continue,
                    EngineExitAction::SleepThenRespawn(delay) => {
                        tokio::time::sleep(delay).await;
                        continue;
                    }
                }
            }
            _ = wait_for_shutdown(&shutdown) => {
                info!("Supervisor shutting down opencode2api child...");
                let _ = child.kill().await;
                return;
            }
        }
    }
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info,quirkproxy=debug".into()),
        )
        .init();

    info!("=== Starting QuirkProxy (Rust Supervisor + OpenCode2API Core Engine) ===");

    let config = Config::from_env();
    let tunnel_ports: Vec<u16> = (0..config.warp_identities)
        .map(|i| config.tunnel_port(i))
        .collect();

    let warp = Arc::new(WarpPool::new(
        tunnel_ports,
        &config.warp_state_dir,
        config.warp_enabled,
        config.warp_protocol,
    ));

    let shutdown = Arc::new(AtomicBool::new(false));

    // 1. Start the Cloudflare WARP tunnel pool (userspace, no NET_ADMIN)
    let warp_tasks = warp.start(shutdown.clone()).await;

    // 2. Prepare opencode2api configuration
    ensure_opencode2api_config(&config)?;

    // 3. Spawn opencode2api core engine under supervision (restart on crash)
    let engine_task = {
        let cfg = config.clone();
        let shutdown = shutdown.clone();
        tokio::spawn(async move { supervise_engine(cfg, shutdown).await })
    };

    // 4. Wait for opencode2api to become ready on internal loopback
    // NOTE: no total `timeout()` here — it is a hard deadline that includes the
    // response body, so it would cut long SSE streams mid-flight. `read_timeout`
    // resets on every successful read and only kills genuinely stalled streams.
    let http_client = reqwest::Client::builder()
        .tcp_nodelay(true)
        .pool_idle_timeout(Some(Duration::from_secs(90)))
        .pool_max_idle_per_host(64)
        .connect_timeout(Duration::from_secs(10))
        .read_timeout(Duration::from_secs(180))
        .build()?;

    let health_url = format!("{}/healthz", config.upstream_api);
    info!(
        "Waiting for opencode2api engine to report healthy on {}...",
        health_url
    );

    let mut ready = false;
    let deadline = tokio::time::Instant::now() + ENGINE_READY_TIMEOUT;
    while tokio::time::Instant::now() < deadline {
        tokio::time::sleep(ENGINE_READY_POLL).await;
        if let Ok(resp) = http_client
            .get(&health_url)
            .timeout(Duration::from_secs(2))
            .send()
            .await
        {
            if resp.status().is_success() {
                ready = true;
                break;
            }
        }
    }

    if ready {
        info!("Core engine opencode2api is healthy and ready!");
    } else {
        warn!("Core engine startup timeout reached; proceeding to listen...");
    }

    // 5. Build Axum reverse proxy router
    let semaphore = Arc::new(tokio::sync::Semaphore::new(config.max_concurrency));

    let state = AppState {
        config: config.clone(),
        warp: warp.clone(),
        http_client: http_client.clone(),
        semaphore,
    };

    let cors = match config.cors_allowed_origins.as_deref() {
        Some("*") => {
            warn!("CORS is configured to allow all origins (*)");
            CorsLayer::permissive()
        }
        Some(origins) if !origins.trim().is_empty() => {
            let parsed: Vec<_> = origins
                .split(',')
                .filter_map(|s| s.trim().parse().ok())
                .collect();
            CorsLayer::new()
                .allow_origin(parsed)
                .allow_methods(tower_http::cors::Any)
                .allow_headers(tower_http::cors::Any)
        }
        _ => CorsLayer::new()
            .allow_origin(tower_http::cors::AllowOrigin::predicate(|origin, _| {
                let s = origin.to_str().unwrap_or_default();
                s.starts_with("http://localhost")
                    || s.starts_with("http://127.0.0.1")
                    || s.starts_with("https://localhost")
                    || s.starts_with("https://127.0.0.1")
            }))
            .allow_methods(tower_http::cors::Any)
            .allow_headers(tower_http::cors::Any),
    };

    let app = Router::new()
        .route("/healthz", get(proxy::healthz))
        .route("/v1/rotate", post(proxy::trigger_rotate))
        .route("/v1/rotate/next", post(proxy::trigger_rotate_next))
        .fallback(proxy::proxy_handler)
        .layer(cors)
        .layer(TraceLayer::new_for_http())
        .with_state(state);

    let bind_addr: SocketAddr = format!("{}:{}", config.host, config.port)
        .parse()
        .context("invalid bind address")?;

    info!("QuirkProxy listening on http://{}", bind_addr);

    let listener = tokio::net::TcpListener::bind(&bind_addr).await?;

    // 6. Run server with graceful shutdown handling
    // TCP_NODELAY on accepted connections is critical: without it, small
    // request/response writes are delayed by Nagle + delayed ACK (~40ms).
    axum::serve(listener, app)
        .tcp_nodelay(true)
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    info!("Shutting down QuirkProxy...");

    // Signal all supervisors to stop and reap child processes
    shutdown.store(true, Ordering::Relaxed);

    let shutdown_all = async {
        let _ = engine_task.await;
        for task in warp_tasks {
            let _ = task.await;
        }
    };

    if tokio::time::timeout(SUPERVISOR_SHUTDOWN_TIMEOUT, shutdown_all)
        .await
        .is_err()
    {
        warn!(
            "Shutdown timed out after {:?}; some child processes may be forcibly terminated by the OS.",
            SUPERVISOR_SHUTDOWN_TIMEOUT
        );
    } else {
        info!("All supervisor child processes stopped cleanly.");
    }

    Ok(())
}

async fn shutdown_signal() {
    let ctrl_c = async {
        signal::ctrl_c()
            .await
            .expect("failed to install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        signal::unix::signal(signal::unix::SignalKind::terminate())
            .expect("failed to install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => info!("Received SIGINT signal"),
        _ = terminate => info!("Received SIGTERM signal"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Uptime that the supervisor considers a one-off crash, not a crash loop.
    fn crashing() -> Duration {
        Duration::from_millis(50)
    }

    /// Uptime that the supervisor considers a healthy run.
    fn stable() -> Duration {
        ENGINE_STABLE_UPTIME
    }

    #[test]
    fn backoff_starts_at_one_second() {
        assert_eq!(RestartBackoff::new().delay(), Duration::from_secs(1));
    }

    #[test]
    fn short_lived_child_grows_the_delay_geometrically() {
        let mut backoff = RestartBackoff::new();
        let delays: Vec<Duration> = (0..5)
            .map(|_| match backoff.on_child_exit(crashing()) {
                EngineExitAction::SleepThenRespawn(delay) => delay,
                EngineExitAction::RespawnNow => {
                    panic!("a crash must not be respawned immediately")
                }
            })
            .collect();

        assert_eq!(
            delays,
            vec![
                Duration::from_secs(1),
                Duration::from_secs(2),
                Duration::from_secs(4),
                Duration::from_secs(8),
                Duration::from_secs(16),
            ]
        );
    }

    #[test]
    fn crash_loop_delay_is_capped() {
        let mut backoff = RestartBackoff::new();
        // 1, 2, 4, 8, 16, 30, 30, ... — the cap must hold no matter how long
        // the engine keeps dying during startup.
        for _ in 0..20 {
            backoff.on_child_exit(crashing());
        }
        assert_eq!(backoff.delay(), ENGINE_MAX_BACKOFF);

        match backoff.on_child_exit(crashing()) {
            EngineExitAction::SleepThenRespawn(delay) => assert_eq!(delay, ENGINE_MAX_BACKOFF),
            EngineExitAction::RespawnNow => panic!("a crash must not be respawned immediately"),
        }
    }

    #[test]
    fn stable_run_resets_the_delay_and_respawns_immediately() {
        let mut backoff = RestartBackoff::new();
        // Walk the delay up so a reset is observable.
        backoff.on_child_exit(crashing());
        backoff.on_child_exit(crashing());
        backoff.on_child_exit(crashing());
        assert_eq!(backoff.delay(), Duration::from_secs(8));

        assert_eq!(
            backoff.on_child_exit(stable()),
            EngineExitAction::RespawnNow
        );
        assert_eq!(backoff.delay(), Duration::from_secs(1));
    }

    #[test]
    fn stable_run_below_threshold_still_backs_off() {
        let mut backoff = RestartBackoff::new();
        let just_under = ENGINE_STABLE_UPTIME - Duration::from_millis(1);

        match backoff.on_child_exit(just_under) {
            EngineExitAction::SleepThenRespawn(delay) => assert_eq!(delay, Duration::from_secs(1)),
            EngineExitAction::RespawnNow => {
                panic!("an engine that dies just before the threshold must back off")
            }
        }
        assert_eq!(backoff.delay(), Duration::from_secs(2));
    }

    #[test]
    fn failed_spawn_advances_the_same_delay_series() {
        let mut backoff = RestartBackoff::new();

        assert_eq!(backoff.on_spawn_failure(), Duration::from_secs(1));
        assert_eq!(backoff.on_spawn_failure(), Duration::from_secs(2));
        assert_eq!(backoff.on_spawn_failure(), Duration::from_secs(4));
    }

    #[test]
    fn spawn_failures_and_early_exits_share_one_delay_series() {
        let mut backoff = RestartBackoff::new();

        assert_eq!(backoff.on_spawn_failure(), Duration::from_secs(1));
        match backoff.on_child_exit(crashing()) {
            EngineExitAction::SleepThenRespawn(delay) => assert_eq!(delay, Duration::from_secs(2)),
            EngineExitAction::RespawnNow => panic!("a crash must not be respawned immediately"),
        }
        assert_eq!(backoff.on_spawn_failure(), Duration::from_secs(4));
    }

    #[test]
    fn stable_run_after_spawn_failures_resets_the_delay() {
        let mut backoff = RestartBackoff::new();
        backoff.on_spawn_failure();
        backoff.on_spawn_failure();

        assert_eq!(
            backoff.on_child_exit(stable()),
            EngineExitAction::RespawnNow
        );
        assert_eq!(backoff.on_spawn_failure(), Duration::from_secs(1));
    }
}
