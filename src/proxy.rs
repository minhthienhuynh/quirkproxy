use axum::{
    body::Body,
    extract::{Query, Request, State},
    http::{header, HeaderMap, HeaderName, HeaderValue, StatusCode},
    response::{IntoResponse, Response},
    Json,
};
use bytes::Bytes;
use http_body_util::{BodyExt, Limited};
use serde::Deserialize;
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
    pub semaphore: Arc<tokio::sync::Semaphore>,
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

    let engine = match state
        .http_client
        .get(format!("{}/health", state.config.upstream_api))
        .timeout(Duration::from_secs(3))
        .send()
        .await
    {
        Ok(resp) => {
            let status = resp.status();
            let body: serde_json::Value = resp.json().await.unwrap_or_default();
            EngineHealth {
                reachable: true,
                ready: status.is_success(),
                models_loaded: body["models"]["total"].as_u64().unwrap_or(0),
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
            "engine": "zen-proxy (Bun)",
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

/// Compares two secrets without leaking their contents through timing.
///
/// A plain `==` on strings short-circuits on the first differing byte, which
/// is enough to recover a bearer token one byte at a time given enough
/// requests. The length is compared first (it is not secret), then every
/// byte is folded into an accumulator so the comparison takes the same time
/// for every input.
fn constant_time_eq(a: &str, b: &str) -> bool {
    let (a, b) = (a.as_bytes(), b.as_bytes());
    if a.is_empty() || b.is_empty() || a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

fn validate_auth(headers: &HeaderMap, server_key: &str) -> bool {
    let provided = headers
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or_default();

    !server_key.trim().is_empty()
        && !provided.trim().is_empty()
        && constant_time_eq(provided, server_key)
}

#[derive(Deserialize, Default)]
pub struct RotateQuery {
    pub mode: Option<String>,
    pub tunnel: Option<usize>,
    pub force: Option<bool>,
}

pub async fn trigger_rotate(
    State(state): State<AppState>,
    headers: HeaderMap,
    Query(query): Query<RotateQuery>,
) -> Response {
    if !state.warp.enabled() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({
                "status": "disabled",
                "message": "Cloudflare WARP is disabled in configuration"
            })),
        )
            .into_response();
    }

    // Rotation is expensive (drops the current egress identity): never allow
    // unauthenticated callers to trigger it.
    if !validate_auth(&headers, &state.config.server_key) {
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({
                "status": "unauthorized",
                "message": "Missing or invalid bearer token"
            })),
        )
            .into_response();
    }

    if query.mode.as_deref() == Some("next") || query.mode.as_deref() == Some("staggered") {
        return trigger_rotate_next(State(state), headers, Query(query))
            .await
            .into_response();
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
        )
            .into_response(),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(json!({
                "status": "error",
                "message": e.to_string(),
            })),
        )
            .into_response(),
    }
}

pub async fn trigger_rotate_next(
    State(state): State<AppState>,
    headers: HeaderMap,
    Query(query): Query<RotateQuery>,
) -> Response {
    if !state.warp.enabled() {
        return (
            StatusCode::BAD_REQUEST,
            Json(json!({
                "status": "disabled",
                "message": "Cloudflare WARP is disabled in configuration"
            })),
        )
            .into_response();
    }

    if !validate_auth(&headers, &state.config.server_key) {
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({
                "status": "unauthorized",
                "message": "Missing or invalid bearer token"
            })),
        )
            .into_response();
    }

    match state
        .warp
        .rotate_next(
            "Manual API Trigger (/v1/rotate/next)",
            query.tunnel,
            query.force.unwrap_or(false),
        )
        .await
    {
        Ok(outcome) => (
            StatusCode::OK,
            Json(json!({
                "status": outcome.status,
                "message": outcome.message,
                "tunnel": outcome.tunnel,
                "port": outcome.port,
                "old_ip": outcome.old_ip,
                "new_ip": outcome.new_ip,
                "warp": state.warp.status().await,
            })),
        )
            .into_response(),
        Err(e) => (
            StatusCode::INTERNAL_SERVER_ERROR,
            Json(json!({
                "status": "error",
                "message": e.to_string(),
            })),
        )
            .into_response(),
    }
}

/// Generates a fast, cryptographically unique OpenCode CLI 2.x session ID
/// Format: ses_<12-hex-inverted-timestamp><14-base62-random>
///
/// The hex part carries an inverted timestamp so that newer sessions sort
/// before older ones. The low 12 bits of the timestamp field are replaced by a
/// fixed constant, which is why the random counter is folded into the base62
/// suffix instead: entropy that is masked off before formatting would have no
/// effect on the generated id at all.
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
        .map(|i| {
            // The counter perturbs the first character so that two ids
            // generated in the same millisecond still differ even if the
            // random draw repeats.
            let idx = if i == 0 {
                counter as usize % BASE62.len()
            } else {
                fastrand::usize(..BASE62.len())
            };
            BASE62[idx] as char
        })
        .collect();

    format!("ses_{}{}", hex_ts, rand_b62)
}

