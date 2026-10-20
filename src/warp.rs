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
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::process::Command;
use tokio::sync::Mutex;
use tokio::task::JoinHandle;
use tracing::{error, info, warn};

/// Don't recycle the same tunnel twice inside this window. Re-registering
/// costs a WARP round-trip and briefly drops the tunnel, so a burst of 429s
/// must not turn into a registration storm.
const RECYCLE_COOLDOWN: Duration = Duration::from_secs(60);
/// How long a background recycle waits for in-flight streams to finish before
/// giving up. Recycling tears down the tunnel, which truncates any stream
/// still riding it.
const RECYCLE_DRAIN_TIMEOUT: Duration = Duration::from_secs(30);
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

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct RotateRecord {
    pub tunnel: usize,
    pub old_ip: Option<String>,
    pub new_ip: String,
    pub timestamp: String,
    pub reason: String,
}

#[derive(Clone, Debug, Serialize)]
pub struct TunnelStatus {
    pub index: usize,
    pub port: u16,
    pub ip: Option<String>,
    pub healthy: bool,
    pub recycling: bool,
}

#[derive(Clone, Debug, Serialize)]
pub struct WarpStatus {
    pub enabled: bool,
    pub running: bool,
    pub identities: usize,
    /// Number of tunnels whose last egress probe succeeded. `running` is true
    /// when at least one tunnel works; a partially-up pool is normal during
    /// startup and while a tunnel is being recycled.
    pub healthy: usize,
    pub proxy_urls: Vec<String>,
    pub rotations: usize,
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
    state: Mutex<TunnelState>,
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

    /// Re-registers a fresh WARP identity and rewrites the wireproxy config.
    /// The running child is not touched here; its supervisor restarts it.
    async fn re_register(&self) -> Result<()> {
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

        self.run_wgcf(&["register", "--accept-tos"]).await?;
        self.run_wgcf(&["generate"]).await?;

        self.write_conf()
    }

