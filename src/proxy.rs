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
use crate::warp::WarpManager;

/// Max buffered request body (32 MiB). The body must be buffered to allow
/// replaying it after a WARP IP rotation.
const MAX_BODY_BYTES: usize = 32 * 1024 * 1024;

#[derive(Clone)]
pub struct AppState {
    pub config: Config,
    pub warp: Arc<WarpManager>,
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
    let opencode_ok = state
        .http_client
        .get(format!("{}/healthz", state.config.upstream_api))
        .timeout(Duration::from_secs(3))
        .send()
        .await
        .map(|r| r.status().is_success())
        .unwrap_or(false);

    // Always 200 for container liveness; health detail is in the JSON body.
    (
        StatusCode::OK,
        Json(json!({
            "status": if opencode_ok { "healthy" } else { "degraded" },
            "service": "QuirkProxy",
            "version": "1.0.0",
            "engine": "opencode2api",
            "engine_online": opencode_ok,
            "warp": warp_status,
        })),
    )
}

pub async fn trigger_rotate(State(state): State<AppState>, headers: HeaderMap) -> impl IntoResponse {
    if !state.config.warp_enabled {
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

    match state.warp.rotate("Manual API Trigger (/v1/rotate)").await {
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
                warn!("Rejecting request body of {} bytes (limit {})", bytes.len(), MAX_BODY_BYTES);
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

    let max_retries = if state.config.warp_enabled
        && (state.config.warp_auto_rotate_429 || state.config.warp_auto_rotate_403)
    {
        state.config.max_retries.max(1)
    } else {
        1
    };

    let mut attempt = 0;
    while attempt < max_retries {
        attempt += 1;

        let mut headers = forwarded_headers.clone();
        // Inject fresh session ID on retries or when not supplied by client
        if attempt > 1 || !headers.contains_key("x-session-id") {
            let ses = make_session_id();
            if let Ok(val) = HeaderValue::from_str(&ses) {
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

            let is_403_rotate_eligible = is_403
                && {
                    let lower = err_text.to_lowercase();
                    err_text.contains("FreeTierError")
                        || err_text.contains("Abuse")
                        || err_text.contains("OpenCode Free Model Gateway")
                        || lower.contains("forbidden")
                        || lower.contains("rate limit")
                };

            let should_rotate = (is_429 && state.config.warp_auto_rotate_429)
                || (is_403 && state.config.warp_auto_rotate_403 && is_403_rotate_eligible);

            if should_rotate && state.config.warp_enabled && attempt < max_retries {
                let err_kind = if is_429 { "RateLimit 429" } else { "Abuse 403" };
                let err_old_ip = state.warp.status().await.current_ip;
                warn!(
                    "Upstream returned {} ({}) - triggering WARP IP rotation (attempt {}/{})...",
                    status, err_kind, attempt, max_retries
                );

                if let Ok(Some(new_ip)) = state
                    .warp
                    .rotate(&format!("Upstream {} ({}) on attempt {}", status, err_kind, attempt))
                    .await
                {
                    // rotate() returns the previous IP when throttled: retrying on the
                    // same egress identity would just burn an attempt.
                    if Some(&new_ip) == err_old_ip.as_ref() {
                        warn!("WARP rotation throttled; not retrying on the same IP.");
                    } else {
                        info!("WARP rotated to new IP: {} - Retrying request...", new_ip);
                        tokio::time::sleep(Duration::from_millis(300)).await;
                        continue;
                    }
                }
            }

            // Return upstream error response if cannot rotate or exhausted retries
            let mut response = Response::builder().status(status);
            for (k, v) in &res_headers {
                if !is_hop_by_hop(k) {
                    response = response.header(k, v);
                }
            }
            return response
                .body(Body::from(err_bytes))
                .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response());
        }

        // Success or normal response -> stream back
        let mut response_builder = Response::builder().status(status);
        for (k, v) in res.headers() {
            if !is_hop_by_hop(k) {
                response_builder = response_builder.header(k, v);
            }
        }

        let stream = res.bytes_stream();
        let body = Body::from_stream(stream);

        return response_builder
            .body(body)
            .unwrap_or_else(|_| StatusCode::INTERNAL_SERVER_ERROR.into_response());
    }

    (
        StatusCode::BAD_GATEWAY,
        Json(json!({ "error": { "message": "All upstream attempts exhausted" } })),
    )
        .into_response()
}
