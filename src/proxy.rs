use axum::{
    body::Body,
    extract::{Request, State},
    http::{header, HeaderMap, HeaderName, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use bytes::Bytes;
use http_body_util::BodyExt;
use serde_json::json;
use std::sync::Arc;
use std::time::Duration;
use tracing::{error, info, warn};

use crate::config::Config;
use crate::gate::{classify_403, RefusalKind};
use crate::warp::WarpPool;

/// Max buffered request body (32 MiB). The body must be buffered to allow
/// replaying it after a WARP IP rotation.
const MAX_BODY_BYTES: usize = 32 * 1024 * 1024;

#[derive(Clone)]
pub struct AppState {
    pub config: Config,
    pub warp: Arc<WarpPool>,
    pub http_client: reqwest::Client,
}

#[inline]
fn is_hop_by_hop(name: &header::HeaderName) -> bool {
    matches!(
        name.as_str(),
        "connection"
            | "keep-alive"
            | "proxy-authenticate"
            | "proxy-authorization"
            | "te"
            | "trailers"
            | "transfer-encoding"
            | "upgrade"
            | "host"
            | "content-length"
    )
}

pub async fn healthz(State(state): State<AppState>) -> impl IntoResponse {
    let warp_status = state.warp.status().await;

    // The engine reports 503 while its model catalog is refreshing (cold start
    // or a transient upstream blip) but still serves requests. Distinguish
    // "can serve" from "fully warmed" so the container is not marked degraded
    // for a condition that resolves itself.
    let engine = match state
        .http_client
        .get(format!("{}/healthz", state.config.upstream_api))
        .timeout(Duration::from_secs(3))
        .send()
        .await
    {
        Ok(resp) => {
            let status = resp.status();
            let body: serde_json::Value = resp.json().await.unwrap_or_default();
            EngineHealth {
                reachable: true,
                // A 503 with models still loading is "warming", not "down".
                ready: status.is_success(),
                models_loaded: body["models"]["exposed"].as_u64().unwrap_or(0),
            }
        }
        Err(_) => EngineHealth {
            reachable: false,
            ready: false,
            models_loaded: 0,
        },
    };

    let overall = match (engine.reachable, engine.ready) {
        (true, true) => "healthy",
        (true, false) => "warming",
        (false, _) => "degraded",
    };

    // Always 200 for container liveness; health detail is in the JSON body.
    (
        StatusCode::OK,
        Json(json!({
            "status": overall,
            "service": "QuirkProxy",
            "version": env!("CARGO_PKG_VERSION"),
            "engine": "opencode2api",
            "engine_online": engine.reachable,
            "engine_ready": engine.ready,
            "engine_models": engine.models_loaded,
            "inflight": state.warp.inflight_count(),
            "warp": warp_status,
        })),
    )
}

struct EngineHealth {
    reachable: bool,
    ready: bool,
    models_loaded: u64,
}

pub async fn trigger_rotate(
    State(state): State<AppState>,
    headers: HeaderMap,
) -> impl IntoResponse {
    if !state.warp.enabled() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({
                "status": "disabled",
                "message": "Cloudflare WARP is disabled in configuration"
            })),
        );
    }

    // Rotation is expensive (drops the current egress identity): never allow
    // unauthenticated callers to trigger it.
    let provided = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or_default();

    if provided != state.config.server_key {
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({
                "status": "unauthorized",
                "message": "Missing or invalid bearer token"
            })),
        );
    }

    match state
        .warp
        .rotate_all("Manual API Trigger (/v1/rotate)")
        .await
    {
        Ok(new_ip) => (
            StatusCode::OK,
            Json(json!({
                "status": "success",
                "new_ip": new_ip,
                "warp": state.warp.status().await,
            })),
        ),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(json!({
                "status": "error",
                "message": e.to_string(),
            })),
        ),
    }
}

