//! A viewer's life across subscribe sessions (docs/67 D13): dial, feed the
//! [`Pipeline`], tick it, reconnect on the SPA's policy, and stop for good on
//! a viewer-terminal close.
//!
//! The transport is the engine's [`RelaySession`] seam, the same one the
//! broadcaster publishes through, so the iOS viewer dials with the desktop's
//! wtransport setup (Origin header, keepalive, the vendored patch that
//! surfaces refusal statuses) and its tests can run against a fake.

use crate::pipeline::{Pipeline, ViewerEvent};
use crate::playout::PlayoutPreset;
use crate::reconnect::{RECONNECT_MAX_ATTEMPTS, reconnect_delay_ms};
use gawk_engine::relay::{BoxFuture, RelaySession, SessionClose, StartError};
use gawk_wire::MAX_KEYFRAME_BYTES;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::mpsc;

/// The SPA's `REORDER_TICK_MS`: about one frame at 60 fps.
pub const TICK_MS: u64 = 16;
/// The SPA's stats cadence.
pub const STATS_INTERVAL_MS: f64 = 500.0;

/// Builds the subscribe URL (`viewer.ts`): the per-attempt session-group
/// `owner` token, the R59 labels (docs/61 D1; `app` from the injected
/// distribution, docs/67 D5), and `rejoin=1` on every attempt after the
/// first. No `secret`: it is publish-only. No `delivery`, `buffer` or
/// `parity`: v1 takes the fleet defaults (D13).
pub fn subscribe_url(
    relay_url: &str,
    broadcast_id: &str,
    owner: &str,
    rejoin: bool,
) -> Result<String, String> {
    let mut url = url::Url::parse(relay_url).map_err(|e| format!("bad relay URL: {e}"))?;
    if url.scheme() != "https" {
        return Err(format!("relay URL must be https, got {}", url.scheme()));
    }
    url.set_path(&format!("/subscribe/{broadcast_id}"));
    {
        let mut q = url.query_pairs_mut();
        q.append_pair("owner", owner);
        q.append_pair("app", gawk_engine::defaults::this().app);
        q.append_pair("os", gawk_engine::relay::CLIENT_OS);
        if rejoin {
            q.append_pair("rejoin", "1");
        }
    }
    Ok(url.into())
}

/// 16 hex chars, fresh per attempt: the SPA's `mintStripeOwnerToken`.
fn mint_owner() -> String {
    use std::hash::{BuildHasher, Hasher};
    let mut h = std::collections::hash_map::RandomState::new().build_hasher();
    h.write_u128(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_nanos()),
    );
    format!("{:016x}", h.finish())
}

/// Opens a subscribe session. Production is [`WtSubscribeDialer`].
pub trait SubscribeDialer: Send + Sync {
    fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RelaySession>, StartError>>;
}

/// The wtransport-backed dialer.
pub struct WtSubscribeDialer {
    pub origin: String,
    /// Skip certificate verification: dev certs only.
    pub insecure: bool,
}

impl SubscribeDialer for WtSubscribeDialer {
    fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RelaySession>, StartError>> {
        let url = url.to_owned();
        Box::pin(async move {
            let s =
                gawk_engine::transport::dial_subscribe(&url, &self.origin, self.insecure).await?;
            Ok(Arc::new(s) as Arc<dyn RelaySession>)
        })
    }
}

/// Where the viewer stands.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ViewerState {
    Connecting,
    Live,
    /// Waiting `delay_ms` before attempt `attempt`.
    Reconnecting {
        attempt: u32,
        delay_ms: u64,
        close_code: Option<u32>,
    },
    Ended(EndReason),
}

/// Why a viewer stopped for good.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EndReason {
    /// A viewer-terminal close: 4000 (broadcast ended) or 4006 (operator
    /// kill).
    Closed(u32),
    /// The relay has no such broadcast (404 on the dial).
    NotFound,
    /// [`RECONNECT_MAX_ATTEMPTS`] attempts in a row failed.
    GaveUp,
    /// The app stopped it.
    Stopped,
}

/// Receives everything a viewer produces, on the viewer's task.
pub trait ViewerSink: Send + Sync {
    fn state(&self, state: ViewerState);
    fn event(&self, event: ViewerEvent);
}

