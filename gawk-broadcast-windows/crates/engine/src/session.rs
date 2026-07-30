//! Session orchestration: the port of the Go engine's `Start`/`startLive`/
//! `supervise` (gawk-broadcast/internal/engine/engine.go + resume.go).
//! Callbacks become an event channel — same information, Rust-shaped.

use crate::clock::Clock;
use crate::dispatch::{
    SERVER_STREAM_READ_LIMIT, SERVER_STREAM_READ_TIMEOUT_MS, ServerMessage, dispatch_server_message,
};
use crate::relay::{RelaySession, SessionClose, StartError, StartPhase, publish_url};
use crate::resume::{
    RESUME_WINDOW, ResumeBackoff, close_code_message, resume_terminal, terminal_for_publisher,
};
use crate::sender::Sender;
use crate::stats::Stats;
use crate::timesync::{ClockMappingPublisher, TIME_SYNC_INTERVAL_MS, TimeSyncClient};
use crate::transport;
use gawk_wire as wire;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::{mpsc, watch};

/// Everything a session needs to start. URLs arrive RESOLVED (the config
/// layer owns "blank means the default").
#[derive(Debug, Clone)]
pub struct SessionConfig {
    pub relay_url: String,
    /// Reclaim this code instead of minting, when non-empty.
    pub broadcast_id: String,
    /// Hex resume token, sent only when `broadcast_id` is set too.
    pub resume_token_hex: String,
    pub publish_secret: String,
    pub origin: String,
    /// Skip TLS verification — dev certs only.
    pub insecure: bool,
}

/// What the session tells its shell. The Go engine's `Callbacks`, as events.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EngineEvent {
    /// The relay announced (or re-announced) this broadcast's code.
    Announce { broadcast_id: String },
    /// The resume token to persist, hex-encoded, for THIS broadcast id.
    ResumeToken { token_hex: String },
    /// The relay's live viewer count push (R18).
    ViewerCount(u32),
    /// R28 telemetry identity (WB7 consumes it; emitted since the stream
    /// arrives whether or not a reporter runs).
    TelemetryHello {
        enabled: bool,
        report_interval_ms: u16,
        token: Vec<u8>,
        broadcast_key_hex: String,
    },
    /// A reclaim attempt is running (attempt counter for the status line).
    Resuming { attempt: u32 },
    /// The broadcast is back on a fresh session. The media side should force
    /// an IDR now to re-prime the relay's invalidated keyframe cache
    /// (docs/38 D5's improvement over the Linux engine).
    Resumed,
    /// The session is over; `error` is `None` for a clean stop.
    Ended { error: Option<String> },
}

struct Shared {
    broadcast_id: String,
    resume_token_hex: String,
    viewer_count: Option<u32>,
    resumes: u64,
    resuming: bool,
}

/// A live publisher session. Media pumps feed [`Session::sender`]; the shell
/// consumes the event stream and calls [`Session::stop`] to end cleanly.
pub struct Session {
    sender: Arc<Sender>,
    shared: Arc<Mutex<Shared>>,
    ts: Arc<TimeSyncClient>,
    stop_tx: watch::Sender<bool>,
    run: Mutex<Option<tokio::task::JoinHandle<()>>>,
}

impl std::fmt::Debug for Session {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Session")
            .field("broadcast_id", &self.broadcast_id())
            .finish()
    }
}

impl Session {
    /// Dials the relay and brings the session up. Fails only in the connect
    /// phase — which is exactly when the shells may offer a mint instead
    /// (never on later failures; R1's bug).
    pub async fn start(
        cfg: SessionConfig,
        clock: Arc<dyn Clock>,
    ) -> Result<(Arc<Self>, mpsc::UnboundedReceiver<EngineEvent>), StartError> {
        let url = publish_url(
            &cfg.relay_url,
            &cfg.broadcast_id,
            &cfg.publish_secret,
            &cfg.resume_token_hex,
        )
        .map_err(|e| StartError {
            phase: StartPhase::Connect,
            status: 0,
            message: e,
        })?;

        let relay: Arc<dyn RelaySession> =
            Arc::new(transport::dial(&url, &cfg.origin, cfg.insecure).await?);

        let (events_tx, events_rx) = mpsc::unbounded_channel();
        let (stop_tx, stop_rx) = watch::channel(false);
        let sender = Arc::new(Sender::new(relay.clone(), clock.clone()));
        let ts = {
            let sender = sender.clone();
            Arc::new(TimeSyncClient::new(
                Box::new(move |d| sender.send_datagram_best_effort(d)),
                clock.clone(),
            ))
        };
        let shared = Arc::new(Mutex::new(Shared {
            broadcast_id: cfg.broadcast_id.clone(),
            resume_token_hex: cfg.resume_token_hex.clone(),
            viewer_count: None,
            resumes: 0,
            resuming: false,
        }));

        let session = Arc::new(Session {
            sender: sender.clone(),
            shared: shared.clone(),
            ts: ts.clone(),
            stop_tx,
            run: Mutex::new(None),
        });

        let run = tokio::spawn(run_loop(RunCtx {
            cfg,
            relay,
            sender,
            shared,
            ts,
            clock,
            events: events_tx,
            stop: stop_rx,
        }));
        *session.run.lock().unwrap() = Some(run);

        Ok((session, events_rx))
    }