    /// Runs one `wgcf` subcommand inside this tunnel's identity directory with
    /// a hard timeout. Both callers hold `recycle_lock`, so a wedged child must
    /// not be able to block the pool indefinitely.
    async fn run_wgcf(&self, args: &[&str]) -> Result<()> {
        let label = args.first().copied().unwrap_or("wgcf");

        let child = Command::new("wgcf")
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
                return Err(e).with_context(|| format!("failed to execute wgcf {}", label))
            }
            Err(_) => {
                anyhow::bail!(
                    "wgcf {} timed out after {:?} (tunnel {})",
                    label,
                    WGCF_TIMEOUT,
                    self.index
                )
            }
        };

        if !output.status.success() {
            anyhow::bail!(
                "wgcf {} failed: {}",
                label,
                String::from_utf8_lossy(&output.stderr).trim()
            );
        }

        Ok(())
    }

    /// Ensures an identity exists on first boot, then writes the wireproxy
    /// config. A re-registration on boot would rotate the IP on every restart,
    /// so an existing profile is reused as-is.
    async fn ensure_identity(&self) -> Result<()> {
        tokio::fs::create_dir_all(&self.dir).await.ok();

        let profile = self.dir.join("wgcf-profile.conf");
        if !profile.exists() {
            info!(
                "Tunnel {}: no WARP identity yet, registering...",
                self.index
            );
            self.re_register().await?;
        } else {
            self.write_conf()?;
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
                }
                None => {
                    // A failed probe must not wipe a known-good exit address
                    // and must not mark a recycled tunnel unhealthy: the
                    // recycle path verifies the new egress on its own, and
                    // nulling the IP here would make `/healthz` flap between
                    // "healthy" and "no IP".
                    if st.ip.is_none() {
                        st.healthy = false;
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

    /// Runs `wireproxy` for this tunnel, restarting it with backoff whenever it
    /// exits unexpectedly, or immediately when a recycle asks for it through
    /// [`Tunnel::restart`]. This task owns the child handle, so restarting goes
    /// through it rather than through the process table.
    async fn supervise(self: &Arc<Self>, shutdown: Arc<AtomicBool>) {
        let mut backoff = Duration::from_secs(1);

        while !shutdown.load(Ordering::Relaxed) {
            let child = Command::new("wireproxy")
                .args(["-c", &self.conf_path.to_string_lossy()])
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .kill_on_drop(true)
                .spawn();

            let mut child = match child {
                Ok(c) => c,
                Err(e) => {
                    error!(
                        "Tunnel {}: failed to spawn wireproxy: {}. Retrying in {:?}",
                        self.index, e, backoff
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
                        "Tunnel {}: wireproxy exited ({:?}) after {:?}",
                        self.index, status, uptime
                    );
                    cancelled.store(true, Ordering::Relaxed);
                    self.state.lock().await.healthy = false;
                    if uptime >= TUNNEL_STABLE_UPTIME {
                        backoff = Duration::from_secs(1);
                        respawn_now = true;
                    }
                }
                _ = self.restart.notified() => {
                    info!("Tunnel {}: restart requested, stopping wireproxy...", self.index);
                    cancelled.store(true, Ordering::Relaxed);
                    let _ = child.kill().await;
                    let _ = child.wait().await;
                    {
                        let mut st = self.state.lock().await;
                        st.healthy = false;
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
        let st = self.state.lock().await;
        TunnelStatus {
            index: self.index,
            port: self.port,
            ip: st.ip.clone(),
            healthy: st.healthy,
            recycling: self.recycling.load(Ordering::Relaxed),
        }
    }
}

pub struct WarpPool {
    tunnels: Vec<Arc<Tunnel>>,
    enabled: bool,
    /// Round-robin cursor for picking which tunnel to recycle.
    cursor: AtomicUsize,
    /// Global count of streams currently riding any tunnel.
    inflight: Arc<AtomicUsize>,
    /// Serialises re-registrations: two concurrent `wgcf register` runs in the
    /// same directory would corrupt the identity files.
    recycle_lock: Mutex<()>,
    rotations: AtomicUsize,
    history: Mutex<Vec<RotateRecord>>,
}

impl WarpPool {
    pub fn new(
        tunnel_ports: impl IntoIterator<Item = u16>,
        state_dir: &str,
        enabled: bool,
    ) -> Self {
        let base = PathBuf::from(state_dir);
        let tunnels = tunnel_ports
            .into_iter()
            .enumerate()
            .map(|(index, port)| {
                let dir = base.join(format!("id{}", index + 1));
                let probe = build_probe_client(port);
                Arc::new(Tunnel {
                    index,
                    port,
                    conf_path: dir.join("wireproxy.conf"),
                    dir,
                    state: Mutex::new(TunnelState {
                        ip: None,
                        last_recycle: Instant::now() - RECYCLE_COOLDOWN,
                        healthy: false,
                        spawn_generation: 0,
                        restart_generation: 0,
                        probe_completed: false,
                    }),
                    recycling: AtomicBool::new(false),
                    restart: tokio::sync::Notify::new(),
                    probe,
                })
            })
            .collect();

        Self {
            tunnels,
            enabled,
            cursor: AtomicUsize::new(0),
            inflight: Arc::new(AtomicUsize::new(0)),
            recycle_lock: Mutex::new(()),
            rotations: AtomicUsize::new(0),
            history: Mutex::new(Vec::new()),
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
            if let Some(ip) = ip {
                if seen.contains(&ip) {
                    duplicates.push(t.index);
                } else {
                    seen.push(ip);
                }
            }
        }
        duplicates
    }

    /// Picks the next tunnel for recycling *and* claims it, in one step.
    ///
    /// Picking and claiming separately loses recycles: two concurrent 429s
    /// can both observe the same tunnel as free, and the loser of the claim
    /// silently drops its recycle while the tunnel it wanted stays burned.
    /// Claiming with a compare-exchange inside the selection loop makes the
    /// loser move on to the next candidate instead.
    fn claim_tunnel(&self) -> Option<Arc<Tunnel>> {
        let n = self.tunnels.len();
        if n == 0 {
            return None;
        }
        for _ in 0..n {
            let start = self.cursor.fetch_add(1, Ordering::Relaxed) % n;
            let t = &self.tunnels[start % n];
            if t.recycling.load(Ordering::Relaxed) {
                continue;
            }
            let eligible = match t.state.try_lock() {
                Ok(st) => st.last_recycle.elapsed() >= RECYCLE_COOLDOWN,
                Err(_) => false,
            };
            if !eligible {
                continue;
            }
            if t.recycling
                .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                .is_err()
            {
                // Another caller claimed it first; try the next candidate.
                continue;
            }
            return Some(t.clone());
        }
        None
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
        let _guard = self.recycle_lock.lock().await;

        let old_ip = tunnel.state.lock().await.ip.clone();
        info!(
            "Recycling tunnel {} ({}): {} (current egress: {:?})",
            tunnel.index, tunnel.port, reason, old_ip
        );

        let result = match tunnel.re_register().await {
            Ok(()) => {
                // The supervisor owns the child: signal it so the restart picks
                // up the freshly written config, and wait until it has.
                if !restart_tunnel(&tunnel).await {
                    tunnel.state.lock().await.last_recycle = Instant::now();
                    tunnel.recycling.store(false, Ordering::SeqCst);
                    return None;
                }

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
                    // After a recycle the tunnel runs a fresh identity, so the
                    // previous exit address is stale: report the new one, or
                    // nothing at all when it could not be verified.
                    st.ip = new_ip.clone();
                    st.probe_completed = true;
                }

                match new_ip.clone() {
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

    pub async fn status(&self) -> WarpStatus {
        let mut tunnels = Vec::with_capacity(self.tunnels.len());
        for t in &self.tunnels {
            tunnels.push(t.status().await);
        }

        WarpStatus {
            enabled: self.enabled(),
            running: tunnels.iter().any(|t| t.healthy),
            identities: tunnels.len(),
            healthy: tunnels.iter().filter(|t| t.healthy).count(),
            proxy_urls: if self.enabled() {
                self.proxy_urls()
            } else {
                Vec::new()
            },
            rotations: self.rotations.load(Ordering::Relaxed),
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

fn chrono_now() -> String {
    use std::time::SystemTime;
    let now = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    format!("{}", now)
}

async fn wait_for_shutdown(shutdown: &Arc<AtomicBool>) {
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
        .timeout(Duration::from_secs(8))
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
}
