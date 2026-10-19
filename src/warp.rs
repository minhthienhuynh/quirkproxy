use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::process::Command;
use tokio::sync::Mutex;
use tracing::{error, info, warn};

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
        }
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
        let proxy_url = self.proxy_url();
        let proxy = match reqwest::Proxy::all(&proxy_url) {
            Ok(p) => p,
            Err(e) => {
                warn!("failed to create reqwest proxy: {}", e);
                return None;
            }
        };

        let client = match reqwest::Client::builder()
            .proxy(proxy)
            .pool_max_idle_per_host(0)
            .timeout(Duration::from_secs(5))
            .build()
        {
            Ok(c) => c,
            Err(e) => {
                warn!("failed to build reqwest probe client: {}", e);
                return None;
            }
        };

        // 1. Primary: Cloudflare Edge Trace (fastest, lowest latency via WARP)
        if let Ok(resp) = client.get("https://cloudflare.com/cdn-cgi/trace").send().await {
            if let Ok(text) = resp.text().await {
                for line in text.lines() {
                    if let Some(ip) = line.strip_prefix("ip=") {
                        let trimmed = ip.trim().to_string();
                        if !trimmed.is_empty() {
                            return Some(trimmed);
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

        info!("Starting WARP in SOCKS5 Proxy Mode on 127.0.0.1:{}...", self.socks_port);

        // Ensure dbus service is running if on Linux
        let _ = Command::new("service").args(["dbus", "start"]).output().await;

        // Verify warp-svc daemon is running
        let pgrep = Command::new("pgrep").arg("warp-svc").output().await;
        let is_running = pgrep.map(|o| o.status.success()).unwrap_or(false);
        if !is_running {
            info!("Spawning background warp-svc daemon...");
            let mut spawn_cmd = Command::new("warp-svc");
            spawn_cmd.stdout(std::process::Stdio::null()).stderr(std::process::Stdio::null());
            if let Err(e) = spawn_cmd.spawn() {
                warn!("Could not spawn warp-svc directly: {}", e);
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }

        // Check registration
        let reg_out = self.run_warp_cli(&["registration", "show"]).await.unwrap_or_default();
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
            info!("WARP SOCKS5 Proxy Ready! Egress IP: {} (port :{})", ip, self.socks_port);
            let mut st = self.state.lock().await;
            st.current_ip = Some(ip);
        } else {
            warn!("WARP started, but initial public IP probe timed out.");
        }

        Ok(())
    }

    pub async fn rotate(&self, reason: &str) -> Result<Option<String>> {
        let _guard = self.lock.lock().await;

        let (old_ip, can_rotate) = {
            let st = self.state.lock().await;
            let elapsed = st.last_rotated.elapsed();
            (st.current_ip.clone(), elapsed >= Duration::from_secs(4))
        };

        if !can_rotate {
            warn!("WARP rotation throttled (within cooldown period).");
            return Ok(old_ip);
        }

        warn!("Rotating WARP IP... Reason: {} (Current: {:?})", reason, old_ip);

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
                } else {
                    warn!(
                        "Attempt {}: Assigned IP ({}) identical to old IP. Retrying registration...",
                        attempt, cand
                    );
                    final_ip = candidate;
                }
            }
        }

        let new_ip = final_ip;
        let mut st = self.state.lock().await;
        st.last_rotated = Instant::now();

        if let Some(ref ip) = new_ip {
            info!("WARP rotated successfully! New IP: {} (was {:?})", ip, old_ip);
            st.current_ip = new_ip.clone();
            st.rotations += 1;
            st.history.push(RotateRecord {
                old_ip,
                new_ip: ip.clone(),
                timestamp: chrono_now(),
                reason: reason.to_string(),
            });
            if st.history.len() > 20 {
                st.history.remove(0);
            }
        } else {
            error!("WARP rotation completed, but could not verify new IP.");
        }

        Ok(new_ip)
    }

    pub async fn status(&self) -> WarpStatus {
        let st = self.state.lock().await;
        WarpStatus {
            enabled: self.enabled,
            running: st.current_ip.is_some(),
            current_ip: st.current_ip.clone(),
            proxy_url: if self.enabled { Some(self.proxy_url()) } else { None },
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
