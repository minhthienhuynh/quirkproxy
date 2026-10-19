use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::process::Command;
use tokio::sync::Mutex;
use tracing::{error, info, warn};

/// Minimum time between two WARP identity rotations. A rotation tears down the
/// tunnel for every in-flight request, so this is a safety floor, not a tuning
/// knob: below ~4s Cloudflare also starts returning the same registration.
const ROTATE_COOLDOWN: Duration = Duration::from_secs(4);

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RotateRecord {
    pub old_ip: Option<String>,
    pub new_ip: String,
    pub timestamp: String,
    pub reason: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct WarpStatus {
    pub enabled: bool,
    pub running: bool,
    pub current_ip: Option<String>,
    pub proxy_url: Option<String>,
    pub rotations: usize,
    pub history: Vec<RotateRecord>,
}

pub struct WarpManager {
    socks_port: u16,
    enabled: bool,
    lock: Mutex<()>,
    state: Arc<Mutex<WarpState>>,
    /// Number of requests currently streaming through the WARP tunnel.
    /// Rotating the identity tears down every one of them, so a rotation
    /// scheduled while this is non-zero would truncate another client's
    /// response mid-token.
    inflight: Arc<AtomicUsize>,
    /// Reused HTTP client for egress IP probes.
    probe_client: Option<reqwest::Client>,
}

/// RAII guard that keeps the in-flight counter honest even if the request
/// future is dropped or panics mid-stream.
pub struct InflightGuard {
    counter: Arc<AtomicUsize>,
}

impl InflightGuard {
    pub fn new(counter: Arc<AtomicUsize>) -> Self {
        counter.fetch_add(1, Ordering::Relaxed);
        Self { counter }
    }
}

impl Drop for InflightGuard {
    fn drop(&mut self) {
        self.counter.fetch_sub(1, Ordering::Relaxed);
    }
}

struct WarpState {
    current_ip: Option<String>,
    rotations: usize,
    last_rotated: Instant,
    history: Vec<RotateRecord>,
}

impl WarpManager {
    pub fn new(socks_port: u16, enabled: bool) -> Self {
        Self {
            socks_port,
            enabled,
            lock: Mutex::new(()),
            state: Arc::new(Mutex::new(WarpState {
                current_ip: None,
                rotations: 0,
                last_rotated: Instant::now() - Duration::from_secs(60),
                history: Vec::new(),
            })),
            inflight: Arc::new(AtomicUsize::new(0)),
            probe_client: build_probe_client(socks_port),
        }
    }

    /// Marks a request as in flight through the tunnel for the caller's
    /// lifetime. Hold the returned guard until the response body completes.
    pub fn begin_request(&self) -> InflightGuard {
        InflightGuard::new(self.inflight.clone())
    }

    pub fn inflight_count(&self) -> usize {
        self.inflight.load(Ordering::Relaxed)
    }

    /// Waits until no request is streaming through the tunnel, or the timeout
    /// expires. Returns `true` if the tunnel drained.
    async fn wait_for_drain(&self, timeout: Duration) -> bool {
        let deadline = Instant::now() + timeout;
        while Instant::now() < deadline {
            if self.inflight_count() == 0 {
                return true;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        self.inflight_count() == 0
    }

    pub fn proxy_url(&self) -> String {
        format!("socks5://127.0.0.1:{}", self.socks_port)
    }

    async fn run_warp_cli(&self, args: &[&str]) -> Result<String> {
        let mut cmd = Command::new("warp-cli");
        cmd.arg("--accept-tos").args(args);
        let output = cmd.output().await.context("failed to execute warp-cli")?;
        let stdout = String::from_utf8_lossy(&output.stdout);
        let stderr = String::from_utf8_lossy(&output.stderr);
        let combined = format!("{}\n{}", stdout, stderr).trim().to_string();
        if !output.status.success() {
            warn!("warp-cli {:?} returned non-zero: {}", args, combined);
        }
        Ok(combined)
    }

    pub async fn probe_ip(&self) -> Option<String> {
        // Building a reqwest Client per probe allocates a TLS config and a
        // connection pool every time. Rotations probe repeatedly, so the probe
        // client is built once and reused (pooling is disabled anyway, so no
        // stale connection survives a re-registration).
        let client = match &self.probe_client {
            Some(c) => c.clone(),
            None => return None,
        };

        // 1. Primary: Cloudflare Edge Trace (fastest, lowest latency via WARP)
        if let Ok(resp) = client
            .get("https://cloudflare.com/cdn-cgi/trace")
            .send()
            .await
        {
            if let Ok(text) = resp.text().await {
                for line in text.lines() {
                    if let Some(ip) = line.strip_prefix("ip=") {
                        let trimmed = ip.trim();
                        if !trimmed.is_empty() {
                            return Some(trimmed.to_string());
                        }
                    }
                }
            }
        }

        // 2. Secondary: api.ipify.org
        #[derive(Deserialize)]
        struct IpifyResp {
            ip: String,
        }

        if let Ok(resp) = client.get("https://api.ipify.org?format=json").send().await {
            if let Ok(data) = resp.json::<IpifyResp>().await {
                return Some(data.ip);
            }
        }

        // 3. Fallback: ifconfig.me
        if let Ok(resp) = client.get("https://ifconfig.me/ip").send().await {
            if let Ok(text) = resp.text().await {
                let trimmed = text.trim().to_string();
                if !trimmed.is_empty() {
                    return Some(trimmed);
                }
            }
        }

        None
    }

    pub async fn start(&self) -> Result<()> {
        if !self.enabled {
            info!("Cloudflare WARP is disabled in config.");
            return Ok(());
        }

        info!(
            "Starting WARP in SOCKS5 Proxy Mode on 127.0.0.1:{}...",
            self.socks_port
        );

        // Ensure dbus service is running if on Linux
        let _ = Command::new("service")
            .args(["dbus", "start"])
            .output()
            .await;

        // Verify warp-svc daemon is running
        let pgrep = Command::new("pgrep").arg("warp-svc").output().await;
        let is_running = pgrep.map(|o| o.status.success()).unwrap_or(false);
        if !is_running {
            info!("Spawning background warp-svc daemon...");
            let mut spawn_cmd = Command::new("warp-svc");
            spawn_cmd
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null());
            if let Err(e) = spawn_cmd.spawn() {
                warn!("Could not spawn warp-svc directly: {}", e);
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }

        // Check registration
        let reg_out = self
            .run_warp_cli(&["registration", "show"])
            .await
            .unwrap_or_default();
        if reg_out.contains("Error") || reg_out.contains("No registration") {
            info!("Registering new WARP client device...");
            let _ = self.run_warp_cli(&["registration", "new"]).await;
            tokio::time::sleep(Duration::from_secs(1)).await;
        }

        // Configure SOCKS5 Proxy Mode (Zero host network interface / routing change)
        let _ = self.run_warp_cli(&["mode", "proxy"]).await;
        let port_str = self.socks_port.to_string();
        let _ = self.run_warp_cli(&["proxy", "port", &port_str]).await;
        let _ = self.run_warp_cli(&["connect"]).await;

        tokio::time::sleep(Duration::from_secs(3)).await;

        if let Some(ip) = self.probe_ip().await {
            info!(
                "WARP SOCKS5 Proxy Ready! Egress IP: {} (port :{})",
                ip, self.socks_port
            );
            let mut st = self.state.lock().await;
            st.current_ip = Some(ip);
        } else {
            warn!("WARP started, but initial public IP probe timed out.");
        }

        Ok(())
    }

    /// Rotates the WARP identity.
    ///
    /// `wait_for_drain` is what separates a request-triggered rotation from an
    /// operator one. A request that already failed may rotate immediately: its
    /// own stream is gone, and waiting would add latency to the retry. A
    /// scheduled or manual rotation must not cut through other clients'
    /// in-flight streams, so it waits for the tunnel to drain first.
    pub async fn rotate(&self, reason: &str, wait_for_drain: bool) -> Result<Option<String>> {
        // Hold the rotation mutex only to decide *whether* to rotate, then
        // release it. Holding it for the whole 5-30s rotation serialises every
        // concurrent caller behind a mutex, so a burst of 429s turns into a
        // queue of blocked tasks instead of a single rotation others can skip.
        {
            let _guard = self.lock.lock().await;
            let st = self.state.lock().await;
            if st.last_rotated.elapsed() < ROTATE_COOLDOWN {
                warn!("WARP rotation throttled (within cooldown period).");
                return Ok(st.current_ip.clone());
            }
        }

        if wait_for_drain {
            let drained = self.wait_for_drain(Duration::from_secs(30)).await;
            if !drained {
                warn!(
                    "Rotation deferred: {} request(s) still streaming after 30s.",
                    self.inflight_count()
                );
                let st = self.state.lock().await;
                return Ok(st.current_ip.clone());
            }
        }

        // Claim the rotation slot. Whoever gets here first wins; anyone else
        // waits for that rotation and then reads its result instead of
        // starting a second concurrent rotation.
        let _guard = self.lock.lock().await;

        let (old_ip, still_stale) = {
            let st = self.state.lock().await;
            (
                st.current_ip.clone(),
                st.last_rotated.elapsed() >= ROTATE_COOLDOWN,
            )
        };

        if !still_stale {
            // Another task completed a rotation while we waited: reuse it.
            info!("Rotation already performed by a concurrent request; reusing result.");
            return Ok(old_ip);
        }

        warn!(
            "Rotating WARP IP... Reason: {} (Current: {:?})",
            reason, old_ip
        );

        let mut final_ip = None;
        for attempt in 1..=4 {
            let _ = self.run_warp_cli(&["disconnect"]).await;
            tokio::time::sleep(Duration::from_millis(500)).await;

            let _ = self.run_warp_cli(&["registration", "delete"]).await;
            tokio::time::sleep(Duration::from_millis(500)).await;

            let _ = self.run_warp_cli(&["registration", "new"]).await;
            tokio::time::sleep(Duration::from_millis(500)).await;

            let port_str = self.socks_port.to_string();
            let _ = self.run_warp_cli(&["mode", "proxy"]).await;
            let _ = self.run_warp_cli(&["proxy", "port", &port_str]).await;
            let _ = self.run_warp_cli(&["connect"]).await;

            tokio::time::sleep(Duration::from_secs(3)).await;

            let candidate = self.probe_ip().await;
            if let Some(ref cand) = candidate {
                if old_ip.as_ref() != Some(cand) {
                    final_ip = candidate;
                    break;
                }
                // Same address again: retry, but do not claim success below.
                warn!(
                    "Attempt {}/4: assigned IP {} is identical to the old one; re-registering...",
                    attempt, cand
                );
            }
        }

        let mut st = self.state.lock().await;
        st.last_rotated = Instant::now();

        // A rotation that returns the same address is a failure: reporting it
        // as success would leave a blacklisted IP in place while the caller
        // believes it now has a clean one.
        let success = matches!(
            final_ip,
            Some(ref ip) if old_ip.as_ref() != Some(ip)
        );

        if success {
            let ip = final_ip.clone().unwrap_or_default();
            info!(
                "WARP rotated successfully! New IP: {} (was {:?})",
                ip, old_ip
            );
            st.current_ip = Some(ip.clone());
            st.rotations += 1;
            st.history.push(RotateRecord {
                old_ip,
                new_ip: ip,
                timestamp: chrono_now(),
                reason: reason.to_string(),
            });
            if st.history.len() > 20 {
                st.history.remove(0);
            }
        } else if let Some(ref ip) = final_ip {
            warn!(
                "WARP re-registered after 4 attempts but kept IP {} (old {:?}); not counting as a rotation.",
                ip, old_ip
            );
            st.current_ip = final_ip.clone();
        } else {
            error!("WARP rotation completed, but could not verify any new IP.");
        }

        Ok(final_ip)
    }

    pub async fn status(&self) -> WarpStatus {
        let st = self.state.lock().await;
        WarpStatus {
            enabled: self.enabled,
            running: st.current_ip.is_some(),
            current_ip: st.current_ip.clone(),
            proxy_url: if self.enabled {
                Some(self.proxy_url())
            } else {
                None
            },
            rotations: st.rotations,
            history: st.history.clone(),
        }
    }
}

fn chrono_now() -> String {
    use std::time::SystemTime;
    let now = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    format!("{}", now)
}

/// Builds the egress-IP probe client once. Connection pooling is disabled so a
/// probe can never reuse a socket from the previous WARP registration.
fn build_probe_client(socks_port: u16) -> Option<reqwest::Client> {
    let proxy = match reqwest::Proxy::all(format!("socks5://127.0.0.1:{}", socks_port)) {
        Ok(p) => p,
        Err(e) => {
            warn!("failed to create reqwest proxy: {}", e);
            return None;
        }
    };

    match reqwest::Client::builder()
        .proxy(proxy)
        .pool_max_idle_per_host(0)
        .timeout(std::time::Duration::from_secs(5))
        .build()
    {
        Ok(c) => Some(c),
        Err(e) => {
            warn!("failed to build reqwest probe client: {}", e);
            None
        }
    }
}