    /// The send surface for the media pumps.
    pub fn sender(&self) -> Arc<Sender> {
        self.sender.clone()
    }

    /// The announced broadcast code, once it has arrived.
    pub fn broadcast_id(&self) -> String {
        self.shared.lock().unwrap().broadcast_id.clone()
    }

    /// Counters, merged from the sender and the session state.
    pub fn stats(&self) -> Stats {
        let mut st = self.sender.stats();
        let sh = self.shared.lock().unwrap();
        if let Some(count) = sh.viewer_count {
            st.viewer_count_available = true;
            st.viewer_count = count;
        }
        st.resumes = sh.resumes;
        st.resuming = sh.resuming;
        if let Some(s) = self.ts.sample() {
            st.time_sync_available = true;
            st.time_sync_rtt_ms = s.rtt_ms;
            st.time_sync_offset_us = s.offset_us;
        }
        st
    }

    /// Ends the broadcast: closes the sender to new keyframe writes, waits
    /// for in-flight ones, then tears the session down. One event, one
    /// teardown — the run loop emits `Ended` exactly once.
    pub async fn stop(&self) {
        let _ = self.stop_tx.send(true);
        self.sender.wait().await;
        let run = self.run.lock().unwrap().take();
        if let Some(run) = run {
            let _ = run.await;
        }
    }
}

struct RunCtx {
    cfg: SessionConfig,
    relay: Arc<dyn RelaySession>,
    sender: Arc<Sender>,
    shared: Arc<Mutex<Shared>>,
    ts: Arc<TimeSyncClient>,
    clock: Arc<dyn Clock>,
    events: mpsc::UnboundedSender<EngineEvent>,
    stop: watch::Receiver<bool>,
}

enum ServeEnd {
    Stopped,
    Closed(SessionClose),
}

async fn run_loop(mut ctx: RunCtx) {
    let mut relay = ctx.relay.clone();
    loop {
        match serve_session(&mut ctx, relay.clone()).await {
            ServeEnd::Stopped => {
                let _ = ctx.events.send(EngineEvent::Ended { error: None });
                return;
            }
            ServeEnd::Closed(close) => {
                if let SessionClose::Code(code) = close
                    && terminal_for_publisher(code)
                {
                    let _ = ctx.events.send(EngineEvent::Ended {
                        error: Some(close_code_message(code)),
                    });
                    return;
                }
            }
        }

        // A recoverable loss: reclaim on a fresh session (docs/38 D5).
        match resume(&mut ctx).await {
            Ok(next) => {
                ctx.sender.set_relay(next.clone());
                // The relay dropped this broadcast's caches and the reclaim
                // may have landed on a different pod: both halves of the
                // clock story start again from nothing.
                ctx.ts.reset();
                {
                    let mut sh = ctx.shared.lock().unwrap();
                    sh.resumes += 1;
                    sh.resuming = false;
                }
                let _ = ctx.events.send(EngineEvent::Resumed);
                relay = next;
            }
            Err(None) => {
                // stop() cancelled us mid-reclaim; it owns the ending.
                let _ = ctx.events.send(EngineEvent::Ended { error: None });
                return;
            }
            Err(Some(msg)) => {
                let _ = ctx.events.send(EngineEvent::Ended {
                    error: Some(format!(
                        "lost the connection to the relay and could not resume: {msg}"
                    )),
                });
                return;
            }
        }
    }
}

/// Serves one relay session until it dies or the shell stops the broadcast.
async fn serve_session(ctx: &mut RunCtx, relay: Arc<dyn RelaySession>) -> ServeEnd {
    let mut ping = tokio::time::interval(Duration::from_millis(TIME_SYNC_INTERVAL_MS));
    ping.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    // The mapping check runs on a fine cadence; the publisher decides.
    let mut mapping_tick = tokio::time::interval(Duration::from_millis(250));
    mapping_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut clock_pub = ClockMappingPublisher::new();
    let mut stream_tasks = tokio::task::JoinSet::new();

    let end = loop {
        tokio::select! {
            close = relay.closed() => break ServeEnd::Closed(close),
            _ = ctx.stop.changed() => {
                if *ctx.stop.borrow() {
                    break ServeEnd::Stopped;
                }
            }
            accepted = relay.accept_uni() => {
                match accepted {
                    Ok(mut stream) => {
                        // Read detached, bounded and deadlined: a slow-to-
                        // announce relay must stay distinguishable from a
                        // refused connect, and the token can beat the
                        // announce (docs/22 finding 9) — dispatch by type.
                        let sender = ctx.sender.clone();
                        let shared = ctx.shared.clone();
                        let events = ctx.events.clone();
                        stream_tasks.spawn(async move {
                            let read = tokio::time::timeout(
                                Duration::from_millis(SERVER_STREAM_READ_TIMEOUT_MS),
                                stream.read_to_end(SERVER_STREAM_READ_LIMIT),
                            )
                            .await;
                            let Ok(Ok(msg)) = read else { return };
                            handle_server_message(&msg, &sender, &shared, &events);
                        });
                    }
                    Err(_) => break ServeEnd::Closed(relay.closed().await),
                }
            }
            dgram = relay.receive_datagram() => {
                match dgram {
                    Ok(d) => handle_datagram(&d, ctx),
                    Err(_) => break ServeEnd::Closed(relay.closed().await),
                }
            }
            _ = ping.tick() => ctx.ts.ping(),
            _ = mapping_tick.tick() => {
                let sample = ctx.ts.sample();
                if clock_pub.due(ctx.clock.now_us(), sample.is_some())
                    && let Some(s) = sample {
                        let mut d = Vec::with_capacity(wire::CLOCK_MAPPING_SIZE);
                        wire::append_clock_mapping(&mut d, s.offset_us);
                        ctx.sender.send_datagram_best_effort(&d);
                    }
            }
        }
    };
    stream_tasks.abort_all();
    end
}

