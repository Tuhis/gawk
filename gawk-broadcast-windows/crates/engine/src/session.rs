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
use crate::room::{self, Identity, RoomConfig, RoomDialer, RoomRequest, RoomSummary};
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
#[derive(Debug, Clone, Default)]
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
    /// R42 rooms (docs/44 §4.8): join this code (or static slug) and attach
    /// the broadcast on publish, re-attaching on every resume. Ignored when
    /// `room_new` is set.
    pub room_code: String,
    /// Mint a new dynamic room from this broadcast once its identity is
    /// known (`/room/new`).
    pub room_new: bool,
    /// A static room's attach key (presented on join).
    pub room_attach_secret: String,
    /// The relay's create secret (presented on a mint).
    pub room_create_secret: String,
    /// The tile label the attachment carries (bounded by the wire).
    pub room_label: String,
    /// The room nickname; empty lets the relay assign one.
    pub nickname: String,
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
    /// The relay fleet's advertised telemetry ingest URL (0x12, R37 docs/40
    /// §4.10) — validated absolute https. Rides its own uni stream, so it
    /// can arrive before OR after the hello; the shell must tolerate both
    /// orders.
    TelemetryEndpoint { url: String },
    /// A reclaim attempt is running (attempt counter for the status line).
    Resuming { attempt: u32 },
    /// The broadcast is back on a fresh session. The media side should force
    /// an IDR now to re-prime the relay's invalidated keyframe cache
    /// (docs/38 D5's improvement over the Linux engine).
    Resumed,
    /// The session is over; `error` is `None` for a clean stop.
    Ended { error: Option<String> },

    // --- R42 rooms (docs/44 §4.8) — the room control session's events ---
    /// The room's current picture: replaced from every snapshot, patched
    /// from every delta. The shell renders it; it never merges.
    RoomState(RoomSummary),
    /// A `/room/new` mint succeeded: the code viewers join by and the
    /// creator token (hex) — the grant the "Open room view" link carries.
    RoomCreated {
        code: String,
        creator_token_hex: String,
    },
    /// The room now lists this broadcast.
    RoomAttached,
    /// The room no longer lists this broadcast (or this session left).
    RoomDetached { reason: String },
    /// The room control session is over for good: the room ended (4007),
    /// the join was refused, or a reconnect gave up. Attached media sessions
    /// have their own lifecycle and are untouched.
    RoomEnded { reason: String },
    /// The relay refused a room command (attach, detach…).
    RoomRejected { reason: String, message: String },
    /// The room control session is being redialed (attempt counter).
    RoomReconnecting { attempt: u32 },
}

struct Shared {
    broadcast_id: String,
    resume_token_hex: String,
    viewer_count: Option<u32>,
    resumes: u64,
    resuming: bool,
}

/// One running room control session, owned by the publish session.
struct RoomHandle {
    stop: watch::Sender<bool>,
    requests: mpsc::UnboundedSender<RoomRequest>,
    task: tokio::task::JoinHandle<()>,
}

impl RoomHandle {
    fn stop(self) {
        let _ = self.stop.send(true);
        self.task.abort();
    }
}

/// A live publisher session. Media pumps feed [`Session::sender`]; the shell
/// consumes the event stream and calls [`Session::stop`] to end cleanly.
pub struct Session {
    cfg: SessionConfig,
    sender: Arc<Sender>,
    shared: Arc<Mutex<Shared>>,
    ts: Arc<TimeSyncClient>,
    stop_tx: watch::Sender<bool>,
    run: Mutex<Option<tokio::task::JoinHandle<()>>>,
    /// The publish identity the room session attaches with (announce +
    /// token, both known), re-published with a new generation on every
    /// resume so the room re-attaches.
    identity: watch::Sender<Option<Identity>>,
    events: mpsc::UnboundedSender<EngineEvent>,
    room: Arc<Mutex<Option<RoomHandle>>>,
    room_dialer: Arc<dyn RoomDialer>,
    rt: tokio::runtime::Handle,
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

