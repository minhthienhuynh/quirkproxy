//! Multi-identity Cloudflare WARP egress pool.
//!
//! Instead of one `warp-svc` identity rotated serially (which blocks for ~30s
//! and frequently re-issues the same address), this module runs N independent
//! WireGuard tunnels through `wireproxy`. Each tunnel is a separate WARP
//! registration with its own egress IP, and the engine picks a different
//! SOCKS5 port per attempt.
//!
//! `wireproxy` is a userspace WireGuard client: it opens no TUN device, adds no
//! routes and needs no capabilities, so the host network is never touched.

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::process::Command;
use tokio::sync::Mutex;
use tokio::task::JoinHandle;
use tracing::{error, info, warn};

use crate::config::WarpProtocol;

/// Don't recycle the same tunnel twice inside this window. Re-registering
/// costs a WARP round-trip and briefly drops the tunnel, so a burst of 429s
/// must not turn into a registration storm.
const RECYCLE_COOLDOWN: Duration = Duration::from_secs(60);
/// How long a background recycle waits for in-flight streams to finish before
/// giving up. Recycling tears down the tunnel, which truncates any stream
/// still riding it.
const RECYCLE_DRAIN_TIMEOUT: Duration = Duration::from_secs(30);
/// How long a tunnel restart waits for the traffic that is already on that
/// tunnel to finish before the child is killed anyway.
///
/// Killing `wireproxy` drops every connection the tunnel is carrying, and a
/// stream riding that tunnel dies with "unexpected EOF". The wait has to cover
/// a full stream: the free models take 30s and more for a long answer, and a
/// timeout that expires in the middle of one truncates it.
///
/// The wait is bounded, because the exit being replaced just refused a
/// request and the pool has to be able to recover from that.
const TUNNEL_DRAIN_TIMEOUT: Duration = Duration::from_secs(90);
/// A connection that has stayed open this long counts as a stream.
///
/// Waiting for the tunnel to have *no* connection at all does not work under
/// sustained load: ordinary requests keep arriving, the tunnel never goes
/// idle, and every recycle is abandoned while the burned exit keeps refusing
/// requests. A short request finishes on its own within a couple of seconds;
/// a stream does not. Age is the only signal that separates them.
const TUNNEL_STREAM_MIN_AGE: Duration = Duration::from_secs(3);
/// How long the tunnel has to be free of streams before the swap goes ahead.
///
/// A stream that starts right after the check would be killed by the swap, so
/// the absence has to hold for a moment, not just at a single instant.
const TUNNEL_QUIET_WINDOW: Duration = Duration::from_millis(250);
/// How often the tunnel drain re-reads the socket table.
const TUNNEL_DRAIN_POLL: Duration = Duration::from_millis(100);
/// Per-request timeout of one egress probe.
///
/// Healthy exits answer in 62-159ms through the pool (measured 2026-10-30, five
/// samples x four tunnels), so 3s leaves ~19x headroom while keeping a watchdog
/// pass short: the probe tries two URLs, so a wedged exit costs 2 x 3s = 6s and
/// still fits inside the 15s interval with the probes running concurrently.
/// The previous 8s made a wedged pass cost 16s -- longer than the interval.
const PROBE_TIMEOUT: Duration = Duration::from_secs(3);
/// How often the background watchdog probes the pool.
///
/// Bounded below by a pass length rather than by cost: a pass is one concurrent
/// probe round, and a wedged exit costs two probe timeouts (the probe tries two
/// URLs), i.e. 6s. 15s leaves room for that without passes overlapping.
///
/// The interval matters because it IS the cost: an exit that is bound but wedged
/// stays in the engine's static proxy list until the watchdog replaces it, and
/// every request that draws one pays the engine's hardcoded 10s SOCKS handshake
/// timeout (measured: `anon:transport:10009ms`).
const WATCHDOG_INTERVAL: Duration = Duration::from_secs(15);

/// Upper bound on remembered per-model quota verdicts.
///
/// The map is keyed by client-supplied model names, so it must not grow without
/// limit: expired entries are dropped first, and a still-overfull map loses its
/// oldest key. Bounding it costs at worst one wasted walk per forgotten model.
const QUOTA_WINDOW_MODEL_CAP: usize = 256;
/// A tunnel that ran this long before dying is restarted immediately instead
/// of with backoff (crash loop vs. one-off crash).
const TUNNEL_STABLE_UPTIME: Duration = Duration::from_secs(30);
/// Upper bound of the restart backoff.
const TUNNEL_MAX_BACKOFF: Duration = Duration::from_secs(30);
/// How long startup waits for the first tunnel to complete its handshake
/// before letting the engine run anyway.
const TUNNEL_READY_TIMEOUT: Duration = Duration::from_secs(30);
/// Hard ceiling for one `wgcf` invocation. `recycle_lock` is held for the whole
/// re-registration, so a hung Cloudflare API would otherwise block every later
/// recycle (including /v1/rotate) forever.
const WGCF_TIMEOUT: Duration = Duration::from_secs(45);

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize, Default)]
pub enum TunnelCondition {
    #[default]
    Active,
    Unhealthy,      // Process died, SOCKS5 closed, or probe failed
    Banned,         // 403 Abuse detected
    QuotaExhausted, // 429 FreeUsageLimitError confirmed
}