fn handle_server_message(
    msg: &[u8],
    sender: &Sender,
    shared: &Mutex<Shared>,
    events: &mpsc::UnboundedSender<EngineEvent>,
) {
    match dispatch_server_message(msg) {
        Ok(ServerMessage::Announce(id)) => {
            shared.lock().unwrap().broadcast_id = id.clone();
            let _ = events.send(EngineEvent::Announce { broadcast_id: id });
        }
        Ok(ServerMessage::ResumeToken(token_hex)) => {
            shared.lock().unwrap().resume_token_hex = token_hex.clone();
            let _ = events.send(EngineEvent::ResumeToken { token_hex });
        }
        Ok(ServerMessage::Capabilities(c)) => sender.apply_capabilities(c),
        Ok(ServerMessage::TelemetryHello {
            enabled,
            report_interval_ms,
            token,
            broadcast_key_hex,
        }) => {
            let _ = events.send(EngineEvent::TelemetryHello {
                enabled,
                report_interval_ms,
                token,
                broadcast_key_hex,
            });
        }
        // Unknown types and malformed messages: ignored — a newer relay
        // must not break this client, and strict parsing already dropped
        // anything misleading.
        Ok(ServerMessage::Unknown(_)) | Err(_) => {}
    }
}

fn handle_datagram(d: &[u8], ctx: &RunCtx) {
    if ctx.ts.handle_datagram(d) {
        return;
    }
    if d.len() >= 2
        && d[1] == wire::TYPE_VIEWER_COUNT
        && let Ok(count) = wire::parse_viewer_count(d)
    {
        ctx.shared.lock().unwrap().viewer_count = Some(count);
        let _ = ctx.events.send(EngineEvent::ViewerCount(count));
    }
    // Anything else relay→publisher is a future message: ignored.
}

/// Reclaims the broadcast on a fresh session, retrying with backoff until it
/// works, the relay says it never will, or the window closes.
/// `Err(None)` = stopped by the shell; `Err(Some(msg))` = gave up.
async fn resume(ctx: &mut RunCtx) -> Result<Arc<dyn RelaySession>, Option<String>> {
    let (id, token) = {
        let sh = ctx.shared.lock().unwrap();
        (sh.broadcast_id.clone(), sh.resume_token_hex.clone())
    };
    if id.is_empty() {
        // The transport died before the announce arrived: no code to
        // reclaim. Deliberately NOT a mint — the relay may have minted one
        // we never heard, and a second broadcast would strand it (R1).
        return Err(Some(
            "the relay session ended before the broadcast code arrived".into(),
        ));
    }
    let url =
        publish_url(&ctx.cfg.relay_url, &id, &ctx.cfg.publish_secret, &token).map_err(Some)?;

    ctx.shared.lock().unwrap().resuming = true;
    let mut backoff = ResumeBackoff::new();
    let deadline = tokio::time::Instant::now() + RESUME_WINDOW;
    let mut attempt: u32 = 0;
    loop {
        attempt += 1;
        if *ctx.stop.borrow() {
            ctx.shared.lock().unwrap().resuming = false;
            return Err(None);
        }
        let _ = ctx.events.send(EngineEvent::Resuming { attempt });
        let delay = backoff.next_delay();
        tokio::select! {
            _ = tokio::time::sleep(delay) => {}
            _ = ctx.stop.changed() => {
                if *ctx.stop.borrow() {
                    ctx.shared.lock().unwrap().resuming = false;
                    return Err(None);
                }
            }
        }

        match transport::dial(&url, &ctx.cfg.origin, ctx.cfg.insecure).await {
            Ok(session) => return Ok(Arc::new(session)),
            Err(e) => {
                if resume_terminal(e.status) || tokio::time::Instant::now() > deadline {
                    ctx.shared.lock().unwrap().resuming = false;
                    return Err(Some(e.to_string()));
                }
            }
        }
    }
}