/// What the app can tell a running viewer.
#[derive(Debug, Clone, PartialEq)]
pub enum Command {
    /// The renderer put this frame on screen (D15's drop-to-live input).
    Presented {
        timestamp_us: u64,
    },
    /// The renderer's queue is too deep: resync at the next keyframe.
    Resync,
    SetPreset(PlayoutPreset),
    Stop,
}

/// What one viewer needs.
#[derive(Debug, Clone)]
pub struct ViewerConfig {
    pub relay_url: String,
    pub broadcast_id: String,
    pub preset: PlayoutPreset,
}

/// Milliseconds on the viewer's monotonic clock, the one every
/// `present_at_ms` is on.
#[derive(Debug, Clone, Copy)]
pub struct ViewerClock {
    epoch: Instant,
}

impl ViewerClock {
    pub fn new() -> Self {
        Self {
            epoch: Instant::now(),
        }
    }

    pub fn now_ms(&self) -> f64 {
        self.epoch.elapsed().as_secs_f64() * 1000.0
    }
}

impl Default for ViewerClock {
    fn default() -> Self {
        Self::new()
    }
}

/// How one connected session ended.
enum SessionEnd {
    Closed(Option<u32>),
    Stall,
    Stopped,
}

/// Runs a viewer until it ends; reports through `sink`, takes `commands`.
pub async fn run(
    cfg: ViewerConfig,
    dialer: Arc<dyn SubscribeDialer>,
    sink: Arc<dyn ViewerSink>,
    clock: ViewerClock,
    mut commands: mpsc::UnboundedReceiver<Command>,
) -> EndReason {
    let mut preset = cfg.preset;
    // Failed attempts since the last session that connected.
    let mut attempt: u32 = 0;
    let mut close_code: Option<u32> = None;
    let mut rejoin = false;
    // A 404 means "no such broadcast" only before any session connected:
    // after a relay restart the broadcast is unknown until its publisher
    // reclaims it, and a reconnecting viewer must ride that out.
    let mut ever_connected = false;
    sink.state(ViewerState::Connecting);
    loop {
        if attempt > 0 {
            if attempt > RECONNECT_MAX_ATTEMPTS {
                return end(&*sink, EndReason::GaveUp);
            }
            let delay_ms = reconnect_delay_ms(attempt, close_code);
            sink.state(ViewerState::Reconnecting {
                attempt,
                delay_ms,
                close_code,
            });
            let sleep = tokio::time::sleep(Duration::from_millis(delay_ms));
            tokio::pin!(sleep);
            loop {
                tokio::select! {
                    _ = &mut sleep => break,
                    cmd = commands.recv() => match cmd {
                        None | Some(Command::Stop) => return end(&*sink, EndReason::Stopped),
                        Some(Command::SetPreset(p)) => preset = p,
                        Some(_) => {}
                    },
                }
            }
        }
        let url = match subscribe_url(&cfg.relay_url, &cfg.broadcast_id, &mint_owner(), rejoin) {
            Ok(u) => u,
            // A malformed relay URL never heals.
            Err(_) => return end(&*sink, EndReason::GaveUp),
        };
        rejoin = true;
        let session = match dialer.dial(&url).await {
            Ok(s) => s,
            Err(e) if e.status == 404 && !ever_connected => {
                return end(&*sink, EndReason::NotFound);
            }
            Err(_) => {
                // Unlike the web, a native client can read a refusal's status;
                // a full fleet (429) or a network error is worth the ladder.
                attempt += 1;
                close_code = None;
                continue;
            }
        };
        ever_connected = true;
        sink.state(ViewerState::Live);
        let mut pipeline = Pipeline::new(preset);
        let outcome = run_session(
            &session,
            &mut pipeline,
            &*sink,
            &clock,
            &mut commands,
            &mut preset,
        )
        .await;
        session.close();
        match outcome {
            SessionEnd::Stopped => return end(&*sink, EndReason::Stopped),
            // A watchdog ended it: an abrupt drop, as far as the ladder goes.
            SessionEnd::Stall => close_code = None,
            SessionEnd::Closed(code) => {
                // A close without a code may still have been announced in-band
                // (SessionClosing, 0x17; R57).
                let code = code.or(pipeline.session_closing());
                if let Some(c) = code.filter(|&c| gawk_wire::terminal_for_viewer(c)) {
                    return end(&*sink, EndReason::Closed(c));
                }
                close_code = code;
            }
        }
        // The session connected, so the count restarts: this is attempt 1.
        attempt = 1;
    }
}