pub async fn proxy_handler(State(state): State<AppState>, req: Request) -> Response {
    let permit = match state.semaphore.clone().try_acquire_owned() {
        Ok(p) => p,
        Err(_) => {
            warn!(
                "Concurrency limit reached ({}), rejecting incoming request",
                state.config.max_concurrency
            );
            return (
                StatusCode::TOO_MANY_REQUESTS,
                [(header::RETRY_AFTER, "1")],
                Json(json!({
                    "error": {
                        "message": "Too many concurrent requests; server concurrency limit reached",
                        "type": "concurrency_limit_exceeded"
                    }
                })),
            )
                .into_response();
        }
    };

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

    // Buffer the request body to allow replay on 429/403 auto-healing retry.
    //
    // The size limit has to be enforced *while* reading, not after: the body
    // is fully materialised in memory before it can be measured, so a
    // multi-gigabyte upload would OOM the container long before the check
    // below could reject it. `Limited` stops the read at the cap and
    // `content-length` rejects an oversized body without reading it at all.
    if let Some(len) = req
        .headers()
        .get(header::CONTENT_LENGTH)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse::<usize>().ok())
    {
        if len > MAX_BODY_BYTES {
            warn!(
                "Rejecting request body of {} bytes (limit {})",
                len, MAX_BODY_BYTES
            );
            return (
                StatusCode::PAYLOAD_TOO_LARGE,
                Json(json!({ "error": { "message": "Request body too large" } })),
            )
                .into_response();
        }
    }

    let body_bytes: Bytes = match Limited::new(req.into_body(), MAX_BODY_BYTES)
        .collect()
        .await
    {
        Ok(collected) => collected.to_bytes(),
        Err(e) => {
            // A body that grew past the cap while streaming is the same
            // condition as one that declared an oversized length.
            if e.downcast_ref::<http_body_util::LengthLimitError>()
                .is_some()
            {
                warn!(
                    "Rejecting request body larger than the limit ({} bytes)",
                    MAX_BODY_BYTES
                );
                return (
                    StatusCode::PAYLOAD_TOO_LARGE,
                    Json(json!({ "error": { "message": "Request body too large" } })),
                )
                    .into_response();
            }
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

    // The in-flight guard is taken before the first upstream attempt, not
    // after its response headers arrive. Counting only from the response
    // onwards hides requests that are still waiting on the upstream, and a
    // manual rotation would then tear down a tunnel under a request that was
    // still in flight.
    let mut inflight = Some(state.warp.begin_request());

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
        let stream = res.bytes_stream();
        let body = match inflight.take() {
            Some(guard) => Body::from_stream(StreamGuard::new(guard, permit, stream)),
            // Unreachable in practice: the guard is taken exactly once per
            // request, on the path that returns a response body.
            None => {
                drop(permit);
                Body::from_stream(stream)
            }
        };

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
    _permit: tokio::sync::OwnedSemaphorePermit,
    inner: S,
}

impl<S> StreamGuard<S> {
    fn new(
        guard: crate::warp::InflightGuard,
        permit: tokio::sync::OwnedSemaphorePermit,
        inner: S,
    ) -> Self {
        Self {
            _guard: guard,
            _permit: permit,
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

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use http_body_util::BodyExt;

    #[test]
    fn constant_time_eq_matches_equal_and_unequal_secrets() {
        assert!(constant_time_eq("local-test-key", "local-test-key"));
        assert!(!constant_time_eq("local-test-key", "local-test-kex"));
        assert!(!constant_time_eq("local-test-key", "local-test-ke"));
        assert!(!constant_time_eq("local-test-key", ""));
        assert!(!constant_time_eq("", "local-test-key"));
        assert!(!constant_time_eq("", ""));
    }

    #[tokio::test]
    async fn body_under_the_limit_is_read_completely() {
        let payload = Bytes::from_static(b"hello");
        let body = Limited::new(Body::from(payload.clone()), MAX_BODY_BYTES);
        let collected = body.collect().await.expect("body should be readable");
        assert_eq!(collected.to_bytes(), payload);
    }

    #[tokio::test]
    async fn body_over_the_limit_is_rejected_while_reading() {
        let payload = Bytes::from(vec![b'x'; MAX_BODY_BYTES + 1]);
        let body = Limited::new(Body::from(payload), MAX_BODY_BYTES);
        let err = body
            .collect()
            .await
            .expect_err("oversized body must be rejected");
        assert!(
            err.downcast_ref::<http_body_util::LengthLimitError>()
                .is_some(),
            "expected a length limit error, got: {err}"
        );
    }

    #[tokio::test]
    async fn concurrency_limit_rejects_with_429_when_saturated() {
        let cfg = Config {
            host: "0.0.0.0".into(),
            port: 8080,
            opencode_api_port: 18088,
            upstream_api: "http://127.0.0.1:18088".into(),
            warp_base_socks_port: 40001,
            warp_identities: 1,
            warp_enabled: false,
            warp_protocol: crate::config::WarpProtocol::WireGuard,
            warp_auto_rotate_429: false,
            warp_auto_rotate_403: false,
            warp_state_dir: "/tmp".into(),
            max_retries: 1,
            opencode2api_bin: "false".into(),
            opencode2api_script: "/tmp/zen.mjs".into(),
            opencode2api_config: "/tmp/cfg.json".into(),
            server_key: "test-key".into(),
            max_concurrency: 1,
            cors_allowed_origins: None,
        };
        let pool = Arc::new(WarpPool::new(
            [40001],
            "/tmp",
            false,
            crate::config::WarpProtocol::WireGuard,
        ));
        let semaphore = Arc::new(tokio::sync::Semaphore::new(1));
        // Exhaust the 1 available permit
        let _permit = semaphore.clone().try_acquire_owned().unwrap();

        let state = AppState {
            config: cfg,
            warp: pool,
            http_client: reqwest::Client::new(),
            semaphore,
        };

        let req = Request::builder()
            .method("POST")
            .uri("/v1/chat/completions")
            .body(Body::empty())
            .unwrap();

        let resp = proxy_handler(State(state), req).await;
        assert_eq!(resp.status(), StatusCode::TOO_MANY_REQUESTS);
    }
}