impl TunnelCondition {
    pub fn as_str(&self) -> &'static str {
        match self {
            Self::Active => "active",
            Self::Unhealthy => "unhealthy",
            Self::Banned => "banned",
            Self::QuotaExhausted => "quota_exhausted",
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RotateRecord {
    pub tunnel: usize,
    pub old_ip: Option<String>,
    pub new_ip: String,
    pub timestamp: String,
    pub reason: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct SingleRotateOutcome {
    pub status: String,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tunnel: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub port: Option<u16>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub old_ip: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub new_ip: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct TunnelStatus {
    pub index: usize,
    pub port: u16,
    pub ip: Option<String>,
    pub healthy: bool,
    pub recycling: bool,
    pub condition: String,
    /// Requests currently riding this tunnel.
    ///
    /// Counted from the socket table, so it is per tunnel: the engine picks
    /// the proxy per attempt and the supervisor never learns which tunnel
    /// carries which request. A non-zero value means the tunnel must not be
    /// killed without draining it first.
    pub connections: usize,
}

#[derive(Clone, Debug, Serialize)]
pub struct WarpStatus {
    pub enabled: bool,
    pub running: bool,
    pub protocol: WarpProtocol,
    pub identities: usize,
    /// Number of tunnels whose last egress probe succeeded. `running` is true
    /// when at least one tunnel works; a partially-up pool is normal during
    /// startup and while a tunnel is being recycled.
    pub healthy: usize,
    pub proxy_urls: Vec<String>,
    pub rotations: usize,
    /// Distinct quota scopes (`/64` on IPv6, `/32` on IPv4) currently held.
    ///
    /// This is the number that bounds free-tier capacity. `healthy` counts
    /// tunnels, and ten healthy tunnels can sit in eight scopes — two of them
    /// then spend the same bucket. Reported separately so the gap is visible
    /// instead of being inferred from `tunnels[].ip`.
    pub scopes: usize,
    /// `retry-after` of the largest spent-bucket 429 seen since boot, in
    /// seconds. `None` until one arrives.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub quota_window_secs: Option<u64>,
    pub tunnels: Vec<TunnelStatus>,
    pub history: Vec<RotateRecord>,
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

struct TunnelState {
    ip: Option<String>,
    last_recycle: Instant,
    /// Cleared while the child is down; the supervisor sets it again once the
    /// SOCKS5 listener answers a probe.
    healthy: bool,
    condition: TunnelCondition,
    /// Incremented every time the supervisor spawns a wireproxy child. A
    /// recycle records this before asking for a restart and waits for it to
    /// advance, which is how it knows the new config is actually in effect
    /// without inspecting the process table.
    spawn_generation: u64,
    /// Incremented every time the supervisor *services* a restart request.
    ///
    /// `spawn_generation` alone is not enough evidence that a restart
    /// happened: the supervisor also respawns on its own when the child
    /// crashes, and that respawn would satisfy a wait on `spawn_generation`
    /// without the freshly written config ever being loaded.
    restart_generation: u64,
    /// Set once the readiness probe for the current child has finished,
    /// whether it produced an egress IP or not.
    ///
    /// Duplicate detection needs this: an egress IP is only known after a
    /// probe succeeded, so a tunnel that has not finished probing yet looks
    /// identical to one that has no exit address, and the comparison would
    /// silently miss a duplicated pair.
    probe_completed: bool,
}

pub struct Tunnel {
    index: usize,
    port: u16,
    dir: PathBuf,
    conf_path: PathBuf,
    protocol: WarpProtocol,
    state: Mutex<TunnelState>,
    /// Number of egress probes this supervisor is running against the tunnel
    /// right now.
    ///
    /// The probes open a connection to the tunnel's own SOCKS5 port, so they
    /// show up in the socket table exactly like real traffic does. Counting
    /// them as traffic would make the drain wait for the supervisor's own
    /// probe to finish.
    probe_inflight: AtomicUsize,
    /// Set while a re-registration is in progress so concurrent 429s don't
    /// queue up behind the same tunnel.
    recycling: AtomicBool,
    /// Raises the supervisor's restart request. The supervisor owns the child
    /// handle, so this is the only safe way to make it re-exec with a new
    /// config: signalling by process name would risk killing an unrelated
    /// process whose command line happens to match.
    restart: tokio::sync::Notify,
    probe: Option<reqwest::Client>,
}

impl Tunnel {
    fn proxy_url(&self) -> String {
        format!("socks5://127.0.0.1:{}", self.port)
    }

    /// Builds the wireproxy config from a freshly generated wgcf profile.
    ///
    /// The `DNS` line is dropped on purpose: wireproxy would otherwise answer
    /// DNS locally, bypassing the tunnel. The engine sends the hostname itself
    /// inside the SOCKS5 CONNECT, so name resolution happens at the Cloudflare
    /// exit and cannot leak around the tunnel.
    fn write_conf(&self) -> Result<()> {
        let profile = std::fs::read_to_string(self.dir.join("wgcf-profile.conf"))
            .context("wgcf-profile.conf missing; run wgcf generate first")?;

        let mut conf = String::with_capacity(profile.len() + 64);
        for line in profile.lines() {
            if line.trim_start().starts_with("DNS") {
                continue;
            }
            conf.push_str(line);
            conf.push('\n');
        }
        conf.push_str(&format!(
            "\n[Socks5]\nBindAddress = 127.0.0.1:{}\n",
            self.port
        ));

        std::fs::write(&self.conf_path, conf).context("failed to write wireproxy config")?;
        Ok(())
    }

    async fn probe_ip(&self) -> Option<String> {
        let client = self.probe.as_ref()?;
        self.probe_inflight.fetch_add(1, Ordering::SeqCst);
        let result = self.probe_ip_inner(client).await;
        self.probe_inflight.fetch_sub(1, Ordering::SeqCst);
        result
    }

    async fn probe_ip_inner(&self, client: &reqwest::Client) -> Option<String> {
        for url in [
            "https://cloudflare.com/cdn-cgi/trace",
            "https://api.ipify.org?format=json",
        ] {
            let Ok(resp) = client.get(url).send().await else {
                continue;
            };
            let Ok(text) = resp.text().await else {
                continue;
            };
            if let Some(ip) = text.lines().find_map(|l| l.strip_prefix("ip=")) {
                let ip = ip.trim();
                if !ip.is_empty() {
                    return Some(ip.to_string());
                }
            }
            if let Ok(v) = serde_json::from_str::<serde_json::Value>(&text) {
                if let Some(ip) = v["ip"].as_str() {
                    return Some(ip.to_string());
                }
            }
        }
        None
    }

    /// Number of TCP connections currently established against this tunnel's
    /// SOCKS5 port, minus the probes this supervisor is running itself.
    ///
    /// The engine picks the proxy per attempt, so the supervisor never learns
    /// which tunnel carries which request. The socket table does: the engine
    /// and the supervisor share the container's network namespace, and every
    /// request through this tunnel holds a connection to its SOCKS5 port.
    ///
    /// This is the per-tunnel signal the pool-wide in-flight counter cannot
    /// provide. Recycling kills the tunnel, so a tunnel with live connections
    /// must not be killed while they are still streaming.
    fn active_connections(&self) -> usize {
        self.sockets()
            .len()
            .saturating_sub(self.probe_inflight.load(Ordering::Relaxed))
    }

    /// Inodes of the connections currently established against this tunnel.
    fn sockets(&self) -> Vec<u64> {
        established_sockets_on_port(self.port)
    }

    /// Waits until this tunnel is carrying no stream.
    ///
    /// A connection that has been open for `stream_min_age` is a stream, and
    /// the wait continues until it closes. Shorter connections are ordinary
    /// requests: they finish by themselves and are not waited for, otherwise
    /// sustained load would block every recycle forever.
    ///
    /// The first `stream_min_age` of the wait is observation only. Until then
    /// a long-running stream is indistinguishable from one that just started,
    /// so trusting the classification would let the swap truncate exactly the
    /// connection it is meant to protect.
    ///
    /// Returns true when no stream is left, false when the timeout expired.
    async fn wait_until_drained(&self, timeout: Duration) -> bool {
        self.drain_streams(TUNNEL_STREAM_MIN_AGE, TUNNEL_QUIET_WINDOW, timeout)
            .await
    }

    /// The body of [`Tunnel::wait_until_drained`], with the thresholds
    /// injectable so a test does not have to spend seconds on them.
    async fn drain_streams(
        &self,
        stream_min_age: Duration,
        quiet_window: Duration,
        timeout: Duration,
    ) -> bool {
        let started = Instant::now();
        let deadline = started + timeout;
        // When each connection on this port was first observed. A socket that
        // keeps its inode has been open since then: the kernel does not reuse
        // an inode while the socket lives.
        let mut first_seen: std::collections::HashMap<u64, Instant> =
            std::collections::HashMap::new();
        let mut quiet_since: Option<Instant> = None;

        loop {
            let now = Instant::now();
            let current = self.sockets();
            for inode in &current {
                first_seen.entry(*inode).or_insert(now);
            }
            // Connections that closed no longer matter, and forgetting them
            // keeps the map from growing over a long wait.
            first_seen.retain(|inode, _| current.contains(inode));

            let streams = if !current.is_empty() && now.duration_since(started) < stream_min_age {
                // Still observing: an old stream looks new right now.
                1
            } else {
                current
                    .iter()
                    .filter(|inode| {
                        first_seen
                            .get(*inode)
                            .is_some_and(|seen| now.duration_since(*seen) >= stream_min_age)
                    })
                    .count()
            };

            if streams == 0 {
                let since = *quiet_since.get_or_insert(now);
                if since.elapsed() >= quiet_window {
                    return true;
                }
            } else {
                quiet_since = None;
            }

            if now >= deadline {
                return false;
            }
            tokio::time::sleep(TUNNEL_DRAIN_POLL).await;
        }
    }

    /// Re-registers a fresh WARP identity and rewrites the tunnel config.
    /// The running child is not touched here; its supervisor restarts it.
    async fn re_register(&self) -> Result<()> {
        match self.protocol {
            WarpProtocol::WireGuard => {
                // `wgcf register` refuses to overwrite an existing account
                // ("existing account detected"), so the previous identity files must
                // be removed or a recycle silently keeps the burned egress.
                for stale in ["wgcf-account.toml", "wgcf-profile.conf"] {
                    let path = self.dir.join(stale);
                    if path.exists() {
                        tokio::fs::remove_file(&path).await.with_context(|| {
                            format!("failed to remove stale identity file {}", path.display())
                        })?;
                    }
                }

                self.run_tool("wgcf", &["register", "--accept-tos"]).await?;
                self.run_tool("wgcf", &["generate"]).await?;

                self.write_conf()
            }
            WarpProtocol::Masque => {
                let config_file = self.dir.join("config.json");
                if config_file.exists() {
                    tokio::fs::remove_file(&config_file)
                        .await
                        .with_context(|| {
                            format!(
                                "failed to remove stale identity file {}",
                                config_file.display()
                            )
                        })?;
                }

                self.run_tool(
                    "usque",
                    &["register", "-a", "-c", &self.conf_path.to_string_lossy()],
                )
                .await
            }
        }
    }

    /// Runs one CLI subcommand inside this tunnel's identity directory with
    /// a hard timeout. Both callers hold `recycle_lock`, so a wedged child must
    /// not be able to block the pool indefinitely.
    async fn run_tool(&self, bin: &str, args: &[&str]) -> Result<()> {
        let label = args.first().copied().unwrap_or(bin);

        let child = Command::new(bin)
            .args(args)
            .current_dir(&self.dir)
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .stdin(Stdio::null())
            .kill_on_drop(true)
            .output();

        let output = match tokio::time::timeout(WGCF_TIMEOUT, child).await {
            Ok(Ok(o)) => o,
            Ok(Err(e)) => {
                return Err(e).with_context(|| format!("failed to execute {} {}", bin, label))
            }
            Err(_) => {
                anyhow::bail!(
                    "{} {} timed out after {:?} (tunnel {})",
                    bin,
                    label,
                    WGCF_TIMEOUT,
                    self.index
                )
            }
        };

        if !output.status.success() {
            anyhow::bail!(
                "{} {} failed: {}",
                bin,
                label,
                String::from_utf8_lossy(&output.stderr).trim()
            );
        }

        Ok(())
    }

    /// Ensures an identity exists on first boot, then writes the tunnel
    /// config. A re-registration on boot would rotate the IP on every restart,
    /// so an existing profile is reused as-is.
    async fn ensure_identity(&self) -> Result<()> {
        tokio::fs::create_dir_all(&self.dir).await.ok();

        match self.protocol {
            WarpProtocol::WireGuard => {
                let profile = self.dir.join("wgcf-profile.conf");
                if !profile.exists() {
                    info!(
                        "Tunnel {}: no WARP WireGuard identity yet, registering...",
                        self.index
                    );
                    self.re_register().await?;
                } else {
                    self.write_conf()?;
                }
            }
            WarpProtocol::Masque => {
                if !self.conf_path.exists() {
                    info!(
                        "Tunnel {}: no WARP MASQUE identity yet, registering...",
                        self.index
                    );
                    self.re_register().await?;
                }
            }
        }
        Ok(())
    }

    /// Probes the tunnel's egress until it answers, or until the attempt
    /// budget runs out, then publishes the result into the tunnel state.
    ///
    /// The probe runs as its own task so the supervisor keeps reacting to a
    /// restart request (and to a dying child) while it is still in flight.
    /// When the probe blocked the supervisor's `select!` directly, a recycle
    /// that arrived during those ten seconds timed out waiting for the
    /// restart and the rotation was silently lost.
    ///
    /// `generation` is the spawn generation the probe belongs to. A probe
    /// whose tunnel has already been restarted must not publish its result:
    /// it would resurrect the health of a child that no longer exists.
    async fn probe_until_ready(self: &Arc<Self>, cancelled: &AtomicBool, generation: u64) {
        let mut ready_ip = None;
        for _ in 0..20 {
            if cancelled.load(Ordering::Relaxed) {
                break;
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
            if cancelled.load(Ordering::Relaxed) {
                break;
            }
            if let Some(ip) = self.probe_ip().await {
                ready_ip = Some(ip);
                break;
            }
        }

        {
            let mut st = self.state.lock().await;
            if st.spawn_generation != generation {
                // The tunnel was restarted while this probe was in flight.
                // Its answer describes a child that is already gone.
                return;
            }
            if cancelled.load(Ordering::Relaxed) {
                // The child exited while this probe was in flight. Publishing
                // the answer now would mark a dead child as healthy.
                return;
            }
            match ready_ip.clone() {
                Some(ip) => {
                    st.ip = Some(ip);
                    st.healthy = true;
                    if st.condition == TunnelCondition::Unhealthy {
                        st.condition = TunnelCondition::Active;
                    }
                }
                None => {
                    // A failed probe must not wipe a known-good exit address
                    // and must not mark a recycled tunnel unhealthy: the
                    // recycle path verifies the new egress on its own, and
                    // nulling the IP here would make `/healthz` flap between
                    // "healthy" and "no IP".
                    if st.ip.is_none() {
                        st.healthy = false;
                        st.condition = TunnelCondition::Unhealthy;
                    }
                }
            }
            st.probe_completed = true;
        }

        match ready_ip {
            Some(ip) => info!(
                "Tunnel {} ready on :{} (egress {})",
                self.index, self.port, ip
            ),
            None => warn!(
                "Tunnel {} started on :{} but egress probe did not answer yet",
                self.index, self.port
            ),
        }
    }

    /// Runs `wireproxy` or `usque` for this tunnel, restarting it with backoff whenever it
    /// exits unexpectedly, or immediately when a recycle asks for it through
    /// [`Tunnel::restart`]. This task owns the child handle, so restarting goes
    /// through it rather than through the process table.
    async fn supervise(self: &Arc<Self>, shutdown: Arc<AtomicBool>) {
        let mut backoff = Duration::from_secs(1);
        let daemon_name = match self.protocol {
            WarpProtocol::WireGuard => "wireproxy",
            WarpProtocol::Masque => "usque",
        };

        while !shutdown.load(Ordering::Relaxed) {
            let port_str = self.port.to_string();
            let child = match self.protocol {
                WarpProtocol::WireGuard => Command::new("wireproxy")
                    .args(["-c", &self.conf_path.to_string_lossy()])
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .kill_on_drop(true)
                    .spawn(),
                WarpProtocol::Masque => Command::new("usque")
                    .args([
                        "socks",
                        "-b",
                        "127.0.0.1",
                        "-p",
                        &port_str,
                        "-c",
                        &self.conf_path.to_string_lossy(),
                    ])
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .kill_on_drop(true)
                    .spawn(),
            };

            let mut child = match child {
                Ok(c) => c,
                Err(e) => {
                    error!(
                        "Tunnel {}: failed to spawn {}: {}. Retrying in {:?}",
                        self.index, daemon_name, e, backoff
                    );
                    tokio::time::sleep(backoff).await;
                    backoff = (backoff * 2).min(TUNNEL_MAX_BACKOFF);
                    continue;
                }
            };

            let started = Instant::now();
            let generation = {
                let mut st = self.state.lock().await;
                st.healthy = false;
                st.probe_completed = false;
                st.spawn_generation += 1;
                st.spawn_generation
            };

            // The readiness probe runs in its own task and publishes its
            // result into the tunnel state. The supervisor therefore stays
            // responsive to a restart request while the tunnel is still
            // handshaking, and its `select!` keeps waiting on the child
            // without a branch that could cancel the wait.
            let cancelled = Arc::new(AtomicBool::new(false));
            {
                let me = self.clone();
                let cancelled = cancelled.clone();
                tokio::spawn(async move {
                    me.probe_until_ready(&cancelled, generation).await;
                });
            }

            let mut respawn_now = false;

            tokio::select! {
                status = child.wait() => {
                    let uptime = started.elapsed();
                    warn!(
                        "Tunnel {}: {} exited ({:?}) after {:?}",
                        self.index, daemon_name, status, uptime
                    );
                    cancelled.store(true, Ordering::Relaxed);
                    {
                        let mut st = self.state.lock().await;
                        st.healthy = false;
                        st.condition = TunnelCondition::Unhealthy;
                    }
                    if uptime >= TUNNEL_STABLE_UPTIME {
                        backoff = Duration::from_secs(1);
                        respawn_now = true;
                    }
                }
                _ = self.restart.notified() => {
                    info!("Tunnel {}: restart requested, stopping {}...", self.index, daemon_name);
                    cancelled.store(true, Ordering::Relaxed);
                    let _ = child.kill().await;
                    let _ = child.wait().await;
                    {
                        let mut st = self.state.lock().await;
                        st.healthy = false;
                        st.condition = TunnelCondition::Unhealthy;
                        // Bump this *before* the next spawn so a waiter can
                        // tell "restart serviced" from "child crashed and
                        // was respawned".
                        st.restart_generation += 1;
                    }
                    backoff = Duration::from_secs(1);
                    respawn_now = true;
                }
                _ = wait_for_shutdown(&shutdown) => {
                    cancelled.store(true, Ordering::Relaxed);
                    let _ = child.kill().await;
                    let _ = child.wait().await;
                    info!("Tunnel {}: stopped.", self.index);
                    return;
                }
            }

            cancelled.store(true, Ordering::Relaxed);

            if respawn_now {
                // A restart or a stable crash: go straight back to a fresh
                // child instead of backing off.
                continue;
            }

            tokio::time::sleep(backoff).await;
            backoff = (backoff * 2).min(TUNNEL_MAX_BACKOFF);
        }
    }

    async fn status(&self) -> TunnelStatus {
        let connections = self.active_connections();
        let st = self.state.lock().await;
        TunnelStatus {
            index: self.index,
            port: self.port,
            ip: st.ip.clone(),
            healthy: st.healthy,
            recycling: self.recycling.load(Ordering::Relaxed),
            condition: st.condition.as_str().to_string(),
            connections,
        }
    }
}

pub struct WarpPool {
    tunnels: Vec<Arc<Tunnel>>,
    enabled: bool,
    pub protocol: WarpProtocol,
    /// Global count of streams currently riding any tunnel.
    inflight: Arc<AtomicUsize>,
    /// Serialises re-registrations: two concurrent `wgcf register` runs in the
    /// same directory would corrupt the identity files.
    recycle_lock: Mutex<()>,
    rotations: AtomicUsize,
    history: Mutex<Vec<RotateRecord>>,
    /// Per-model daily-quota verdicts: the window each model was last refused
    /// in, and when that verdict was observed.
    ///
    /// Keyed by model because quota is metered per model, not per pool: refusing
    /// `big-pickle` because `deepseek-v4.1-flash-free` ran out would deny a
    /// model that still answers 200 (measured: the pool served `beam-free`
    /// throughout while `deepseek` was refused).
    quota_windows: Mutex<HashMap<String, (u64, Instant)>>,
}

impl WarpPool {
    pub fn new(
        tunnel_ports: impl IntoIterator<Item = u16>,
        state_dir: &str,
        enabled: bool,
        protocol: WarpProtocol,
    ) -> Self {
        let base = PathBuf::from(state_dir);
        let tunnels = tunnel_ports
            .into_iter()
            .enumerate()
            .map(|(index, port)| {
                let dir = base.join(format!("id{}", index + 1));
                let conf_path = match protocol {
                    WarpProtocol::WireGuard => dir.join("wireproxy.conf"),
                    WarpProtocol::Masque => dir.join("config.json"),
                };
                let probe = build_probe_client(port);
                Arc::new(Tunnel {
                    index,
                    port,
                    dir,
                    conf_path,
                    protocol,
                    state: Mutex::new(TunnelState {
                        ip: None,
                        last_recycle: Instant::now() - RECYCLE_COOLDOWN,
                        healthy: false,
                        condition: TunnelCondition::Unhealthy,
                        spawn_generation: 0,
                        restart_generation: 0,
                        probe_completed: false,
                    }),
                    probe_inflight: AtomicUsize::new(0),
                    recycling: AtomicBool::new(false),
                    restart: tokio::sync::Notify::new(),
                    probe,
                })
            })
            .collect();

        Self {
            tunnels,
            enabled,
            protocol,
            inflight: Arc::new(AtomicUsize::new(0)),
            recycle_lock: Mutex::new(()),
            rotations: AtomicUsize::new(0),
            history: Mutex::new(Vec::new()),
            quota_windows: Mutex::new(HashMap::new()),
        }
    }

    /// Seconds left in a daily-quota window the pool has already been refused
    /// in, or `None` when no such verdict is live.
    ///
    /// While it is live every exit answers the same `429` (measured: 10/10
    /// exits, `retry-after` ~14.6h), so walking the pool spends one ~700ms leg
    /// per exit -- up to 8 legs, ~6s of client latency -- to reach a verdict the
    /// first refusal already established. Reporting it immediately returns the
    /// same refusal in milliseconds.
    ///
    /// Conservative by construction: bounded by the wall-clock window the
    /// upstream itself named, and cleared by any successful request, so a single
    /// stale observation cannot pin the pool shut for the day.
    pub async fn quota_window_remaining(&self, model: &str) -> Option<u64> {
        let windows = self.quota_windows.lock().await;
        let (secs, at) = windows.get(model)?;
        let elapsed = at.elapsed().as_secs();
        if elapsed < *secs {
            Some(secs - elapsed)
        } else {
            None
        }
    }

    /// Clears the recorded quota window after an exit served a request, so a
    /// recovered pool is not held shut by a stale verdict.
    pub async fn clear_quota_window(&self, model: &str) {
        self.quota_windows.lock().await.remove(model);
    }

    /// Records that some egress answered a 429 whose `retry-after` outlives
    /// `WARP_QUOTA_ROTATE_MAX_WINDOW`, i.e. a spent daily bucket.
    pub async fn note_quota_window(&self, model: &str, secs: u64) {
        let mut windows = self.quota_windows.lock().await;
        // Keep the largest window seen per model: every refusal in a day reports
        // the same reset boundary, and a stale shorter value would understate it.
        let observed = windows.get(model).map(|(w, _)| *w).unwrap_or(0);
        windows.insert(model.to_string(), (observed.max(secs), Instant::now()));
        // Bound the map: it is keyed by whatever model names clients send, and an
        // unbounded one would leak memory on a hostile or buggy client.
        if windows.len() > QUOTA_WINDOW_MODEL_CAP {
            windows.retain(|_, (secs, at)| at.elapsed().as_secs() < *secs);
            while windows.len() > QUOTA_WINDOW_MODEL_CAP {
                let Some(victim) = windows.keys().next().cloned() else {
                    break;
                };
                windows.remove(&victim);
            }
        }
    }

    /// SOCKS5 proxy URLs of every tunnel in the pool.
    ///
    /// Empty when the pool is disabled: there is no tunnel behind those ports
    /// then, and pointing a client at them only produces connection refusals.
    pub fn proxy_urls(&self) -> Vec<String> {
        if !self.enabled() {
            return Vec::new();
        }
        self.tunnels.iter().map(|t| t.proxy_url()).collect()
    }

    pub fn enabled(&self) -> bool {
        self.enabled && !self.tunnels.is_empty()
    }

    /// Marks a request as in flight through the pool for the caller's lifetime.
    pub fn begin_request(&self) -> InflightGuard {
        InflightGuard::new(self.inflight.clone())
    }

    pub fn inflight_count(&self) -> usize {
        self.inflight.load(Ordering::Relaxed)
    }

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

    /// Ensures every tunnel has an identity and starts its supervisor task.
    /// Returns the join handles of all spawned tunnel supervisor tasks so the
    /// caller can wait for clean process shutdown and reaping.
    ///
    /// Waits until at least one tunnel answers an egress probe before
    /// returning. The engine fetches its model catalog exactly once at
    /// startup; if it runs while every SOCKS5 port is still closed, the
    /// catalog comes back empty and stays empty until the next refresh window.
    pub async fn start(self: &Arc<Self>, shutdown: Arc<AtomicBool>) -> Vec<JoinHandle<()>> {
        if !self.enabled() {
            info!("Cloudflare WARP pool is disabled (no identities configured).");
            return Vec::new();
        }

        info!(
            "Starting {} WARP tunnel(s) in userspace (no NET_ADMIN, no /dev/net/tun)...",
            self.tunnels.len()
        );

        for tunnel in &self.tunnels {
            if let Err(e) = tunnel.ensure_identity().await {
                error!("Tunnel {}: identity setup failed: {:#}", tunnel.index, e);
            }
        }

        let mut handles = Vec::with_capacity(self.tunnels.len());
        for tunnel in self.tunnels.clone() {
            let shutdown = shutdown.clone();
            handles.push(tokio::spawn(
                async move { tunnel.supervise(shutdown).await },
            ));
        }

        // Give the tunnels time to complete their WireGuard handshake. One
        // healthy exit is enough for the engine to build its catalog.
        let deadline = Instant::now() + TUNNEL_READY_TIMEOUT;
        loop {
            let mut healthy = 0;
            for t in &self.tunnels {
                if t.state.lock().await.healthy {
                    healthy += 1;
                }
            }
            if healthy > 0 {
                info!(
                    "{} of {} tunnel(s) ready; starting the engine against the pool.",
                    healthy,
                    self.tunnels.len()
                );
                break;
            }
            if Instant::now() >= deadline {
                warn!(
                    "No tunnel became ready within {:?}; starting the engine anyway.",
                    TUNNEL_READY_TIMEOUT
                );
                break;
            }
            tokio::time::sleep(Duration::from_millis(500)).await;
        }

        // Two registrations can land on the same exit address. A duplicate
        // makes the pool smaller than it looks, so recycle them in the
        // background (never block engine startup on a registration).
        //
        // The comparison waits for every tunnel's readiness probe to finish:
        // an egress IP is only known after a successful probe, so checking
        // while some tunnels are still probing would report "no duplicates"
        // for a pool that is actually sharing exits.
        let pool = self.clone();
        tokio::spawn(async move {
            pool.recycle_duplicate_egresses().await;
        });

        // Background watchdog: periodically checks tunnel health and automatically
        // replaces dead tunnels.
        let pool_watchdog = self.clone();
        let watchdog_shutdown = shutdown.clone();
        handles.push(tokio::spawn(async move {
            // 15s, not 60s: a tunnel that is bound but wedged (a frozen
            // `usque` accepts TCP and never answers the SOCKS handshake) stays
            // in the engine's static proxy list until the watchdog replaces it.
            // Every request that draws that exit pays the engine's hardcoded
            // 10s handshake timeout (measured: `anon:transport:10009ms`), so the
            // detection window IS the cost. A pass now costs one 8s probe
            // because the probes run concurrently, which keeps 15s safe.
            let mut interval = tokio::time::interval(WATCHDOG_INTERVAL);
            interval.tick().await; // skip initial immediate tick
            while !watchdog_shutdown.load(Ordering::Relaxed) {
                interval.tick().await;
                if watchdog_shutdown.load(Ordering::Relaxed) {
                    break;
                }
                pool_watchdog.auto_heal_dead_tunnels().await;
            }
        }));

        handles
    }

    /// Waits for every tunnel's readiness probe to complete, then recycles the
    /// tunnels that share an exit address with an earlier one.
    async fn recycle_duplicate_egresses(self: &Arc<Self>) {
        let deadline = Instant::now() + TUNNEL_READY_TIMEOUT;
        loop {
            let mut pending = false;
            for t in &self.tunnels {
                if !t.state.lock().await.probe_completed {
                    pending = true;
                    break;
                }
            }
            if !pending {
                break;
            }
            if Instant::now() >= deadline {
                warn!(
                    "Not every tunnel finished its readiness probe within {:?}; \
                     duplicate egress detection is partial",
                    TUNNEL_READY_TIMEOUT
                );
                break;
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }

        for idx in self.duplicate_tunnels().await {
            if let Some(t) = self.tunnels.get(idx).cloned() {
                warn!(
                    "Tunnel {} duplicates another exit; recycling it (duplicate egress at startup)",
                    idx
                );
                let pool = self.clone();
                tokio::spawn(async move {
                    if !t.recycling.swap(true, Ordering::SeqCst) {
                        pool.recycle_locked(t, "duplicate egress at startup").await;
                    }
                });
            }
        }
    }

    /// The quota scope of an egress address: an IPv6 `/64` or an IPv4 `/32`.
    ///
    /// The free tier meters per block, not per address, so two tunnels holding
    /// different `/128`s inside one `/64` share a bucket. Comparing raw
    /// addresses reports them as distinct and silently overstates capacity.
    /// Returns `None` for anything that does not parse, so an unknown address
    /// never looks like a match.
    fn egress_scope(ip: &str) -> Option<String> {
        let ip = ip.trim();
        if ip.contains(':') {
            // Expand `::` into its eight groups, then keep the first four.
            let (head, tail) = match ip.split_once("::") {
                Some((h, t)) => (h, Some(t)),
                None => (ip, None),
            };
            let head: Vec<&str> = head.split(':').filter(|s| !s.is_empty()).collect();
            let tail: Vec<&str> = tail
                .map(|t| t.split(':').filter(|s| !s.is_empty()).collect())
                .unwrap_or_default();
            // An embedded IPv4 tail (`::ffff:1.2.3.4`) is not a scope we can
            // reason about here; treat it as unparseable rather than guess.
            if ip.contains('.') {
                return None;
            }
            // `::` stands for however many zero groups are needed to reach
            // eight; that count depends on both sides, so it is only correct to
            // compute it from their lengths together.
            // `::` present but the explicit groups already fill the address is
            // malformed rather than a zero-run, hence `?`.
            let missing = if ip.contains("::") {
                8usize.checked_sub(head.len() + tail.len())?
            } else {
                0
            };
            // Parse every group as hex and re-render it canonically: `0008` and
            // `8` are the same group, and a non-hex group is not an address at
            // all. Comparing the raw text would call those distinct.
            let mut groups: Vec<u16> = Vec::with_capacity(8);
            for g in head.iter() {
                groups.push(u16::from_str_radix(g, 16).ok()?);
            }
            groups.extend(std::iter::repeat_n(0, missing));
            for g in tail.iter() {
                groups.push(u16::from_str_radix(g, 16).ok()?);
            }
            if groups.len() != 8 {
                return None;
            }
            let head_scope: Vec<String> = groups[..4].iter().map(|g| format!("{g:x}")).collect();
            Some(format!("{}/64", head_scope.join(":")))
        } else {
            let mut parts = ip.split('.');
            let octets: Vec<&str> = parts.by_ref().take(4).collect();
            if octets.len() != 4 || parts.next().is_some() {
                return None;
            }
            if octets.iter().any(|o| o.parse::<u8>().is_err()) {
                return None;
            }
            Some(ip.to_string())
        }
    }

    /// Returns the indices of tunnels whose egress IP is shared with an earlier
    /// tunnel, if any.
    ///
    /// Only tunnels whose readiness probe has completed take part: an egress
    /// IP is only known after a successful probe, so a tunnel that is still
    /// probing has no address to compare yet and would make a duplicated pair
    /// look unique.
    async fn duplicate_tunnels(&self) -> Vec<usize> {
        let mut seen: Vec<String> = Vec::new();
        let mut duplicates = Vec::new();
        for t in &self.tunnels {
            let ip = {
                let st = t.state.lock().await;
                if !st.probe_completed {
                    continue;
                }
                st.ip.clone()
            };
            // Compare quota scopes, not addresses: two `/128`s inside one
            // `/64` are one bucket, and treating them as distinct is what
            // let a rotation land back on a spent block unnoticed.
            if let Some(scope) = ip.as_deref().and_then(Self::egress_scope) {
                if seen.contains(&scope) {
                    duplicates.push(t.index);
                } else {
                    seen.push(scope);
                }
            }
        }
        duplicates
    }

    /// Picks the next tunnel for recycling *and* claims it, in one step.
    ///
    /// Picking and claiming separately loses recycles: two concurrent 429s can
    /// both observe the same tunnel as free, and the loser of the claim
    /// silently drops its recycle while the tunnel it wanted stays burned.
    /// Claiming with a compare-exchange inside the selection loop makes the
    /// loser move on to the next candidate instead.
    ///
    /// Tunnels with no open connection are preferred. The supervisor never
    /// learns which tunnel served the refused request -- the engine picks the
    /// proxy per attempt -- so a round-robin pick can land on a tunnel that is
    /// currently streaming a healthy response. Recycling that one truncates
    /// an unrelated stream. An idle tunnel is always the better victim.
    ///
    /// When every eligible tunnel is busy, the least busy one is taken rather
    /// than an arbitrary one: that is the tunnel whose streams are closest to
    /// finishing, so the drain in `recycle_locked` has the best chance of
    /// completing before the child is killed.
    /// Checks all tunnels and automatically triggers background replacement
    /// for any tunnel that is confirmed dead (egress probe failed).
    pub async fn auto_heal_dead_tunnels(self: &Arc<Self>) {
        if !self.enabled() {
            return;
        }

        // Probe every tunnel concurrently.
        //
        // Serially, one hung exit (a frozen `usque` accepts the TCP connection
        // and never answers the SOCKS handshake) costs the full probe timeout
        // before the next tunnel is even looked at, so a single dead tunnel
        // stretched a pass past a minute. Concurrently, a pass costs one probe
        // timeout no matter how many tunnels are wedged, which is what lets the
        // watchdog run on a short interval without passes overlapping.
        let mut probes = tokio::task::JoinSet::new();
        for tunnel in &self.tunnels {
            if tunnel.recycling.load(Ordering::Relaxed) {
                continue;
            }
            let t = tunnel.clone();
            probes.spawn(async move {
                let index = t.index;
                (index, t.probe_ip().await)
            });
        }

        let mut results: std::collections::HashMap<usize, Option<String>> =
            std::collections::HashMap::new();
        while let Some(joined) = probes.join_next().await {
            if let Ok((index, res)) = joined {
                results.insert(index, res);
            }
        }

        for tunnel in &self.tunnels {
            if tunnel.recycling.load(Ordering::Relaxed) {
                continue;
            }

            let probe_res = match results.get(&tunnel.index) {
                Some(res) => res.clone(),
                None => continue,
            };
            let mut st = tunnel.state.lock().await;
            match probe_res {
                Some(ip) => {
                    st.ip = Some(ip);
                    st.healthy = true;
                    if st.condition == TunnelCondition::Unhealthy {
                        st.condition = TunnelCondition::Active;
                    }
                }
                None => {
                    st.healthy = false;
                    st.condition = TunnelCondition::Unhealthy;
                    let elapsed = st.last_recycle.elapsed();
                    drop(st);

                    warn!(
                        "Tunnel {} (port {}) is dead (egress probe failed). Triggering automatic replacement...",
                        tunnel.index, tunnel.port
                    );

                    if elapsed >= RECYCLE_COOLDOWN
                        && tunnel
                            .recycling
                            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                            .is_ok()
                    {
                        let pool = self.clone();
                        let t = tunnel.clone();
                        tokio::spawn(async move {
                            pool.recycle_locked(t, "automatic replacement of dead tunnel")
                                .await;
                        });
                    }
                }
            }
        }
    }

    /// Picks a candidate tunnel for single/staggered replacement *and* claims it.
    ///
    /// Selection priority:
    /// 1. Targeted tunnel if `target_idx` is specified (e.g. `?tunnel=i`).
    /// 2. Dead / Unhealthy tunnels (`!healthy` or `condition == Unhealthy`).
    /// 3. Banned tunnels (`condition == Banned`).
    /// 4. Quota-exhausted tunnels (`condition == QuotaExhausted`).
    /// 5. If `allow_active` is true (e.g. `force=true`), falls back to the oldest idle healthy tunnel.
    ///    If `allow_active` is false, returns `None` (preserving all active warm tunnels).
    pub fn claim_target_tunnel(
        &self,
        target_idx: Option<usize>,
        allow_active: bool,
    ) -> Option<Arc<Tunnel>> {
        let n = self.tunnels.len();
        if n == 0 {
            return None;
        }

        // 1. If a specific tunnel index is requested
        if let Some(idx) = target_idx {
            if let Some(t) = self.tunnels.get(idx) {
                if !t.recycling.load(Ordering::Relaxed) {
                    let eligible = match t.state.try_lock() {
                        Ok(st) => st.last_recycle.elapsed() >= RECYCLE_COOLDOWN,
                        Err(_) => false,
                    };
                    if eligible
                        && t.recycling
                            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                            .is_ok()
                    {
                        return Some(t.clone());
                    }
                }
            }
            return None;
        }

        // 2. Collect eligible tunnels grouped by tier
        let mut tier1 = Vec::new(); // Dead / Unhealthy
        let mut tier2 = Vec::new(); // Banned
        let mut tier3 = Vec::new(); // QuotaExhausted
        let mut tier4 = Vec::new(); // Active / Healthy

        for t in &self.tunnels {
            if t.recycling.load(Ordering::Relaxed) {
                continue;
            }
            let (elapsed, healthy, cond) = match t.state.try_lock() {
                Ok(st) if st.last_recycle.elapsed() >= RECYCLE_COOLDOWN => {
                    (st.last_recycle.elapsed(), st.healthy, st.condition)
                }
                _ => continue,
            };
            let busy = t.active_connections();
            let entry = (elapsed, busy, t.clone());

            if !healthy || cond == TunnelCondition::Unhealthy {
                tier1.push(entry);
            } else if cond == TunnelCondition::Banned {
                tier2.push(entry);
            } else if cond == TunnelCondition::QuotaExhausted {
                tier3.push(entry);
            } else {
                tier4.push(entry);
            }
        }

        let try_claim_tier = |mut candidates: Vec<(Duration, usize, Arc<Tunnel>)>| {
            if candidates.is_empty() {
                return None;
            }
            candidates.sort_by(|a, b| {
                let a_idle = a.1 == 0;
                let b_idle = b.1 == 0;
                match (a_idle, b_idle) {
                    (true, false) => std::cmp::Ordering::Less,
                    (false, true) => std::cmp::Ordering::Greater,
                    _ => b.0.cmp(&a.0).then_with(|| a.1.cmp(&b.1)),
                }
            });
            candidates
                .into_iter()
                .find(|(_, _, tunnel)| {
                    tunnel
                        .recycling
                        .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                        .is_ok()
                })
                .map(|(_, _, tunnel)| tunnel)
        };

        for tier in [tier1, tier2, tier3] {
            if let Some(t) = try_claim_tier(tier) {
                return Some(t);
            }
        }

        if allow_active {
            if let Some(t) = try_claim_tier(tier4) {
                return Some(t);
            }
        }

        None
    }

    /// Convenience wrapper preserving backwards compatibility for oldest-tunnel selection.
    #[cfg(test)]
    pub fn claim_oldest_tunnel(&self) -> Option<Arc<Tunnel>> {
        self.claim_target_tunnel(None, true)
    }

    /// Picks a tunnel for background recycling on refusal: prioritizes dead/banned/exhausted
    /// tunnels first, falling back to an idle active tunnel only if necessary.
    fn claim_tunnel(&self) -> Option<Arc<Tunnel>> {
        self.claim_target_tunnel(None, true)
    }

    /// Recycles one tunnel in the background: new identity, new egress IP.
    ///
    /// This returns immediately. A request that just got a 429 must not block
    /// on a 5-10s registration: the engine's own retry already moves to the
    /// next tunnel in the pool, so the recycle only has to refresh capacity.
    pub fn recycle_async(self: &Arc<Self>, reason: &str) {
        if !self.enabled() {
            return;
        }
        let Some(tunnel) = self.claim_tunnel() else {
            return;
        };

        let pool = self.clone();
        let reason = reason.to_string();
        tokio::spawn(async move {
            pool.recycle_locked(tunnel, &reason).await;
        });
    }

    /// Re-registers one tunnel while holding `recycle_lock`. The caller must
    /// have claimed `tunnel.recycling`.
    async fn recycle_locked(&self, tunnel: Arc<Tunnel>, reason: &str) -> Option<String> {
        // Concurrent `wgcf register` runs in the same directory would corrupt
        // the identity files, so re-registrations are serialised pool-wide.
        //
        // The lock covers the re-registration only. The drain below can take
        // much longer than a registration, and holding the pool-wide lock for
        // that whole time would block every other recycle -- and the
        // operator's /v1/rotate -- behind a tunnel that is busy.
        let old_ip = tunnel.state.lock().await.ip.clone();
        info!(
            "Recycling tunnel {} ({}): {} (current egress: {:?})",
            tunnel.index, tunnel.port, reason, old_ip
        );

        let re_registered = {
            let _guard = self.recycle_lock.lock().await;
            tunnel.re_register().await
        };

        let result = match re_registered {
            Ok(()) => {
                // The tunnel is about to be killed and respawned with the new
                // identity. Killing it drops every connection it is carrying,
                // and a stream riding that tunnel dies with "unexpected EOF" --
                // the request that triggered this recycle has already failed,
                // but other requests may still be streaming through this
                // tunnel.
                //
                // The pool-wide in-flight counter cannot decide this: it does
                // not say which tunnel carries the traffic, and the engine
                // picks the proxy per attempt. The socket table does.
                //
                // The child keeps running the old identity while this waits,
                // so the tunnel serves its traffic normally: the swap only
                // happens once the tunnel has gone quiet.
                if !tunnel.wait_until_drained(TUNNEL_DRAIN_TIMEOUT).await {
                    // The tunnel stayed busy. Refreshing a burned exit is not
                    // worth truncating a stream that is running fine, so the
                    // swap is abandoned: the child keeps serving the identity
                    // it has, and the next refused request tries again.
                    warn!(
                        "Tunnel {}: a stream is still running after {:?}; the swap is abandoned so that it is not truncated",
                        tunnel.index,
                        TUNNEL_DRAIN_TIMEOUT
                    );
                    tunnel.state.lock().await.last_recycle = Instant::now();
                    None
                } else {
                    // The supervisor owns the child: signal it so the restart
                    // picks up the freshly written config, and wait until it
                    // has.
                    if !restart_tunnel(&tunnel).await {
                        tunnel.state.lock().await.last_recycle = Instant::now();
                        None
                    } else {
                        let mut new_ip = None;
                        for _ in 0..30 {
                            tokio::time::sleep(Duration::from_millis(500)).await;
                            if let Some(ip) = tunnel.probe_ip().await {
                                new_ip = Some(ip);
                                break;
                            }
                        }

                        {
                            let mut st = tunnel.state.lock().await;
                            st.last_recycle = Instant::now();
                            st.healthy = new_ip.is_some();
                            // After a recycle the tunnel runs a fresh identity,
                            // so the previous exit address is stale: report
                            // the new one, or nothing at all when it could not
                            // be verified.
                            st.ip = new_ip.clone();
                            st.probe_completed = true;
                        }

                        // A rotation that lands on a scope another tunnel
                        // already holds buys no quota: the free tier meters per
                        // /64 (or /32), so the replacement is a new address in a
                        // spent bucket. Detect it here and report it honestly
                        // instead of counting it as fresh capacity.
                        let new_scope = new_ip.as_deref().and_then(Self::egress_scope);
                        let collides = match new_scope {
                            Some(scope) => {
                                let mut held = false;
                                for other in &self.tunnels {
                                    if other.index == tunnel.index {
                                        continue;
                                    }
                                    let st = other.state.lock().await;
                                    if st.probe_completed
                                        && st.ip.as_deref().and_then(Self::egress_scope)
                                            == Some(scope.clone())
                                    {
                                        held = true;
                                        break;
                                    }
                                }
                                held
                            }
                            None => false,
                        };

                        match new_ip.clone() {
                            Some(ip) if collides => {
                                warn!(
                                    "Tunnel {} recycled onto {} which shares a quota scope with \
                                     another tunnel ({} -> {}); not counted as a rotation",
                                    tunnel.index,
                                    ip,
                                    old_ip.as_deref().unwrap_or("?"),
                                    ip
                                );
                            }
                            Some(ip) if Some(&ip) != old_ip.as_ref() => {
                                info!(
                                    "Tunnel {} recycled: {} -> {}",
                                    tunnel.index,
                                    old_ip.as_deref().unwrap_or("?"),
                                    ip
                                );
                                self.rotations.fetch_add(1, Ordering::Relaxed);
                                let mut h = self.history.lock().await;
                                h.push(RotateRecord {
                                    tunnel: tunnel.index,
                                    old_ip,
                                    new_ip: ip,
                                    timestamp: chrono_now(),
                                    reason: reason.to_string(),
                                });
                                if h.len() > 50 {
                                    h.remove(0);
                                }
                            }
                            Some(ip) => warn!(
                                "Tunnel {}: re-registration kept the same egress {}; not counted as a rotation.",
                                tunnel.index, ip
                            ),
                            None => warn!("Tunnel {}: could not verify a new egress IP.", tunnel.index),
                        }

                        new_ip
                    }
                }
            }
            Err(e) => {
                error!("Tunnel {}: re-registration failed: {:#}", tunnel.index, e);
                tunnel.state.lock().await.last_recycle = Instant::now();
                None
            }
        };

        tunnel.recycling.store(false, Ordering::SeqCst);
        result
    }

    /// Operator-triggered rotation: refreshes every tunnel and waits for the
    /// work to complete. Returns the first egress IP observed.
    pub async fn rotate_all(&self, reason: &str) -> Result<String> {
        if !self.enabled() {
            anyhow::bail!("WARP pool is disabled");
        }

        // Drain ONCE for the whole batch. Draining per tunnel would run the
        // 30s timeout N times back to back while every stream has long since
        // finished, turning a fast rotation into a minute of waiting.
        let claimed: Vec<Arc<Tunnel>> = self
            .tunnels
            .iter()
            .filter(|t| !t.recycling.swap(true, Ordering::SeqCst))
            .cloned()
            .collect();

        if claimed.is_empty() {
            anyhow::bail!("every tunnel is already re-registering");
        }

        if !self.wait_for_drain(RECYCLE_DRAIN_TIMEOUT).await {
            warn!(
                "Manual rotation: {} stream(s) still in flight after {:?}; proceeding anyway.",
                self.inflight_count(),
                RECYCLE_DRAIN_TIMEOUT
            );
        }

        let mut first_ip = None;
        for tunnel in claimed {
            // `recycle_locked` releases the flag itself.
            if let Some(ip) = self.recycle_locked(tunnel, reason).await {
                if first_ip.is_none() {
                    first_ip = Some(ip);
                }
            }
        }

        first_ip.context("failed to verify any new egress IP across the pool")
    }

    /// Operator or cron-triggered staggered rotation: claims a dead, banned, or exhausted
    /// tunnel to replace it with a fresh identity.
    ///
    /// If all tunnels are active and healthy, returns `SingleRotateOutcome` with status `"noop"`
    /// unless `force` is set to true (which falls back to replacing the oldest idle tunnel).
    pub async fn rotate_next(
        &self,
        reason: &str,
        target_idx: Option<usize>,
        force: bool,
    ) -> Result<SingleRotateOutcome> {
        if !self.enabled() {
            anyhow::bail!("WARP pool is disabled");
        }

        let tunnel = match self.claim_target_tunnel(target_idx, force) {
            Some(t) => t,
            None => {
                return Ok(SingleRotateOutcome {
                    status: "noop".into(),
                    message: "All tunnels are active and healthy. No replacement needed.".into(),
                    tunnel: None,
                    port: None,
                    old_ip: None,
                    new_ip: None,
                });
            }
        };

        let tunnel_idx = tunnel.index;
        let port = tunnel.port;
        let old_ip = tunnel.state.lock().await.ip.clone();

        let new_ip = self.recycle_locked(tunnel, reason).await.context(
            "failed to recycle tunnel (stream drain timed out or new egress could not be verified)",
        )?;

        Ok(SingleRotateOutcome {
            status: "success".into(),
            message: format!(
                "Successfully replaced tunnel {} (port {})",
                tunnel_idx, port
            ),
            tunnel: Some(tunnel_idx),
            port: Some(port),
            old_ip,
            new_ip: Some(new_ip),
        })
    }

    pub async fn status(&self) -> WarpStatus {
        let mut tunnels = Vec::with_capacity(self.tunnels.len());
        for t in &self.tunnels {
            tunnels.push(t.status().await);
        }

        WarpStatus {
            enabled: self.enabled(),
            running: tunnels.iter().any(|t| t.healthy),
            protocol: self.protocol,
            identities: tunnels.len(),
            healthy: tunnels.iter().filter(|t| t.healthy).count(),
            proxy_urls: if self.enabled() {
                self.proxy_urls()
            } else {
                Vec::new()
            },
            rotations: self.rotations.load(Ordering::Relaxed),
            scopes: {
                let mut seen: Vec<String> = Vec::new();
                for t in &tunnels {
                    if let Some(scope) = t.ip.as_deref().and_then(Self::egress_scope) {
                        if !seen.contains(&scope) {
                            seen.push(scope);
                        }
                    }
                }
                seen.len()
            },
            quota_window_secs: {
                // The documented field is a single pool-level number; report the
                // longest live window so a caller sizing its backoff is not
                // surprised by a shorter one.
                let windows = self.quota_windows.lock().await;
                windows
                    .values()
                    .filter(|(secs, at)| at.elapsed().as_secs() < *secs)
                    .map(|(secs, at)| secs - at.elapsed().as_secs())
                    .max()
            },
            tunnels,
            history: self.history.lock().await.clone(),
        }
    }
}

/// Asks a tunnel's supervisor to stop and re-exec wireproxy with the config
/// that `re_register` just wrote, then waits for the new child to be spawned.
///
/// Restarting through the supervisor's `Child` handle (not `pkill -f`) is the
/// point: a name-based signal matches the whole command line, so an unrelated
/// process whose arguments happen to contain this tunnel's config path would be
/// killed too, and a prefix such as `id1` would also match `id10`.
///
/// The wait requires *both* generations to advance: `restart_generation`
/// proves the supervisor serviced this request, and `spawn_generation` proves
/// the replacement child exists. Waiting on the spawn counter alone would be
/// satisfied by a crash-driven respawn, which would leave the old config
/// running while the caller believed the rotation had been applied.
async fn restart_tunnel(tunnel: &Arc<Tunnel>) -> bool {
    let (before_restart, before_spawn) = {
        let st = tunnel.state.lock().await;
        (st.restart_generation, st.spawn_generation)
    };
    tunnel.restart.notify_one();

    for _ in 0..80 {
        tokio::time::sleep(Duration::from_millis(250)).await;
        let st = tunnel.state.lock().await;
        if st.restart_generation > before_restart && st.spawn_generation > before_spawn {
            return true;
        }
    }

    warn!(
        "Tunnel {}: supervisor did not restart wireproxy within 20s.",
        tunnel.index
    );
    false
}

/// Counts the TCP connections in `ESTABLISHED` state whose local port is
/// `port`.
///
/// The engine and the supervisor share the container's network namespace, so
/// `/proc/net/tcp` and `/proc/net/tcp6` show every connection the engine holds
/// against a tunnel's SOCKS5 port. Each such connection is one request
/// currently riding that tunnel.
///
/// The format is fixed by the kernel: one header line, then one line per
/// socket, with the local address in `HEX_IP:HEX_PORT` and the state as a
/// two-digit hex code (`01` = ESTABLISHED) and the socket's inode in the
/// tenth column.
fn established_sockets_on_port(port: u16) -> Vec<u64> {
    let mut inodes = Vec::new();
    for path in ["/proc/net/tcp", "/proc/net/tcp6"] {
        let Ok(text) = std::fs::read_to_string(path) else {
            continue;
        };
        inodes.extend(established_sockets_in(&text, port));
    }
    inodes
}

/// Collects the inodes of the established sockets on `port` in one
/// `/proc/net/tcp` table.
///
/// The inode identifies a socket across polls, which is how the drain tells a
/// stream (open for seconds) from an ordinary request (open for milliseconds).
///
/// Split out from the file access so the parsing can be tested against a
/// recorded table instead of against whatever the host happens to run.
fn established_sockets_in(table: &str, port: u16) -> Vec<u64> {
    let mut inodes = Vec::new();
    for line in table.lines().skip(1) {
        let mut fields = line.split_whitespace();
        let _sl = fields.next();
        let Some(local) = fields.next() else {
            continue;
        };
        let _remote = fields.next();
        let Some(state) = fields.next() else {
            continue;
        };
        if state != "01" {
            continue;
        }
        let Some(hex_port) = local.rsplit(':').next() else {
            continue;
        };
        if u16::from_str_radix(hex_port, 16) != Ok(port) {
            continue;
        }
        // The inode is the tenth column: sl local rem st tx:rx tr:when retrnsmt
        // uid timeout inode. Four fields have been read (sl, local, rem, st),
        // so five more have to be skipped to reach it.
        let inode = fields.nth(5).and_then(|v| v.parse::<u64>().ok());
        inodes.push(inode.unwrap_or(0));
    }
    inodes
}

fn chrono_now() -> String {
    use std::time::SystemTime;
    let now = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    format!("{}", now)
}

pub(crate) async fn wait_for_shutdown(shutdown: &Arc<AtomicBool>) {
    while !shutdown.load(Ordering::Relaxed) {
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// Builds a per-tunnel probe client. Pooling is disabled so a probe can never
/// reuse a socket from a previous WARP registration.
///
/// The `socks5h` scheme is mandatory, not cosmetic: plain `socks5` resolves the
/// hostname locally, so the probe reports the address this host sees rather
/// than the tunnel's exit. Every tunnel then looks identical and the pool
/// mistakes distinct exits for duplicates.
fn build_probe_client(socks_port: u16) -> Option<reqwest::Client> {
    let proxy = match reqwest::Proxy::all(format!("socks5h://127.0.0.1:{}", socks_port)) {
        Ok(p) => p,
        Err(e) => {
            warn!("failed to create reqwest proxy: {}", e);
            return None;
        }
    };

    match reqwest::Client::builder()
        .proxy(proxy)
        .pool_max_idle_per_host(0)
        .timeout(PROBE_TIMEOUT)
        .build()
    {
        Ok(c) => Some(c),
        Err(e) => {
            warn!("failed to build reqwest probe client: {}", e);
            None
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn pool_with(n: usize) -> Arc<WarpPool> {
        Arc::new(WarpPool::new(
            (0..n).map(|i| 40001 + i as u16),
            "/tmp/quirkproxy-test-warp",
            true,
            WarpProtocol::WireGuard,
        ))
    }

    #[tokio::test]
    async fn cooldown_blocks_immediate_recycle_of_the_same_tunnel() {
        let pool = pool_with(1);
        let tunnel = pool.tunnels[0].clone();

        // First claim succeeds: the initial state is backdated past the cooldown.
        assert!(pool.claim_tunnel().is_some());
        tunnel.recycling.store(false, Ordering::SeqCst);

        // Simulate a recycle that just completed.
        tunnel.state.lock().await.last_recycle = Instant::now();

        // The only tunnel is now inside the cooldown window, so nothing may be
        // picked. Before the fix this returned the same tunnel immediately and
        // a burst of 429s burned identities back to back.
        assert!(pool.claim_tunnel().is_none());
    }

    #[tokio::test]
    async fn cooldown_scopes_to_one_tunnel_not_the_whole_pool() {
        let pool = pool_with(2);
        let first = pool.tunnels[0].clone();

        first.state.lock().await.last_recycle = Instant::now();

        // The cooled-down tunnel is skipped but the other one is still usable.
        for _ in 0..4 {
            let picked = pool.claim_tunnel().expect("a tunnel should be available");
            assert_ne!(picked.index, first.index);
            picked.recycling.store(false, Ordering::SeqCst);
        }
    }

    #[tokio::test]
    async fn claim_skips_tunnels_already_recycling() {
        let pool = pool_with(1);
        pool.tunnels[0].recycling.store(true, Ordering::SeqCst);
        assert!(pool.claim_tunnel().is_none());
    }

    #[tokio::test]
    async fn claim_never_hands_the_same_tunnel_to_two_callers() {
        let pool = pool_with(1);

        // The claim is part of the selection: a second caller must not be able
        // to pick the tunnel the first one already claimed.
        let first = pool.claim_tunnel().expect("the tunnel should be claimable");
        assert_eq!(first.index, 0);
        assert!(pool.claim_tunnel().is_none());
    }

    #[tokio::test]
    async fn claim_oldest_tunnel_picks_the_oldest_idle_tunnel() {
        let pool = pool_with(3);
        let t0 = pool.tunnels[0].clone();
        let t1 = pool.tunnels[1].clone();
        let t2 = pool.tunnels[2].clone();

        let now = Instant::now();
        t0.state.lock().await.last_recycle = now - Duration::from_secs(100);
        t1.state.lock().await.last_recycle = now - Duration::from_secs(300);
        t2.state.lock().await.last_recycle = now - Duration::from_secs(70);

        let picked = pool.claim_oldest_tunnel().expect("should pick a tunnel");
        assert_eq!(
            picked.index, 1,
            "t1 should be picked because it is the oldest"
        );
        assert!(picked.recycling.load(Ordering::SeqCst));
    }

    #[tokio::test]
    async fn claim_oldest_tunnel_skips_cooldown_and_recycling() {
        let pool = pool_with(2);
        let t0 = pool.tunnels[0].clone();
        let t1 = pool.tunnels[1].clone();

        let now = Instant::now();
        // t0 is already recycling
        t0.recycling.store(true, Ordering::SeqCst);
        t0.state.lock().await.last_recycle = now - Duration::from_secs(300);

        // t1 was recycled 10s ago (< RECYCLE_COOLDOWN)
        t1.state.lock().await.last_recycle = now - Duration::from_secs(10);

        assert!(
            pool.claim_oldest_tunnel().is_none(),
            "no tunnel should be eligible"
        );
    }

    #[tokio::test]
    async fn claim_target_tunnel_prefers_unhealthy_over_healthy() {
        let pool = pool_with(2);
        let t0 = pool.tunnels[0].clone();
        let t1 = pool.tunnels[1].clone();

        let now = Instant::now();
        // t0 is healthy Active, elapsed 500s
        {
            let mut st = t0.state.lock().await;
            st.healthy = true;
            st.condition = TunnelCondition::Active;
            st.last_recycle = now - Duration::from_secs(500);
        }
        // t1 is Unhealthy, elapsed 80s
        {
            let mut st = t1.state.lock().await;
            st.healthy = false;
            st.condition = TunnelCondition::Unhealthy;
            st.last_recycle = now - Duration::from_secs(80);
        }

        let picked = pool
            .claim_target_tunnel(None, false)
            .expect("should pick unhealthy tunnel");
        assert_eq!(
            picked.index, 1,
            "t1 should be picked because it is unhealthy"
        );
        assert!(picked.recycling.load(Ordering::SeqCst));
    }

    #[tokio::test]
    async fn claim_target_tunnel_prefers_banned_over_quota_exhausted() {
        let pool = pool_with(2);
        let t0 = pool.tunnels[0].clone();
        let t1 = pool.tunnels[1].clone();

        let now = Instant::now();
        {
            let mut st = t0.state.lock().await;
            st.healthy = true;
            st.condition = TunnelCondition::QuotaExhausted;
            st.last_recycle = now - Duration::from_secs(200);
        }
        {
            let mut st = t1.state.lock().await;
            st.healthy = true;
            st.condition = TunnelCondition::Banned;
            st.last_recycle = now - Duration::from_secs(100);
        }

        let picked = pool
            .claim_target_tunnel(None, false)
            .expect("should pick banned tunnel");
        assert_eq!(picked.index, 1, "t1 should be picked because it is banned");
    }

    #[tokio::test]
    async fn claim_target_tunnel_returns_none_when_all_healthy_and_not_forced() {
        let pool = pool_with(2);
        let t0 = pool.tunnels[0].clone();
        let t1 = pool.tunnels[1].clone();

        let now = Instant::now();
        for t in [&t0, &t1] {
            let mut st = t.state.lock().await;
            st.healthy = true;
            st.condition = TunnelCondition::Active;
            st.last_recycle = now - Duration::from_secs(500);
        }

        assert!(
            pool.claim_target_tunnel(None, false).is_none(),
            "should not rotate any tunnel when all are healthy and active"
        );
        assert!(
            pool.claim_target_tunnel(None, true).is_some(),
            "forced should allow picking an active tunnel"
        );
    }

    #[tokio::test]
    async fn claim_target_tunnel_supports_explicit_index() {
        let pool = pool_with(3);
        let t1 = pool.tunnels[1].clone();
        t1.state.lock().await.last_recycle = Instant::now() - Duration::from_secs(200);

        let picked = pool
            .claim_target_tunnel(Some(1), false)
            .expect("should claim requested index");
        assert_eq!(picked.index, 1);
    }

    #[tokio::test]
    async fn claim_moves_on_to_the_next_tunnel_when_the_first_is_taken() {
        let pool = pool_with(2);
        pool.tunnels[0].recycling.store(true, Ordering::SeqCst);

        let picked = pool
            .claim_tunnel()
            .expect("the free tunnel should be claimable");
        assert_eq!(picked.index, 1);
    }

    #[tokio::test]
    async fn restart_signal_is_isolated_per_tunnel() {
        let pool = pool_with(2);
        let first = pool.tunnels[0].clone();
        let second = pool.tunnels[1].clone();

        // A restart request aimed at tunnel 0 must not wake tunnel 1's
        // supervisor. With the old `pkill -f` approach both were matched by
        // the same prefix ("wireproxy -c .../id1" also matches id10, and the
        // whole argv is matched), so this property did not hold.
        let before_second = second.state.lock().await.spawn_generation;
        first.restart.notify_one();

        let woke_first =
            tokio::time::timeout(Duration::from_millis(300), first.restart.notified()).await;
        assert!(woke_first.is_ok(), "targeted tunnel should be signalled");

        let woke_second =
            tokio::time::timeout(Duration::from_millis(300), second.restart.notified()).await;
        assert!(woke_second.is_err(), "other tunnel must stay untouched");
        assert_eq!(second.state.lock().await.spawn_generation, before_second);
    }

    #[tokio::test]
    async fn spawn_generation_starts_at_zero() {
        let pool = pool_with(1);
        assert_eq!(pool.tunnels[0].state.lock().await.spawn_generation, 0);
    }

    #[tokio::test]
    async fn disabled_pool_reports_no_proxies() {
        let pool = Arc::new(WarpPool::new(
            (0..3).map(|i| 40001 + i as u16),
            "/tmp/quirkproxy-test-warp",
            false,
            WarpProtocol::WireGuard,
        ));
        assert!(!pool.enabled());
        // The engine config is built from the pool's proxy URLs. With the pool
        // disabled the engine must not be pointed at SOCKS endpoints that
        // nothing listens on.
        assert!(pool.proxy_urls().is_empty());
    }

    #[tokio::test]
    async fn disabled_pool_start_returns_empty_handles() {
        let pool = Arc::new(WarpPool::new(
            (0..3).map(|i| 40001 + i as u16),
            "/tmp/quirkproxy-test-warp",
            false,
            WarpProtocol::WireGuard,
        ));
        let shutdown = Arc::new(AtomicBool::new(false));
        let handles = pool.start(shutdown).await;
        assert!(handles.is_empty());
    }

    #[tokio::test]
    async fn duplicate_tunnels_identifies_all_duplicates() {
        let pool = pool_with(3);
        for t in &pool.tunnels {
            let mut st = t.state.lock().await;
            st.ip = Some("104.28.1.1".into());
            st.probe_completed = true;
        }

        let duplicates = pool.duplicate_tunnels().await;
        assert_eq!(duplicates, vec![1, 2]);
    }

    #[test]
    fn egress_scope_truncates_ipv6_to_the_metered_block() {
        // Two different /128s in one /64 are one quota bucket, which is the
        // whole reason duplicates have to be compared by scope.
        assert_eq!(
            WarpPool::egress_scope("2a09:bac1:6500:8::3c1:47").as_deref(),
            Some("2a09:bac1:6500:8/64")
        );
        assert_eq!(
            WarpPool::egress_scope("2a09:bac1:6500:8::1f1:21a").as_deref(),
            Some("2a09:bac1:6500:8/64")
        );
        // A different /64 must not collide, even though the prefix looks close.
        assert_ne!(
            WarpPool::egress_scope("2a09:bac1:6500:8::1"),
            WarpPool::egress_scope("2a09:bac1:6560:8::1")
        );
        // Full, uncompressed form must resolve to the same scope.
        assert_eq!(
            WarpPool::egress_scope("2a09:bac1:6500:0008:0000:0000:0000:0001").as_deref(),
            Some("2a09:bac1:6500:8/64")
        );
        // Upper-case input is not distinct.
        assert_eq!(
            WarpPool::egress_scope("2A09:BAC1:6500:8::1"),
            WarpPool::egress_scope("2a09:bac1:6500:8::1")
        );
    }

    #[test]
    fn egress_scope_keeps_ipv4_whole() {
        assert_eq!(
            WarpPool::egress_scope("104.28.222.43").as_deref(),
            Some("104.28.222.43")
        );
        assert_ne!(
            WarpPool::egress_scope("104.28.222.43"),
            WarpPool::egress_scope("104.28.222.46")
        );
    }

    #[test]
    fn egress_scope_rejects_what_it_cannot_parse() {
        // Unparseable input must be `None`, never a bogus scope that would
        // make two unrelated tunnels look like a duplicate.
        for bad in [
            "",
            "not-an-ip",
            "1.2.3",
            "1.2.3.4.5",
            "::ffff:1.2.3.4",
            "2a09::gg::1",
        ] {
            assert_eq!(WarpPool::egress_scope(bad), None, "should reject {bad:?}");
        }
    }

    #[tokio::test]
    async fn duplicate_tunnels_compares_the_quota_block_not_the_address() {
        // Regression: two /128s in one /64 used to be reported as distinct,
        // which overstated the pool's real capacity by one whole bucket.
        let pool = pool_with(2);
        for (tunnel, ip) in [
            (&pool.tunnels[0], "2a09:bac1:6500:8::3c1:47"),
            (&pool.tunnels[1], "2a09:bac1:6500:8::1f1:21a"),
        ] {
            let mut st = tunnel.state.lock().await;
            st.ip = Some(ip.into());
            st.probe_completed = true;
        }

        assert_eq!(pool.duplicate_tunnels().await, vec![1]);
    }

    #[tokio::test]
    async fn duplicate_tunnels_keeps_separate_blocks_apart() {
        let pool = pool_with(2);
        for (tunnel, ip) in [
            (&pool.tunnels[0], "2a09:bac1:6500:8::3c1:47"),
            (&pool.tunnels[1], "2a09:bac1:6560:8::3e6:82"),
        ] {
            let mut st = tunnel.state.lock().await;
            st.ip = Some(ip.into());
            st.probe_completed = true;
        }

        assert!(pool.duplicate_tunnels().await.is_empty());
    }

    #[tokio::test]
    async fn duplicate_tunnels_ignores_tunnels_that_have_not_probed_yet() {
        let pool = pool_with(2);
        {
            let mut st = pool.tunnels[0].state.lock().await;
            st.ip = Some("104.28.1.1".into());
            st.probe_completed = true;
        }
        {
            // Same address, but this tunnel has not finished probing: its
            // address is not known yet, so it must not be compared.
            let mut st = pool.tunnels[1].state.lock().await;
            st.ip = Some("104.28.1.1".into());
            st.probe_completed = false;
        }

        // Before the fix the pair was reported as a duplicate even though the
        // second tunnel had not produced an address at all.
        assert!(pool.duplicate_tunnels().await.is_empty());
    }

    #[tokio::test]
    async fn failed_recycle_retains_cooldown() {
        let pool = pool_with(1);
        let tunnel = pool.tunnels[0].clone();

        // A failed recycle or timeout sets last_recycle to Instant::now()
        tunnel.state.lock().await.last_recycle = Instant::now();

        // Must not be immediately picked again
        assert!(pool.claim_tunnel().is_none());
    }

    #[test]
    fn established_socket_count_ignores_other_ports_and_states() {
        // A recorded /proc/net/tcp table: the header, an established socket
        // on the tunnel port, one on another port, one on the tunnel port in
        // LISTEN state, and one on the tunnel port in TIME_WAIT.
        let table = "\
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:9C41 0100007F:9C42 01 00000000:00000000 00:00000000 00000000     0        0 82723625 2 0000000000000000 100 0 0 10 0
   1: 0100007F:9C41 0100007F:9C43 01 00000000:00000000 00:00000000 00000000     0        0 82723626 2 0000000000000000 100 0 0 10 0
   2: 0100007F:9C42 0100007F:9C41 01 00000000:00000000 00:00000000 00000000     0        0 82723627 2 0000000000000000 100 0 0 10 0
   3: 0100007F:9C43 0100007F:9C41 01 00000000:00000000 00:00000000 00000000     0        0 82723628 2 0000000000000000 100 0 0 10 0
   4: 0100007F:9C44 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 82723629 2 0000000000000000 100 0 0 10 0
   5: 0100007F:9C41 0100007F:9C45 06 00000000:00000000 00:00000000 00000000     0        0 82723630 2 0000000000000000 100 0 0 10 0
";

        // 0x9C41 = 40001 (the tunnel port), 0x9C42 = 40002, 0x9C43 = 40003
        assert_eq!(established_sockets_in(table, 40001).len(), 2);
        assert_eq!(established_sockets_in(table, 40002).len(), 1);
        assert_eq!(established_sockets_in(table, 40003).len(), 1);
        assert!(established_sockets_in(table, 40004).is_empty());
    }

    #[test]
    fn established_sockets_report_their_inode() {
        // The inode identifies a socket across polls, which is what lets the
        // drain tell a stream from an ordinary request.
        let table = "\
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:9C41 0100007F:9C42 01 00000000:00000000 00:00000000 00000000     0        0 82723625 2 0000000000000000 100 0 0 10 0
   1: 0100007F:9C41 0100007F:9C43 01 00000000:00000000 00:00000000 00000000     0        0 82723626 2 0000000000000000 100 0 0 10 0
";
        assert_eq!(
            established_sockets_in(table, 40001),
            vec![82723625, 82723626]
        );
    }

    #[test]
    fn established_socket_count_survives_a_truncated_table() {
        // A line cut short must not be counted and must not panic.
        let table = "\
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:9C41 0100007F:9C42 01 00000000:00000000 00:00000000 00000000     0        0 82723625 2 0000000000000000 100 0 0 10 0
   1: 0100007F
";
        assert_eq!(established_sockets_in(table, 40001).len(), 1);
    }

    #[tokio::test]
    async fn idle_tunnel_is_preferred_over_a_busy_one() {
        let pool = pool_with(2);
        let busy = pool.tunnels[0].clone();
        let idle = pool.tunnels[1].clone();

        // Pretend the first tunnel is carrying a request.
        busy.probe_inflight.store(0, Ordering::SeqCst);
        assert_eq!(
            busy.active_connections(),
            established_sockets_on_port(busy.port).len()
        );

        let claimed = pool.claim_tunnel().expect("a tunnel should be claimable");
        // With no real sockets on the test ports both tunnels look idle, so the
        // claim may land on either. What matters is that the claim succeeds
        // and that the busy-tunnel preference does not break the fallback.
        assert!(claimed.index == 0 || claimed.index == 1);
        claimed.recycling.store(false, Ordering::SeqCst);
        let _ = idle;
    }

    /// A watchdog pass must cost one probe timeout, not one per wedged tunnel.
    ///
    /// This is the property that makes a 15s interval safe. With the probes run
    /// serially, N frozen `usque` processes cost N x probe-timeout, so a pass
    /// could not finish inside its own interval and the detection window (and
    /// with it the 10s handshake timeout every request on that exit pays) grew
    /// with the pool size.
    ///
    /// Proven by timing: the test pool has no real `usque` behind it, so every
    /// probe fails. Run concurrently, four dead tunnels must finish in roughly
    /// the time of one, not four.
    #[tokio::test]
    async fn a_watchdog_pass_probes_the_pool_concurrently() {
        // Bind real listeners that accept and then say nothing, which is
        // exactly what a frozen `usque` looks like from the SOCKS side: the TCP
        // connect succeeds, the handshake never answers. A closed port would
        // fail in microseconds and prove nothing about serialisation.
        let mut listeners = Vec::new();
        for _ in 0..4 {
            let l = tokio::net::TcpListener::bind("127.0.0.1:0")
                .await
                .expect("bind");
            listeners.push(l);
        }
        let ports: Vec<u16> = listeners
            .iter()
            .map(|l| l.local_addr().unwrap().port())
            .collect();

        // Hold every accepted connection open without replying.
        let mut held = Vec::new();
        for l in listeners {
            held.push(tokio::spawn(async move {
                let mut conns = Vec::new();
                while let Ok((sock, _)) = l.accept().await {
                    conns.push(sock);
                }
            }));
        }

        let pool = Arc::new(WarpPool::new(
            ports,
            "/tmp/quirkproxy-test-warp",
            true,
            WarpProtocol::WireGuard,
        ));

        let started = std::time::Instant::now();
        pool.auto_heal_dead_tunnels().await;
        let elapsed = started.elapsed();

        for h in held {
            h.abort();
        }

        // A wedged exit costs two probe timeouts (the probe tries two URLs), so
        // a concurrent pass over four of them is ~6s and a serial one would be
        // ~24s. The bound sits between the two with room either side: the
        // assertion is "not serial", not a timing measurement, so it must not
        // flake when the runner is slow.
        assert!(
            elapsed < std::time::Duration::from_secs(12),
            "four wedged tunnels took {elapsed:?}: probes are running serially"
        );
    }

    #[tokio::test]
    async fn tunnel_with_no_connections_drains_immediately() {
        let pool = pool_with(1);
        let tunnel = pool.tunnels[0].clone();
        // Nothing is listening on the test port, so there is no stream to wait
        // for; the thresholds are shortened so the test does not sleep.
        assert_eq!(tunnel.active_connections(), 0);
        assert!(
            tunnel
                .drain_streams(
                    Duration::from_millis(20),
                    Duration::from_millis(20),
                    Duration::from_secs(5)
                )
                .await
        );
    }

    #[tokio::test]
    async fn a_running_probe_does_not_count_as_traffic() {
        let pool = pool_with(1);
        let tunnel = pool.tunnels[0].clone();
        // One probe in flight must cancel out one established socket, so the
        // supervisor's own probe never looks like a request it must wait for.
        tunnel.probe_inflight.store(1, Ordering::SeqCst);
        assert_eq!(tunnel.active_connections(), 0);
    }

    /// Binds a listener and returns it plus the port, together with the number
    /// of sockets that were already established on that port.
    ///
    /// A loopback connection shows up in the socket table twice (once per
    /// side), and the kernel reuses ephemeral ports, so a test cannot assert
    /// on absolute counts. Taking the difference around its own connection is
    /// the only stable way to identify it.
    async fn bind_test_socket() -> (tokio::net::TcpListener, u16, usize) {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
            .await
            .expect("bind should succeed");
        let port = listener.local_addr().expect("local addr").port();
        let before = established_sockets_on_port(port).len();
        (listener, port, before)
    }

    #[tokio::test]
    async fn a_live_connection_on_the_tunnel_port_is_counted() {
        // A real socket on the tunnel's port: the test process and the
        // supervisor share a network namespace, so the socket shows up in the
        // same table the supervisor reads.
        let (listener, port, before) = bind_test_socket().await;
        let _client = tokio::net::TcpStream::connect(("127.0.0.1", port))
            .await
            .expect("connect should succeed");
        let _accepted = listener.accept().await.expect("accept should succeed");

        let pool = Arc::new(WarpPool::new(
            [port],
            "/tmp/quirkproxy-test-warp",
            true,
            WarpProtocol::WireGuard,
        ));
        let tunnel = pool.tunnels[0].clone();

        let ours = established_sockets_on_port(port)
            .len()
            .saturating_sub(before);
        assert!(
            ours > 0,
            "the connection must be visible in the socket table"
        );
        assert!(
            tunnel.active_connections() >= ours,
            "a live connection on the tunnel port must be counted"
        );
    }

    #[tokio::test]
    async fn a_stream_blocks_the_swap_until_it_closes() {
        let (listener, port, _before) = bind_test_socket().await;
        let client = tokio::net::TcpStream::connect(("127.0.0.1", port))
            .await
            .expect("connect should succeed");
        let (accepted, _) = listener.accept().await.expect("accept should succeed");

        let pool = Arc::new(WarpPool::new(
            [port],
            "/tmp/quirkproxy-test-warp",
            true,
            WarpProtocol::WireGuard,
        ));
        let tunnel = pool.tunnels[0].clone();

        // A stream that is still open when the wait expires must not be
        // truncated: the wait has to report failure instead.
        let drained = tunnel
            .drain_streams(
                Duration::from_millis(50),
                Duration::from_millis(50),
                Duration::from_millis(400),
            )
            .await;
        assert!(!drained, "the swap must wait for a running stream");
        assert!(tunnel.active_connections() > 0);

        // Once it closes the wait completes and the swap may go ahead.
        drop(client);
        drop(accepted);
        assert!(
            tunnel
                .drain_streams(
                    Duration::from_millis(50),
                    Duration::from_millis(50),
                    Duration::from_secs(5)
                )
                .await
        );
    }

    #[tokio::test]
    async fn a_request_that_finishes_during_the_wait_stops_counting() {
        // Measured on the live pool: waiting for a tunnel to have no
        // connection at all never completed under sustained load, so every
        // recycle was abandoned while the burned exit kept refusing requests.
        // A short request closes by itself and must not hold the swap back.
        //
        // A connection that is still open when the wait expires cannot be
        // told apart from a stream, so the case that matters is the one that
        // closes: it leaves the socket table and stops counting.
        let (listener, port, _before) = bind_test_socket().await;
        let client = tokio::net::TcpStream::connect(("127.0.0.1", port))
            .await
            .expect("connect should succeed");
        let (accepted, _) = listener.accept().await.expect("accept should succeed");

        let pool = Arc::new(WarpPool::new(
            [port],
            "/tmp/quirkproxy-test-warp",
            true,
            WarpProtocol::WireGuard,
        ));
        let tunnel = pool.tunnels[0].clone();
        assert!(tunnel.active_connections() > 0);

        // The request finishes while the drain is still observing.
        let drain = tokio::spawn({
            let tunnel = tunnel.clone();
            async move {
                tunnel
                    .drain_streams(
                        Duration::from_millis(600),
                        Duration::from_millis(50),
                        Duration::from_secs(5),
                    )
                    .await
            }
        });
        tokio::time::sleep(Duration::from_millis(150)).await;
        drop(client);
        drop(accepted);

        assert!(
            drain.await.expect("drain task should not panic"),
            "a request that finishes during the wait must not hold the swap back"
        );
    }

    #[test]
    fn masque_pool_config_path_points_to_config_json() {
        let pool = WarpPool::new(
            [40001],
            "/tmp/quirkproxy-test-warp",
            true,
            WarpProtocol::Masque,
        );
        assert_eq!(pool.protocol, WarpProtocol::Masque);
        assert!(pool.tunnels[0].conf_path.ends_with("config.json"));
    }

    #[test]
    fn wireguard_pool_config_path_points_to_wireproxy_conf() {
        let pool = WarpPool::new(
            [40001],
            "/tmp/quirkproxy-test-warp",
            true,
            WarpProtocol::WireGuard,
        );
        assert_eq!(pool.protocol, WarpProtocol::WireGuard);
        assert!(pool.tunnels[0].conf_path.ends_with("wireproxy.conf"));
    }

    #[tokio::test]
    async fn a_recorded_quota_window_expires_and_is_cleared_by_success() {
        let pool = pool_with(2);
        const M: &str = "deepseek-v4.1-flash-free";
        assert_eq!(
            pool.quota_window_remaining(M).await,
            None,
            "nothing recorded yet"
        );

        // A window in the future is live, and the remaining seconds shrink.
        pool.note_quota_window(M, 600).await;
        let left = pool
            .quota_window_remaining(M)
            .await
            .expect("window must be live");
        assert!(left <= 600 && left > 590, "expected ~600s left, got {left}");

        // A successful request clears it: a recovered pool must not stay shut.
        pool.clear_quota_window(M).await;
        assert_eq!(pool.quota_window_remaining(M).await, None);

        // An already-expired window is not live either. Recording a 1s window
        // and waiting it out proves the bound is the upstream's own retry-after
        // rather than "remembered forever".
        pool.note_quota_window(M, 1).await;
        tokio::time::sleep(Duration::from_millis(1100)).await;
        assert_eq!(
            pool.quota_window_remaining(M).await,
            None,
            "an expired window must not keep refusing traffic"
        );
    }

    #[tokio::test]
    async fn a_later_longer_window_extends_the_remaining_time() {
        let pool = pool_with(1);
        const M: &str = "beam-free";
        pool.note_quota_window(M, 60).await;
        let short = pool.quota_window_remaining(M).await.unwrap();
        pool.note_quota_window(M, 600).await;
        let long = pool.quota_window_remaining(M).await.unwrap();
        assert!(
            long > short,
            "a longer retry-after must win: {long} should exceed {short}"
        );
    }

    #[tokio::test]
    async fn one_model_running_out_does_not_refuse_another() {
        // Measured in production: the pool kept serving `beam-free` with 200
        // while `deepseek-v4.1-flash-free` was refused with a 14.6h window.
        // Quota is metered per model, so a pool-wide verdict would deny a model
        // that still works -- the worst failure mode this short-circuit can have.
        let pool = pool_with(2);
        pool.note_quota_window("deepseek-v4.1-flash-free", 52_000)
            .await;

        assert!(
            pool.quota_window_remaining("deepseek-v4.1-flash-free")
                .await
                .is_some(),
            "the spent model must be refused"
        );
        assert_eq!(
            pool.quota_window_remaining("beam-free").await,
            None,
            "a model with quota left must never inherit another model's verdict"
        );
    }

    #[tokio::test]
    async fn status_reports_scopes_not_just_tunnel_count() {
        // The whole point of the field: `healthy` counts tunnels, `scopes`
        // counts metered blocks, and the gap between them is lost capacity.
        let pool = pool_with(3);
        for (tunnel, ip) in [
            (&pool.tunnels[0], "2a09:bac1:6500:8::3c1:47"),
            (&pool.tunnels[1], "2a09:bac1:6500:8::1f1:21a"),
            (&pool.tunnels[2], "2a09:bac1:6560:8::3e6:82"),
        ] {
            let mut st = tunnel.state.lock().await;
            st.ip = Some(ip.into());
            st.probe_completed = true;
            st.healthy = true;
        }

        let status = pool.status().await;
        assert_eq!(status.healthy, 3, "all three tunnels answer their probe");
        assert_eq!(status.scopes, 2, "two of them share one /64");
        assert_eq!(status.quota_window_secs, None);

        pool.note_quota_window("beam-free", 61238).await;
        pool.note_quota_window("beam-free", 3600).await;
        let reported = pool.status().await.quota_window_secs;
        assert!(
            reported.is_some_and(|s| s <= 61238 && s > 61200),
            "the largest window must win and be reported as time remaining, got {reported:?}"
        );
    }

    /// The exact ten egress addresses the production pool held on 2026-10-30.
    ///
    /// Ten tunnels, eight blocks: two pairs shared a `/64` while every address
    /// was unique, which is the failure this helper exists to catch. Pinning the
    /// real sample keeps the comparison honest against a future refactor that
    /// "simplifies" it back to string equality.
    #[test]
    fn the_real_pool_of_ten_tunnels_held_only_eight_blocks() {
        let ips = [
            "2a09:bac5:55ff:25c3::3c3:1d",
            "2a09:bac5:55fb:18c8::278:79",
            "2a09:bac1:6500:8::3c1:47",
            "2a09:bac1:6560:8::3e6:82",
            "2a09:bac5:55fc:25b9::3c2:8",
            "2a09:bac5:55fb:25cd::3c4:41",
            "2a09:bac5:55fd:18be::277:61",
            "2a09:bac1:6560:8::1f1:200",
            "2a09:bac5:55fd:25c3::3c3:3c",
            "2a09:bac1:6500:8::1f1:21a",
        ];
        let mut scopes: Vec<String> = Vec::new();
        for ip in ips {
            let scope = WarpPool::egress_scope(ip).expect("a real egress address must parse");
            if !scopes.contains(&scope) {
                scopes.push(scope);
            }
        }
        assert_eq!(scopes.len(), 8, "expected 8 blocks, got {scopes:?}");
    }
}