        Ok(Self::start_with_session(cfg, relay, clock))
    }

    /// Brings the session up on an already-dialed relay — exactly what
    /// [`Session::start`] does after its dial. This is the relay seam
    /// (docs/38 §5): lifecycle behavior is defined by what happens when the
    /// transport fails, and only a scripted [`RelaySession`] lets tests
    /// produce those failures on demand.
    pub fn start_with_session(
        cfg: SessionConfig,
        relay: Arc<dyn RelaySession>,
        clock: Arc<dyn Clock>,
    ) -> (Arc<Self>, mpsc::UnboundedReceiver<EngineEvent>) {
        let dialer: Arc<dyn RoomDialer> = Arc::new(transport::WtRoomDialer {
            origin: cfg.origin.clone(),
            insecure: cfg.insecure,
        });
        Self::start_with_session_and_room_dialer(cfg, relay, clock, dialer)
    }

    /// [`Session::start_with_session`] with the room dial seam injected too,
    /// so a test can script the room control session alongside the relay.
    pub fn start_with_session_and_room_dialer(
        cfg: SessionConfig,
        relay: Arc<dyn RelaySession>,
        clock: Arc<dyn Clock>,
        room_dialer: Arc<dyn RoomDialer>,
    ) -> (Arc<Self>, mpsc::UnboundedReceiver<EngineEvent>) {
        let (events_tx, events_rx) = mpsc::unbounded_channel();
        let (stop_tx, stop_rx) = watch::channel(false);
        let (identity_tx, _) = watch::channel(None);
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

        let room = Arc::new(Mutex::new(None));
        let session = Arc::new(Session {
            cfg: cfg.clone(),
            sender: sender.clone(),
            shared: shared.clone(),
            ts: ts.clone(),
            stop_tx,
            run: Mutex::new(None),
            identity: identity_tx.clone(),
            events: events_tx.clone(),
            room: room.clone(),
            room_dialer,
            rt: tokio::runtime::Handle::current(),
        });

        let run = tokio::spawn(run_loop(RunCtx {
            cfg: cfg.clone(),
            relay,
            sender,
            shared,
            ts,
            clock,
            events: events_tx,
            stop: stop_rx,
            identity: identity_tx,
            room,
        }));
        *session.run.lock().unwrap() = Some(run);

        // The configured room: joined (or minted) from the start, so the
        // attach lands as soon as the identity does.
        if cfg.room_new {
            session.room_create();
        } else if !cfg.room_code.is_empty() {
            session.room_join(&cfg.room_code, &cfg.room_attach_secret, "");
        }

        (session, events_rx)
    }

    /// Joins a room (a dynamic code or a static slug) and attaches this
    /// broadcast once its identity is known; replaces any current room
    /// session. `creator_token_hex` rejoins a room this app minted earlier
    /// with the creator grant.
    pub fn room_join(&self, code: &str, attach_secret: &str, creator_token_hex: &str) {
        self.spawn_room(RoomConfig {
            code: code.to_owned(),
            attach_secret: attach_secret.to_owned(),
            creator_token_hex: creator_token_hex.to_owned(),
            ..self.room_config()
        });
    }

    /// Mints a new dynamic room from this broadcast (once announce and
    /// token are both known); replaces any current room session.
    pub fn room_create(&self) {
        self.spawn_room(RoomConfig {
            code: String::new(),
            ..self.room_config()
        });
    }

    /// Detaches this broadcast (when attached) and leaves the room. A no-op
    /// without a room session.
    pub fn room_detach(&self) {
        if let Some(h) = self.room.lock().unwrap().as_ref() {
            let _ = h.requests.send(RoomRequest::Detach);
        }
    }

    /// Ends the room control session without detaching — the attachment
    /// then follows the broadcast's own lifecycle on the relay.
    pub fn room_leave(&self) {
        if let Some(h) = self.room.lock().unwrap().take() {
            h.stop();
        }
    }

    fn room_config(&self) -> RoomConfig {
        RoomConfig {
            relay_url: self.cfg.relay_url.clone(),
            origin: self.cfg.origin.clone(),
            insecure: self.cfg.insecure,
            code: String::new(),
            attach_secret: String::new(),
            create_secret: self.cfg.room_create_secret.clone(),
            creator_token_hex: String::new(),
            label: self.cfg.room_label.clone(),
            nickname: self.cfg.nickname.clone(),
        }
    }

    fn spawn_room(&self, cfg: RoomConfig) {
        self.room_leave();
        let (stop, stop_rx) = watch::channel(false);
        let (requests, requests_rx) = mpsc::unbounded_channel();
        let task = self.rt.spawn(room::run_room(room::RoomCtx {
            cfg,
            dialer: self.room_dialer.clone(),
            identity: self.identity.subscribe(),
            events: self.events.clone(),
            stop: stop_rx,
            requests: requests_rx,
        }));
        *self.room.lock().unwrap() = Some(RoomHandle {
            stop,
            requests,
            task,
        });
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
    /// for in-flight ones, tears the session down, then closes the relay
    /// connection explicitly (the Go engine's `CloseWithError(0)`) so the
    /// relay sees the publisher leave now, not at the idle timeout. One
    /// event, one teardown — the run loop emits `Ended` exactly once.
    pub async fn stop(&self) {
        let _ = self.stop_tx.send(true);
        self.sender.wait().await;
        let run = self.run.lock().unwrap().take();
        if let Some(run) = run {
            let _ = run.await;
        }
        self.sender.current_relay().close();
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
    identity: watch::Sender<Option<Identity>>,
    room: Arc<Mutex<Option<RoomHandle>>>,
}

enum ServeEnd {
    Stopped,
    Closed(SessionClose),
}

/// Publishes the attach identity once BOTH halves are known. `generation`
/// is the resume count: a resume re-publishes the same id/token under a new
/// generation, which is what tells the room session to re-attach — the
/// relay's attach ownership is per control session, and a resumed
/// broadcaster must re-attach before it may detach (docs/44 §4.8).
fn publish_identity(shared: &Mutex<Shared>, identity: &watch::Sender<Option<Identity>>) {
    let sh = shared.lock().unwrap();
    if sh.broadcast_id.is_empty() {
        return;
    }
    let Some(token) = room::hex_decode(&sh.resume_token_hex) else {
        return;
    };
    if token.len() != wire::RESUME_TOKEN_SIZE {
        return;
    }
    let next = Identity {
        broadcast_id: sh.broadcast_id.clone(),
        resume_token: token,
        generation: sh.resumes,
    };
    drop(sh);
    identity.send_if_modified(|cur| {
        if cur.as_ref() == Some(&next) {
            return false;
        }
        *cur = Some(next);
        true
    });
}

async fn run_loop(mut ctx: RunCtx) {
    let end = run_sessions(&mut ctx).await;
    // The room control session lives exactly as long as the broadcast it
    // attaches: without a publisher there is nothing to attach, and the
    // attachment follows the broadcast's own grace on the relay.
    if let Some(h) = ctx.room.lock().unwrap().take() {
        h.stop();
    }
    let _ = ctx.events.send(end);
}

/// Serves and resumes until the session is over; returns the `Ended` event.
async fn run_sessions(ctx: &mut RunCtx) -> EngineEvent {
    let mut relay = ctx.relay.clone();
    loop {
        match serve_session(ctx, relay.clone()).await {
            ServeEnd::Stopped => {
                return EngineEvent::Ended { error: None };
            }
            ServeEnd::Closed(close) => {
                if let SessionClose::Code(code) = close
                    && terminal_for_publisher(code)
                {
                    return EngineEvent::Ended {
                        error: Some(close_code_message(code)),
                    };
                }
            }
        }

        // A recoverable loss: reclaim on a fresh session (docs/38 D5).
        match resume(ctx).await {
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
                // R42: the same identity under a new generation — the room
                // session re-attaches on the reclaimed broadcast.
                publish_identity(&ctx.shared, &ctx.identity);
                relay = next;
            }
            Err(None) => {
                // stop() cancelled us mid-reclaim; it owns the ending.
                return EngineEvent::Ended { error: None };
            }
            Err(Some(msg)) => {
                return EngineEvent::Ended {
                    error: Some(format!(
                        "lost the connection to the relay and could not resume: {msg}"
                    )),
                };
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
            changed = ctx.stop.changed() => {
                // Err means the stop Sender is gone: the shell dropped the
                // Session without a completed stop() (the quit path's
                // bounded timeout does this). That is a stop — treating it
                // as "no stop yet" would busy-loop changed() forever.
                if changed.is_err() || *ctx.stop.borrow() {
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
                        let identity = ctx.identity.clone();
                        stream_tasks.spawn(async move {
                            let read = tokio::time::timeout(
                                Duration::from_millis(SERVER_STREAM_READ_TIMEOUT_MS),
                                stream.read_to_end(SERVER_STREAM_READ_LIMIT),
                            )
                            .await;
                            let Ok(Ok(msg)) = read else { return };
                            handle_server_message(&msg, &sender, &shared, &events, &identity);
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
    identity: &watch::Sender<Option<Identity>>,
) {
    match dispatch_server_message(msg) {
        Ok(ServerMessage::Announce(id)) => {
            shared.lock().unwrap().broadcast_id = id.clone();
            let _ = events.send(EngineEvent::Announce { broadcast_id: id });
            publish_identity(shared, identity);
        }
        Ok(ServerMessage::ResumeToken(token_hex)) => {
            shared.lock().unwrap().resume_token_hex = token_hex.clone();
            let _ = events.send(EngineEvent::ResumeToken { token_hex });
            publish_identity(shared, identity);
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
        Ok(ServerMessage::TelemetryEndpoint(url)) => {
            let _ = events.send(EngineEvent::TelemetryEndpoint { url });
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
            changed = ctx.stop.changed() => {
                // A dropped stop Sender is a stop (see serve_session) — and
                // here spinning would also hammer the relay with dials.
                if changed.is_err() || *ctx.stop.borrow() {
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