fn end(sink: &dyn ViewerSink, reason: EndReason) -> EndReason {
    sink.state(ViewerState::Ended(reason.clone()));
    reason
}

async fn run_session(
    session: &Arc<dyn RelaySession>,
    pipeline: &mut Pipeline,
    sink: &dyn ViewerSink,
    clock: &ViewerClock,
    commands: &mut mpsc::UnboundedReceiver<Command>,
    preset: &mut PlayoutPreset,
) -> SessionEnd {
    // Receiving runs in pump tasks that hand whole messages back here, so the
    // pipeline has one owner and no receive future is ever dropped half-way:
    // `select!` drops the losing branches every round, and an `accept_uni`
    // dropped while it reads a stream's header would lose that stream.
    // Stream bodies are read on tasks of their own (a keyframe can be large).
    let (dgram_tx, mut dgram_rx) = mpsc::unbounded_channel::<Vec<u8>>();
    let (stream_tx, mut stream_rx) = mpsc::unbounded_channel::<Vec<u8>>();
    let mut readers = tokio::task::JoinSet::new();
    {
        let session = session.clone();
        readers.spawn(async move {
            while let Ok(d) = session.receive_datagram().await {
                if dgram_tx.send(d).is_err() {
                    break;
                }
            }
        });
    }
    {
        let session = session.clone();
        let mut bodies = tokio::task::JoinSet::new();
        readers.spawn(async move {
            while let Ok(mut s) = session.accept_uni().await {
                let tx = stream_tx.clone();
                bodies.spawn(async move {
                    if let Ok(msg) = s.read_to_end(MAX_KEYFRAME_BYTES).await {
                        let _ = tx.send(msg);
                    }
                });
                while bodies.try_join_next().is_some() {}
            }
        });
    }
    let mut tick = tokio::time::interval(Duration::from_millis(TICK_MS));
    tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let closed = session.closed();
    tokio::pin!(closed);
    let mut out = Vec::new();
    let mut last_stats_ms = f64::NEG_INFINITY;
    loop {
        tokio::select! {
            close = &mut closed => {
                readers.abort_all();
                return SessionEnd::Closed(match close {
                    SessionClose::Code(c) => Some(c),
                    SessionClose::Abrupt(_) => None,
                });
            }
            Some(d) = dgram_rx.recv() => {
                pipeline.on_datagram(&d, clock.now_ms(), &mut out);
            }
            Some(msg) = stream_rx.recv() => {
                pipeline.on_stream(&msg, clock.now_ms(), &mut out);
            }
            _ = tick.tick() => {
                let now = clock.now_ms();
                if pipeline.stall(now).is_some() {
                    readers.abort_all();
                    return SessionEnd::Stall;
                }
                if let Some(ping) = pipeline.time_sync_ping(now) {
                    let _ = session.send_datagram(&ping);
                }
                pipeline.tick(now, &mut out);
                if now - last_stats_ms >= STATS_INTERVAL_MS {
                    last_stats_ms = now;
                    out.push(ViewerEvent::Stats(pipeline.stats(now)));
                }
            }
            cmd = commands.recv() => match cmd {
                None | Some(Command::Stop) => {
                    readers.abort_all();
                    return SessionEnd::Stopped;
                }
                Some(Command::Presented { timestamp_us }) => pipeline.note_presented(timestamp_us),
                Some(Command::Resync) => pipeline.request_resync(clock.now_ms(), &mut out),
                Some(Command::SetPreset(p)) => {
                    *preset = p;
                    pipeline.set_preset(p);
                }
            },
        }
        for e in out.drain(..) {
            sink.event(e);
        }
    }
}

#[cfg(test)]
mod tests;