/// Generates a fast, cryptographically unique OpenCode CLI 2.x session ID
/// Format: ses_<12-hex-inverted-timestamp><14-base62-random>
fn make_session_id() -> String {
    use std::time::SystemTime;
    let now_ms = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64;

    let counter = fastrand::u16(..0x1000);
    let raw = (now_ms << 12) | (counter as u64);
    let inv = (!raw) & 0x0000FFFFFFFFFFFF;
    let hex_ts = format!("{:012x}", (inv & 0xFFFFFFFFF000) | 0xFFE);

    const BASE62: &[u8] = b"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";
    let rand_b62: String = (0..14)
        .map(|_| {
            let idx = fastrand::usize(..BASE62.len());
            BASE62[idx] as char
        })
        .collect();

    format!("ses_{}{}", hex_ts, rand_b62)
}

pub async fn proxy_handler(State(state): State<AppState>, req: Request) -> Response {
    let method = req.method().clone();
    let uri = req.uri().clone();
    let path_and_query = uri
        .path_and_query()
        .map(|pq| pq.as_str())
        .unwrap_or(uri.path());

    let target_url = format!("{}{}", state.config.upstream_api, path_and_query);

    // Filter incoming headers
    let mut forwarded_headers = HeaderMap::new();
    for (k, v) in req.headers() {
        if !is_hop_by_hop(k) {
            forwarded_headers.insert(k.clone(), v.clone());
        }
    }

    // Buffer request body to allow replay on 429/403 auto-healing retry
    let body_bytes: Bytes = match req.into_body().collect().await {
        Ok(collected) => {
            let bytes = collected.to_bytes();
            // Guard against OOM: prompts+images can be large, but unbounded buffering
            // of a multi-GB body would kill the container.
            if bytes.len() > MAX_BODY_BYTES {
                warn!(
                    "Rejecting request body of {} bytes (limit {})",
                    bytes.len(),
                    MAX_BODY_BYTES
                );
                return (
                    StatusCode::PAYLOAD_TOO_LARGE,
                    Json(json!({ "error": { "message": "Request body too large" } })),
                )
                    .into_response();
            }
            bytes
        }
        Err(e) => {
            error!("Failed to read incoming request body: {}", e);
            return StatusCode::BAD_REQUEST.into_response();
        }
    };

    let max_retries = if state.warp.enabled()
        && (state.config.warp_auto_rotate_429 || state.config.warp_auto_rotate_403)
    {
        state.config.max_retries.max(1)
    } else {
        1
    };

    let mut attempt = 0;
    while attempt < max_retries {
        attempt += 1;

        // Retries get a fresh session id: reusing the failed session would
        // poison the attempt even on a new egress address.
        let mut headers = forwarded_headers.clone();
        if attempt > 1 || !headers.contains_key("x-session-id") {
            if let Ok(val) = HeaderValue::from_str(&make_session_id()) {
                headers.insert(HeaderName::from_static("x-session-id"), val);
            }
        }

        let req_builder = state
            .http_client
            .request(method.clone(), &target_url)
            .headers(headers)
            .body(body_bytes.clone());

        let res = match req_builder.send().await {
            Ok(resp) => resp,
            Err(e) => {
                error!("Error sending request to opencode2api: {}", e);
                if attempt >= max_retries {
                    return (
                        StatusCode::BAD_GATEWAY,
                        Json(json!({ "error": { "message": format!("Gateway connection failed: {}", e) } })),
                    )
                        .into_response();
                }
                tokio::time::sleep(Duration::from_millis(300)).await;
                continue;
            }
        };

        let status = res.status();

        // Check for 429 Rate Limit or 403 Abuse / FreeTierError
        if status == reqwest::StatusCode::TOO_MANY_REQUESTS
            || status == reqwest::StatusCode::FORBIDDEN
        {
            let is_429 = status == reqwest::StatusCode::TOO_MANY_REQUESTS;
            let is_403 = status == reqwest::StatusCode::FORBIDDEN;

            let res_headers = res.headers().clone();
            let err_bytes = res.bytes().await.unwrap_or_default();
            let err_text = String::from_utf8_lossy(&err_bytes);

            // A 403 is not one condition. Only some causes are fixed by a new
            // egress address; rotating for a shape/geo/auth refusal spends a
            // registration and ~5s while guaranteeing the same failure.
            let refusal = if is_403 {
                Some(classify_403(&err_text))
            } else {
                None
            };

            let should_rotate = match (is_429, refusal) {
                (true, _) => state.config.warp_auto_rotate_429,
                (false, Some(kind)) => state.config.warp_auto_rotate_403 && kind.rotate_eligible(),
                (false, None) => false,
            };

            if is_403 {
                if let Some(kind) = refusal {
                    if !kind.rotate_eligible() {
                        warn!(
                            "403 classified as {} -- not rotating: {}",
                            kind.as_str(),
                            kind.advice()
                        );
                    }
                }
            }

            if should_rotate && state.warp.enabled() {
                let err_kind = if is_429 {
                    RefusalKind::Quota
                } else {
                    refusal.unwrap_or(RefusalKind::Unknown)
                };
                let status_before = state.warp.status().await;

                warn!(
                    "Upstream returned {} ({}) - refreshing the WARP pool (attempt {}/{})...",
                    status,
                    err_kind.as_str(),
                    attempt,
                    max_retries
                );

                // Never block the request path on a registration. The recycle
                // refreshes one identity in the background; the retry below
                // lands on whichever tunnel the engine has not burned yet.
                state.warp.recycle_async(&format!(
                    "Upstream {} ({}) on attempt {}",
                    status,
                    err_kind.as_str(),
                    attempt
                ));

                if attempt < max_retries {
                    info!(
                        "Retrying on a different tunnel (healthy before retry: {}/{})...",
                        status_before.tunnels.iter().filter(|t| t.healthy).count(),
                        status_before.tunnels.len()
                    );
                    tokio::time::sleep(Duration::from_millis(300)).await;
                    continue;
                }
            }

            // Return upstream error response if cannot rotate or exhausted retries
            return build_response(status, &res_headers, Body::from(err_bytes));
        }

        // Success or normal response -> stream back.
        // Copy end-to-end headers before `res` is consumed by bytes_stream().
        let res_headers = res.headers().clone();

        // The guard is moved into the body stream so the pool counts as busy
        // until the last byte reaches the client, not just until headers arrive.
        let guard = state.warp.begin_request();
        let stream = res.bytes_stream();
        let body = Body::from_stream(StreamGuard::new(guard, stream));

        return build_response(status, &res_headers, body);
    }

    (
        StatusCode::BAD_GATEWAY,
        Json(json!({ "error": { "message": "All upstream attempts exhausted" } })),
    )
        .into_response()
}

/// Builds a response that mirrors upstream status and end-to-end headers.
fn build_response(status: StatusCode, headers: &HeaderMap, body: Body) -> Response {
    let mut builder = Response::builder().status(status);
    for (k, v) in headers {
        if !is_hop_by_hop(k) {
            builder = builder.header(k, v);
        }
    }
    builder
        .body(body)
        .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response())
}

/// Wraps a response byte stream so an [`InflightGuard`] is held for exactly as
/// long as the stream is alive. Dropping the body (client disconnect, timeout,
/// panic) releases the guard too, so the counter cannot leak.
struct StreamGuard<S> {
    _guard: crate::warp::InflightGuard,
    inner: S,
}

impl<S> StreamGuard<S> {
    fn new(guard: crate::warp::InflightGuard, inner: S) -> Self {
        Self {
            _guard: guard,
            inner,
        }
    }
}

impl<S> futures_core::Stream for StreamGuard<S>
where
    S: futures_core::Stream + Unpin,
{
    type Item = S::Item;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        std::pin::Pin::new(&mut self.inner).poll_next(cx)
    }
}
