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
use warp::WarpPool;

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
    let mut backoff = Duration::from_secs(1);

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
                error!(
                    "Failed to spawn opencode2api: {}. Retrying in {:?}",
                    e, backoff
                );
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
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
                if uptime >= ENGINE_STABLE_UPTIME {
                    backoff = Duration::from_secs(1);
                    continue;
                }
            }
            _ = wait_for_shutdown(&shutdown) => {
                info!("Supervisor shutting down opencode2api child...");
                let _ = child.kill().await;
                return;
            }
        }

        tokio::time::sleep(backoff).await;
        backoff = (backoff * 2).min(Duration::from_secs(30));
    }
}

async fn wait_for_shutdown(shutdown: &Arc<AtomicBool>) {
    while !shutdown.load(Ordering::Relaxed) {
        tokio::time::sleep(Duration::from_millis(200)).await;
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
    let state = AppState {
        config: config.clone(),
        warp: warp.clone(),
        http_client: http_client.clone(),
    };

    let app = Router::new()
        .route("/healthz", get(proxy::healthz))
        .route("/v1/rotate", post(proxy::trigger_rotate))
        .fallback(proxy::proxy_handler)
        .layer(CorsLayer::permissive())
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
