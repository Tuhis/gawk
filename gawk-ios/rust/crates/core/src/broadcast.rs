//! The broadcaster, as Swift sees it (docs/67 D6–D12, D17, D19, D21): one
//! [`Broadcaster`] per broadcast, the engine's publish session on a thread of
//! its own (D12's current-thread runtime) with the media pipeline in front.
//!
//! The capture code (ScreenCaptureKit, or D27's debug test source) asks
//! [`Broadcaster::plan`] what to make of each captured frame, converts it
//! upright into that size, and pushes it with [`Broadcaster::push_video`].
//! Pixel buffers cross the boundary as an opaque handle (the
//! `CVPixelBuffer`'s address, valid for the call: VideoToolbox retains what
//! it encodes), so neither side sees the other's object model (D3).
//!
//! Identity is Swift's to keep (D17): the code and resume token arrive on
//! the listener to store in the Keychain, and go back in on the next start
//! for an R17 reclaim within the grace.
//!
//! While live, the quality can change on the same code (docs/70 D12), the
//! room is the engine's room session with its controls (D16–D18), and the
//! counters carry the Upload row's rate and warning (D11).

use crate::rooms::{RoomView, room_view};
use gawk_broadcast::audio::Asbd;
use gawk_broadcast::pipeline::{Pipeline, PipelineCounters};
use gawk_broadcast::rotation::Rotation;
use gawk_broadcast::rung::{Quality as RungQuality, Rung};
use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::relay::StartError;
use gawk_engine::room::RoomSummary;
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use gawk_engine::stats::Stats;
use gawk_engine::telemetry::{Hello, Reporter};
use gawk_engine::uplink::UplinkMonitor;
use std::future::Future;
use std::sync::{Arc, Mutex};
use tokio::sync::{mpsc, oneshot};

#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum Quality {
    Standard,
    Cellular,
}

impl From<Quality> for RungQuality {
    fn from(q: Quality) -> Self {
        match q {
            Quality::Standard => RungQuality::Standard,
            Quality::Cellular => RungQuality::Cellular,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum FrameRotation {
    R0,
    R90,
    R180,
    R270,
}

impl From<FrameRotation> for Rotation {
    fn from(r: FrameRotation) -> Self {
        match r {
            FrameRotation::R0 => Rotation::R0,
            FrameRotation::R90 => Rotation::R90,
            FrameRotation::R180 => Rotation::R180,
            FrameRotation::R270 => Rotation::R270,
        }
    }
}

impl From<Rotation> for FrameRotation {
    fn from(r: Rotation) -> Self {
        match r {
            Rotation::R0 => FrameRotation::R0,
            Rotation::R90 => FrameRotation::R90,
            Rotation::R180 => FrameRotation::R180,
            Rotation::R270 => FrameRotation::R270,
        }
    }
}

/// What to make of a captured frame: turn it by `rotation`, scale it into
/// `width` × `height`, as `420v`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Record)]
pub struct FramePlan {
    pub rotation: FrameRotation,
    pub width: u32,
    pub height: u32,
}

#[derive(Debug, Clone, uniffi::Record)]
pub struct BroadcastOptions {
    pub relay_url: String,
    /// The per-server publish secret (R37, docs/40), empty for none.
    pub publish_secret: String,
    /// Reclaim this code (R17) when set together with `resume_token_hex`.
    pub broadcast_id: String,
    pub resume_token_hex: String,
    pub quality: Quality,
    /// A room to attach on publish (D21), empty for none.
    pub room_code: String,
    pub room_attach_secret: String,
    pub nickname: String,
    /// Skip certificate verification: a local dev relay only.
    pub insecure: bool,
    /// The user opted in to diagnostics (D23: off until then, as on the
    /// desktop). Where reports go follows the desktop's rules: the relay's
    /// advertised ingest, else the default collector on the default fleet
    /// only.
    pub telemetry: bool,
    /// How frames are captured, for telemetry's `capturePath`:
    /// "screencapturekit" or "test-source" (D27).
    pub capture_source: String,
    /// Mint a new room once the broadcast has its code (docs/70 D16's
    /// "Create a new room"); `room_code` is then ignored.
    pub room_new: bool,
    /// Rejoin `room_code` as its creator with this token (hex), from a
    /// room link's `?rt=` grant or an earlier mint (docs/60 D8). Empty for
    /// none.
    pub room_creator_token_hex: String,
}

/// One audio buffer's `AudioStreamBasicDescription` fields (D11).
#[derive(Debug, Clone, Copy, PartialEq, uniffi::Record)]
pub struct AudioDescription {
    pub format_id: u32,
    pub format_flags: u32,
    pub sample_rate: f64,
    pub channels: u32,
    pub bits_per_channel: u32,
}

#[derive(Debug, Clone, PartialEq, Eq, uniffi::Enum)]
pub enum BroadcastStatus {
    Connecting,
    /// Publishing as `code`; `join_link` is the reference UI's watch link.
    Live {
        code: String,
        join_link: String,
    },
    /// The engine is reclaiming the code after a loss (attempt counter).
    Resuming {
        attempt: u32,
    },
    /// Ended. `reclaim_status` names a refused reclaim (401 wrong secret,
    /// 403 token refused, 404 the code expired, 451 banned).
    Ended {
        error: Option<String>,
        reclaim_status: Option<u16>,
    },
}

/// Where frames went (the live status and diagnostics), and the Upload
/// row (docs/70 D11). The frame counts cover the whole broadcast, across
/// quality changes.
#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Record)]
pub struct BroadcastCounters {
    pub pushed: u64,
    pub admitted: u64,
    pub dropped_no_content: u64,
    pub dropped_over_rate: u64,
    pub dropped_backpressure: u64,
    pub encoded: u64,
    pub width: u32,
    pub height: u32,
    /// The rung's frame rate; 0 before an encoder.
    pub fps: u32,
    /// The rung's bitrate cap; 0 before an encoder.
    pub peak_bitrate_bps: u32,
    /// False until two once-a-second samples exist.
    pub upload_available: bool,
    /// Media bits per second handed to the relay connection over the last
    /// second: video datagrams with their parity, keyframe streams and
    /// audio, headers included. QUIC's own overhead, the time-sync pings
    /// and the audio config datagrams aren't counted.
    pub upload_bps: u64,
    /// The engine's uplink watchdog (`UplinkMonitor`): the upload isn't
    /// keeping up. Raised after 5 bad seconds, cleared after 15 good ones.
    pub uplink_warning: bool,
}

/// What happened to the broadcast's room (docs/70 D16–D18). The room's
/// picture itself arrives on [`BroadcastListener::on_room_state`].
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Enum)]
pub enum BroadcastRoomEvent {
    /// A room was minted for this broadcast: its code, and the creator
    /// token (hex) that rejoins it as its creator and that its link's
    /// `?rt=` carries.
    Created {
        code: String,
        creator_token_hex: String,
    },
    /// The room lists this broadcast.
    Attached,
    /// The room no longer lists it. After [`Broadcaster::room_leave`] this
    /// is the leave completing. `by_creator`: the room's creator removed
    /// it, and the broadcaster has left the room (docs/60 D10), so no more
    /// room states or events come until the next `room_join` or
    /// `room_create`. Any other detach leaves the broadcaster in the room
    /// without its stream.
    Detached { reason: String, by_creator: bool },
    /// The room session is over: the room ended, the join was refused, or
    /// a reconnect gave up. The broadcast carries on.
    Ended { reason: String },
    /// The relay refused a room command.
    Rejected { reason: String, message: String },
    /// The room session is being redialed (attempt counter).
    Reconnecting { attempt: u32 },
}

/// Implemented in Swift; called on the broadcaster's thread.
#[uniffi::export(with_foreign)]
pub trait BroadcastListener: Send + Sync {
    fn on_status(&self, status: BroadcastStatus);
    /// Persist for a reclaim within the grace (D17: Keychain).
    fn on_identity(&self, code: String, resume_token_hex: String);
    fn on_viewer_count(&self, count: u32);
    /// The room's picture, on every change. `attached`: this broadcast is
    /// among its tiles. `needs_key`: a gated static room admitted it only
    /// as a watcher, so its key is asked for (docs/70 D16).
    fn on_room_state(&self, room: RoomView, attached: bool, needs_key: bool);
    fn on_room_event(&self, event: BroadcastRoomEvent);
    /// The pipeline's first video failure; the broadcast ends.
    fn on_failure(&self, text: String);
}

struct Live {
    session: Arc<Session>,
    pipeline: Arc<Pipeline>,
    quality: Quality,
    /// What the pipelines replaced by quality changes counted, so the
    /// counters never go backwards.
    retired: PipelineCounters,
    upload: Upload,
    /// The room's creator removed this broadcast and the broadcaster left
    /// (docs/60 D10): what that room session still says is not forwarded.
    /// The next `room_join` or `room_create` clears it.
    room_left: bool,
}

impl Live {
    fn counters(&self) -> PipelineCounters {
        fold(&self.retired, &self.pipeline.counters())
    }
}

/// The Upload row's last reading, written once a second by the run loop.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
struct Upload {
    /// `None` until two samples exist. A quality change keeps the last
    /// reading until the new media has a window of its own (D12: nothing
    /// is narrated).
    bps: Option<u64>,
    warning: bool,
}

/// What Swift's thread asks of the run loop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Command {
    SetQuality(Quality),
}

/// Pairs the broadcast code with its resume token for `on_identity`. The
/// relay sends Announce and the token on separate uni streams, in either
/// order, so whichever arrives second completes the pair.
#[derive(Default)]
struct Identity {
    code: String,
    pending_token: Option<String>,
}

impl Identity {
    fn on_announce(&mut self, code: &str) -> Option<(String, String)> {
        self.code = code.to_owned();
        self.pending_token.take().map(|t| (self.code.clone(), t))
    }

    fn on_token(&mut self, token_hex: String) -> Option<(String, String)> {
        if self.code.is_empty() {
            self.pending_token = Some(token_hex);
            return None;
        }
        Some((self.code.clone(), token_hex))
    }
}

/// One broadcast. [`Broadcaster::stop`] ends it cleanly.
#[derive(uniffi::Object)]
pub struct Broadcaster {
    live: Arc<Mutex<Option<Live>>>,
    stop: Mutex<Option<oneshot::Sender<()>>>,
    commands: mpsc::UnboundedSender<Command>,
}

/// The run loop's ends of a [`Broadcaster`]'s state and channels.
struct Wiring {
    live: Arc<Mutex<Option<Live>>>,
    commands: mpsc::UnboundedReceiver<Command>,
    stop: oneshot::Receiver<()>,
}

impl Broadcaster {
    fn wired() -> (Arc<Self>, Wiring) {
        let live: Arc<Mutex<Option<Live>>> = Arc::default();
        let (stop_tx, stop) = oneshot::channel();
        let (commands_tx, commands) = mpsc::unbounded_channel();
        let this = Arc::new(Self {
            live: live.clone(),
            stop: Mutex::new(Some(stop_tx)),
            commands: commands_tx,
        });
        (
            this,
            Wiring {
                live,
                commands,
                stop,
            },
        )
    }

    /// The publish session, once it is up.
    fn session(&self) -> Option<Arc<Session>> {
        self.live
            .lock()
            .unwrap()
            .as_ref()
            .map(|l| l.session.clone())
    }

    /// The publish session, for a room session Swift is starting.
    fn session_for_a_new_room(&self) -> Option<Arc<Session>> {
        let mut live = self.live.lock().unwrap();
        let l = live.as_mut()?;
        l.room_left = false;
        Some(l.session.clone())
    }
}

#[uniffi::export]
impl Broadcaster {
    /// Starts the publish session; media may be pushed once the status is
    /// [`BroadcastStatus::Live`] (pushes before then are dropped).
    #[uniffi::constructor]
    pub fn start(options: BroadcastOptions, listener: Arc<dyn BroadcastListener>) -> Arc<Self> {
        let (this, wiring) = Self::wired();
        std::thread::Builder::new()
            .name("gawk-broadcast".into())
            .spawn(move || {
                let rt = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("a tokio runtime");
                rt.block_on(run(options, listener, wiring, Session::start));
            })
            .expect("spawn the broadcast thread");
        this
    }

    /// What to make of a `width` × `height` capture wanting `rotation`
    /// (`None`: face up/down or unknown) at host time `pts_100ns`.
    pub fn plan(
        &self,
        width: u32,
        height: u32,
        rotation: Option<FrameRotation>,
        pts_100ns: i64,
    ) -> FramePlan {
        let live = self.live.lock().unwrap();
        let Some(l) = live.as_ref() else {
            let rung = RungQuality::Standard.rung(width, height);
            return FramePlan {
                rotation: FrameRotation::R0,
                width: rung.width,
                height: rung.height,
            };
        };
        let p = l
            .pipeline
            .plan(width, height, rotation.map(Into::into), pts_100ns);
        FramePlan {
            rotation: p.rotation.into(),
            width: p.width,
            height: p.height,
        }
    }

    /// One upright `420v` frame (`pixel_buffer`: a `CVPixelBuffer`'s
    /// address, valid for the call), its host PTS in 100 ns, and its
    /// `SCFrameStatus`.
    pub fn push_video(&self, pixel_buffer: u64, pts_100ns: i64, status: i64) {
        let pipeline = match self.live.lock().unwrap().as_ref() {
            Some(l) => l.pipeline.clone(),
            None => return,
        };
        if pixel_buffer == 0 {
            return;
        }
        // SAFETY: Swift passes the address of a live CVPixelBuffer that it
        // keeps alive for the duration of this call; it is only borrowed
        // here, and VideoToolbox takes its own retain for the encode.
        let pixels = unsafe { &*(pixel_buffer as *const objc2_core_video::CVPixelBuffer) };
        pipeline.push_video(pixels, Some(pts_100ns), status);
    }

    /// One audio buffer: its ASBD, its buffers (one per channel when
    /// planar), its frame count and host PTS in 100 ns.
    pub fn push_audio(
        &self,
        format: AudioDescription,
        buffers: Vec<Vec<u8>>,
        frames: u32,
        pts_100ns: i64,
    ) {
        let pipeline = match self.live.lock().unwrap().as_ref() {
            Some(l) => l.pipeline.clone(),
            None => return,
        };
        let asbd = Asbd {
            format_id: format.format_id,
            format_flags: format.format_flags,
            sample_rate: format.sample_rate,
            channels: format.channels,
            bits_per_channel: format.bits_per_channel,
        };
        let refs: Vec<&[u8]> = buffers.iter().map(Vec::as_slice).collect();
        pipeline.push_audio(&asbd, &refs, frames as usize, Some(pts_100ns));
    }

    /// "active", "unavailable", "error" or "off" (D11: audio never fails a
    /// broadcast, it says so instead).
    pub fn audio_state(&self) -> String {
        match self.live.lock().unwrap().as_ref() {
            Some(l) => l.pipeline.audio_state().into(),
            None => "off".into(),
        }
    }

    /// Where frames went so far, and the Upload row; zeros before the
    /// session is up.
    pub fn counters(&self) -> BroadcastCounters {
        match self.live.lock().unwrap().as_ref() {
            Some(l) => broadcast_counters(l.counters(), l.pipeline.rung(), l.upload),
            None => broadcast_counters(PipelineCounters::default(), None, Upload::default()),
        }
    }

    /// The network path changed (D19): reconnect with the resume token now
    /// instead of waiting out idle timeouts. QUIC migration is off (D12).
    pub fn path_changed(&self) {
        if let Some(session) = self.session() {
            session.republish();
        }
    }

    /// The capture resumed after a pause (a call): re-prime with an IDR.
    pub fn force_idr(&self) {
        if let Some(l) = self.live.lock().unwrap().as_ref() {
            l.pipeline.force_idr();
        }
    }

    /// Changes the quality while live (docs/70 D12): the desktop's quick
    /// restart (docs/64 D8). A new pipeline at `quality` replaces the live
    /// one, and the publish leg is re-established on the same code with
    /// the same resume token. Viewers see a short freeze; the status stays
    /// `Live`, because the restart's own reclaim is not narrated. A no-op
    /// when the quality is unchanged. Asked for before the broadcast has its
    /// code and token, the latest one is kept and applied once it does: a
    /// republish needs both to reclaim the code.
    pub fn set_quality(&self, quality: Quality) {
        let _ = self.commands.send(Command::SetQuality(quality));
    }

    /// Joins a room (a code or a static slug), replacing any current one;
    /// the broadcast attaches once it has its code. `attach_secret` is a
    /// gated static room's key and `creator_token_hex` rejoins as its
    /// creator; either may be empty. A no-op before the session is up.
    pub fn room_join(&self, code: String, attach_secret: String, creator_token_hex: String) {
        if let Some(session) = self.session_for_a_new_room() {
            session.room_join(&code, &attach_secret, &creator_token_hex);
        }
    }

    /// Mints a new room from this broadcast (docs/70 D16), replacing any
    /// current one: [`BroadcastRoomEvent::Created`] follows.
    pub fn room_create(&self) {
        if let Some(session) = self.session_for_a_new_room() {
            session.room_create();
        }
    }

    /// Detaches this broadcast and leaves the room (docs/70 D17's Leave):
    /// [`BroadcastRoomEvent::Detached`] follows, and no more room states.
    pub fn room_leave(&self) {
        if let Some(session) = self.session() {
            session.room_detach();
        }
    }

    /// Removes another stream from the room (creator only, docs/70 D18).
    /// The broadcast stays in the room.
    pub fn room_remove(&self, broadcast_id: String) {
        if let Some(session) = self.session() {
            session.room_remove(&broadcast_id);
        }
    }

    /// Ends the room for everyone (creator only, docs/70 D18):
    /// [`BroadcastRoomEvent::Ended`] follows. The broadcast carries on.
    pub fn room_end(&self) {
        if let Some(session) = self.session() {
            session.room_end();
        }
    }

    /// Renames you in the room, and the tile your stream shows; a room
    /// joined later uses the new name too.
    pub fn room_set_nickname(&self, nickname: String) {
        if let Some(session) = self.session() {
            session.room_set_nickname(&nickname);
        }
    }

    /// Ends the broadcast cleanly; the relay keeps the code for its grace.
    pub fn stop(&self) {
        if let Some(tx) = self.stop.lock().unwrap().take() {
            let _ = tx.send(());
        }
    }
}

impl Drop for Broadcaster {
    fn drop(&mut self) {
        self.stop();
    }
}

/// What brings the publish session up: [`Session::start`] in the app, a
/// scripted relay in the tests.
type Started = Result<(Arc<Session>, mpsc::UnboundedReceiver<EngineEvent>), StartError>;

async fn run<C, F>(
    options: BroadcastOptions,
    listener: Arc<dyn BroadcastListener>,
    wiring: Wiring,
    connect: C,
) where
    C: FnOnce(SessionConfig, Arc<dyn Clock>) -> F,
    F: Future<Output = Started>,
{
    let Wiring {
        live,
        mut commands,
        mut stop,
    } = wiring;
    listener.on_status(BroadcastStatus::Connecting);
    let clock: Arc<dyn Clock> = Arc::new(MonotonicClock::new());
    // Telemetry (D23): "off" unless the user opted in, then the desktop's
    // resolution, so the pairing rule and the advertised-URL precedence are
    // the engine's, not restated here.
    let relay_raw = options.relay_url.clone();
    let telemetry_raw = if options.telemetry { "" } else { "off" };
    let reporter = Reporter::new(env!("CARGO_PKG_VERSION"), clock.clone());
    reporter.set_url(gawk_engine::config::effective_telemetry_url(
        &relay_raw,
        telemetry_raw,
        None,
    ));
    // `SessionConfig` has no creator token: a creator's rejoin is joined
    // once the session is up instead of from the config.
    let creator_rejoin = (!options.room_new
        && !options.room_code.is_empty()
        && !options.room_creator_token_hex.is_empty())
    .then(|| {
        (
            options.room_code.clone(),
            options.room_attach_secret.clone(),
            options.room_creator_token_hex.clone(),
        )
    });
    let cfg = SessionConfig {
        relay_url: options.relay_url.clone(),
        broadcast_id: options.broadcast_id.clone(),
        resume_token_hex: options.resume_token_hex.clone(),
        publish_secret: options.publish_secret.clone(),
        origin: gawk_engine::defaults::origin().to_owned(),
        insecure: options.insecure,
        room_code: if creator_rejoin.is_some() {
            String::new()
        } else {
            options.room_code.clone()
        },
        room_new: options.room_new,
        room_attach_secret: options.room_attach_secret.clone(),
        nickname: options.nickname.clone(),
        ..SessionConfig::default()
    };
    // A stop while the dial is still out (a slow relay, a lost handshake:
    // QUIC gives the dial no bound of its own) ends the broadcast now.
    let started = tokio::select! {
        started = connect(cfg, clock.clone()) => started,
        _ = &mut stop => {
            listener.on_status(BroadcastStatus::Ended { error: None, reclaim_status: None });
            return;
        }
    };
    let (session, mut events) = match started {
        Ok(s) => s,
        Err(e) => {
            listener.on_status(BroadcastStatus::Ended {
                error: Some(e.to_string()),
                reclaim_status: (e.status != 0).then_some(e.status),
            });
            return;
        }
    };
    if let Some((code, key, token)) = &creator_rejoin {
        session.room_join(code, key, token);
    }
    *live.lock().unwrap() = Some(Live {
        session: session.clone(),
        pipeline: Arc::new(Pipeline::new(
            session.sender(),
            tokio::runtime::Handle::current(),
            clock.clone(),
            options.quality.into(),
        )),
        quality: options.quality,
        retired: PipelineCounters::default(),
        upload: Upload::default(),
        room_left: false,
    });
    let mut code = String::new();
    let mut identity = Identity::default();
    // The code and token are both known: a reclaim can be built.
    let mut identified = false;
    let mut reclaim_status = None;
    let mut failure_check = tokio::time::interval(std::time::Duration::from_millis(250));
    // The engine's encoder and sent fps are windowed between `stats()`
    // calls, so the sample is taken once a second, as the desktop shells
    // do: at 250 ms a quiet quarter-second read as 0 fps.
    let mut report_tick = tokio::time::interval(std::time::Duration::from_secs(1));
    let mut capture_rate = CaptureRate::default();
    let mut upload_rate = UploadRate::default();
    let mut uplink = UplinkMonitor::new();
    // A quality restart's reclaim is in flight (docs/70 D12): its first
    // attempt is not narrated, and the watchdogs wait for its media.
    let mut restarting = false;
    // The latest quality asked for, applied once the code and token are
    // both known (a republish reclaims with them).
    let mut wanted_quality: Option<Quality> = None;
    loop {
        tokio::select! {
            _ = &mut stop => {
                session.stop().await;
                reporter.event("ended", "");
                reporter.finish();
                break;
            }
            Some(command) = commands.recv() => {
                let Command::SetQuality(quality) = command;
                wanted_quality = Some(quality);
            }
            _ = report_tick.tick() => {
                let Some((pipeline, counters)) = current(&live) else { continue };
                let st = session.stats();
                if !restarting {
                    let warning = uplink.observe(&st);
                    let bps = upload_rate.sample(media_bytes(&st), tokio::time::Instant::now());
                    if let Some(l) = live.lock().unwrap().as_mut() {
                        l.upload.warning = warning;
                        l.upload.bps = bps.or(l.upload.bps);
                    }
                }
                reporter.report(merged_stats(
                    st,
                    &counters,
                    pipeline.rung(),
                    capture_rate.sample(counters.pushed),
                    &options.capture_source,
                    pipeline.audio_state(),
                ));
                reporter.tick();
            }
            _ = failure_check.tick() => {
                let Some((pipeline, _)) = current(&live) else { continue };
                if let Some(why) = pipeline.take_failure() {
                    reporter.event("error", &why);
                    reporter.finish();
                    listener.on_failure(why.clone());
                    session.stop().await;
                    listener.on_status(BroadcastStatus::Ended { error: Some(why), reclaim_status: None });
                    live.lock().unwrap().take();
                    return;
                }
            }
            event = events.recv() => {
                let Some(event) = event else { break };
                if let Some(room_event) = room_event(&event) {
                    if room_left(&live) {
                        continue;
                    }
                    // docs/60 D10: a broadcaster isn't left in a room its
                    // stream is no longer part of.
                    if matches!(room_event, BroadcastRoomEvent::Detached { by_creator: true, .. }) {
                        session.room_leave();
                        if let Some(l) = live.lock().unwrap().as_mut() {
                            l.room_left = true;
                        }
                    }
                    listener.on_room_event(room_event);
                    continue;
                }
                match event {
                    EngineEvent::Announce { broadcast_id } => {
                        code = broadcast_id.clone();
                        if let Some((c, t)) = identity.on_announce(&broadcast_id) {
                            identified = true;
                            listener.on_identity(c, t);
                        }
                        listener.on_status(BroadcastStatus::Live {
                            join_link: gawk_engine::join_link(gawk_engine::defaults::APP_URL, &broadcast_id),
                            code: broadcast_id,
                        });
                    }
                    EngineEvent::ResumeToken { token_hex } => {
                        if let Some((c, t)) = identity.on_token(token_hex) {
                            identified = true;
                            listener.on_identity(c, t);
                        }
                    }
                    EngineEvent::ViewerCount(n) => listener.on_viewer_count(n),
                    EngineEvent::Resuming { attempt } => {
                        reporter.event("resuming", "");
                        // docs/64 OD4: the restart's own reclaim is not
                        // narrated; a second attempt means it is really
                        // reconnecting, and says so.
                        if !(restarting && attempt == 1) {
                            listener.on_status(BroadcastStatus::Resuming { attempt });
                        }
                    }
                    EngineEvent::TelemetryHello { enabled, report_interval_ms, token, broadcast_key_hex } => {
                        reporter.begin(&Hello { enabled, report_interval_ms, token, broadcast_key_hex });
                    }
                    EngineEvent::TelemetryEndpoint { url } => {
                        reporter.set_url(gawk_engine::config::effective_telemetry_url(
                            &relay_raw,
                            telemetry_raw,
                            Some(&url),
                        ));
                    }
                    EngineEvent::Resumed => {
                        reporter.event("resumed", "");
                        restarting = false;
                        // Re-prime the relay's invalidated keyframe cache.
                        if let Some((pipeline, _)) = current(&live) {
                            pipeline.force_idr();
                        }
                        if !code.is_empty() {
                            listener.on_status(BroadcastStatus::Live {
                                join_link: gawk_engine::join_link(gawk_engine::defaults::APP_URL, &code),
                                code: code.clone(),
                            });
                        }
                    }
                    EngineEvent::ReclaimRefused { status } => reclaim_status = Some(status),
                    EngineEvent::Ended { error } => {
                        match &error {
                            Some(e) => reporter.event("error", e),
                            None => reporter.event("ended", ""),
                        }
                        reporter.finish();
                        listener.on_status(BroadcastStatus::Ended { error, reclaim_status });
                        live.lock().unwrap().take();
                        return;
                    }
                    EngineEvent::RoomState(_) if room_left(&live) => {}
                    EngineEvent::RoomState(s) => {
                        let attached = !code.is_empty() && s.has(&code);
                        let needs_key = room_needs_key(&s, attached);
                        listener.on_room_state(room_view(s), attached, needs_key);
                    }
                    _ => {}
                }
            }
        }
        // A quality asked for, now or before the code and token arrived.
        if identified
            && let Some(quality) = wanted_quality.take()
            && restart(&live, &clock, quality)
        {
            reporter.event("restart", "");
            restarting = true;
            // The watchdogs start over with the new media, as the desktop's
            // do.
            uplink = UplinkMonitor::new();
            upload_rate = UploadRate::default();
        }
    }
    listener.on_status(BroadcastStatus::Ended {
        error: None,
        reclaim_status,
    });
    live.lock().unwrap().take();
}

fn room_left(live: &Mutex<Option<Live>>) -> bool {
    live.lock().unwrap().as_ref().is_some_and(|l| l.room_left)
}

/// The live pipeline and the broadcast's counters so far.
fn current(live: &Mutex<Option<Live>>) -> Option<(Arc<Pipeline>, PipelineCounters)> {
    let live = live.lock().unwrap();
    let l = live.as_ref()?;
    Some((l.pipeline.clone(), l.counters()))
}

/// docs/70 D12's quality change, the desktop's quick restart (`shell.rs`
/// `republish`, docs/64 D8): a new pipeline at `quality` replaces the live
/// one, then a fresh publish leg under the same identity. `false` when
/// there is nothing to change.
fn restart(live: &Mutex<Option<Live>>, clock: &Arc<dyn Clock>, quality: Quality) -> bool {
    let mut guard = live.lock().unwrap();
    let Some(l) = guard.as_mut() else {
        return false;
    };
    if l.quality == quality {
        return false;
    }
    // In this order: the old pipeline stops feeding the sender, the sender
    // forgets its codec and audio format, and only then is the new pipeline
    // there for Swift to push to, so the format it names is the one kept.
    l.pipeline.close();
    l.retired = fold(&l.retired, &l.pipeline.counters());
    let sender = l.session.sender();
    sender.new_lineage();
    l.pipeline = Arc::new(Pipeline::new(
        sender,
        tokio::runtime::Handle::current(),
        clock.clone(),
        quality.into(),
    ));
    l.quality = quality;
    l.upload.warning = false;
    let session = l.session.clone();
    drop(guard);
    session.republish();
    true
}

/// `base` plus what `current` counted; the size is `current`'s.
fn fold(base: &PipelineCounters, current: &PipelineCounters) -> PipelineCounters {
    PipelineCounters {
        pushed: base.pushed + current.pushed,
        admitted: base.admitted + current.admitted,
        dropped_no_content: base.dropped_no_content + current.dropped_no_content,
        dropped_over_rate: base.dropped_over_rate + current.dropped_over_rate,
        dropped_backpressure: base.dropped_backpressure + current.dropped_backpressure,
        dropped_no_encoder: base.dropped_no_encoder + current.dropped_no_encoder,
        encoder_restarts: base.encoder_restarts + current.encoder_restarts,
        encoded: base.encoded + current.encoded,
        width: current.width,
        height: current.height,
    }
}

fn broadcast_counters(
    c: PipelineCounters,
    rung: Option<Rung>,
    upload: Upload,
) -> BroadcastCounters {
    BroadcastCounters {
        pushed: c.pushed,
        admitted: c.admitted,
        dropped_no_content: c.dropped_no_content,
        dropped_over_rate: c.dropped_over_rate,
        dropped_backpressure: c.dropped_backpressure,
        encoded: c.encoded,
        width: c.width,
        height: c.height,
        fps: rung.map_or(0, |r| r.fps),
        peak_bitrate_bps: rung.map_or(0, |r| r.peak_bitrate_bps),
        upload_available: upload.bps.is_some(),
        upload_bps: upload.bps.unwrap_or(0),
        uplink_warning: upload.warning,
    }
}

/// The media bytes the Upload row counts. The sender adds keyframe-stream
/// and audio bytes to `bytes_sent` as well as to their own counters, so
/// they are not added again; parity is the one kind it keeps apart.
fn media_bytes(st: &Stats) -> u64 {
    st.bytes_sent + st.parity_bytes_sent
}

/// The room events Swift hears as [`BroadcastRoomEvent`]s.
fn room_event(e: &EngineEvent) -> Option<BroadcastRoomEvent> {
    Some(match e {
        EngineEvent::RoomCreated {
            code,
            creator_token_hex,
        } => BroadcastRoomEvent::Created {
            code: code.clone(),
            creator_token_hex: creator_token_hex.clone(),
        },
        EngineEvent::RoomAttached => BroadcastRoomEvent::Attached,
        EngineEvent::RoomDetached { reason, by_creator } => BroadcastRoomEvent::Detached {
            reason: reason.clone(),
            by_creator: *by_creator,
        },
        EngineEvent::RoomEnded { reason } => BroadcastRoomEvent::Ended {
            reason: reason.clone(),
        },
        EngineEvent::RoomRejected { reason, message } => BroadcastRoomEvent::Rejected {
            reason: reason.clone(),
            message: message.clone(),
        },
        EngineEvent::RoomReconnecting { attempt } => {
            BroadcastRoomEvent::Reconnecting { attempt: *attempt }
        }
        _ => return None,
    })
}

/// A gated static room admitted the broadcaster as a watcher only: the app
/// asks for its key (docs/60 D8). The desktop's rule (`shell.rs`).
fn room_needs_key(s: &RoomSummary, attached: bool) -> bool {
    !s.attach_ok && !s.dynamic && !attached
}

/// The host clock the capture's PTS are on, in 100 ns, for the test source
/// (D27) to stamp its frames like ScreenCaptureKit would.
#[uniffi::export]
pub fn host_time_100ns() -> i64 {
    gawk_capture::host::now_100ns()
}

/// What the iOS pipeline knows that the engine's counters can't, merged
/// into the telemetry sample as the desktop shell's `merged_stats` does:
/// the rung, the encoder, the capture path and rate, audio, and encoders
/// rebuilt mid-session.
fn merged_stats(
    mut st: Stats,
    counters: &PipelineCounters,
    rung: Option<Rung>,
    capture_fps: Option<f64>,
    capture_source: &str,
    audio_state: &str,
) -> Stats {
    if let Some(r) = rung {
        st.encoder = "VideoToolbox H.264".into();
        st.width = r.width;
        st.height = r.height;
        st.fps = r.fps;
        st.bitrate_bps = r.peak_bitrate_bps;
    }
    if let Some(f) = capture_fps {
        st.capture_fps_available = true;
        st.capture_fps = f;
    }
    st.capture_path = capture_source.to_owned();
    st.audio_state = audio_state.to_owned();
    st.capture_restarts = counters.encoder_restarts;
    st
}

/// Frames the capture pushed per second, windowed between reports.
#[derive(Default)]
struct CaptureRate {
    last: Option<(std::time::Instant, u64)>,
}

impl CaptureRate {
    fn sample(&mut self, pushed: u64) -> Option<f64> {
        let now = std::time::Instant::now();
        let rate = self.last.and_then(|(t, n)| {
            let dt = now.duration_since(t).as_secs_f64();
            (dt > 0.0).then(|| pushed.saturating_sub(n) as f64 / dt)
        });
        self.last = Some((now, pushed));
        rate
    }
}

/// The upload rate in bits per second, windowed between reports.
#[derive(Default)]
struct UploadRate {
    last: Option<(tokio::time::Instant, u64)>,
}

impl UploadRate {
    fn sample(&mut self, bytes: u64, now: tokio::time::Instant) -> Option<u64> {
        let rate = self.last.and_then(|(t, n)| {
            let dt = now.duration_since(t).as_secs_f64();
            (dt > 0.0).then(|| (bytes.saturating_sub(n) as f64 * 8.0 / dt).round() as u64)
        });
        self.last = Some((now, bytes));
        rate
    }
}

#[cfg(test)]
mod stats_tests {
    use super::*;

    #[test]
    fn the_sample_carries_what_only_the_pipeline_knows() {
        let counters = PipelineCounters {
            encoder_restarts: 2,
            ..Default::default()
        };
        let rung = Rung {
            width: 884,
            height: 1920,
            fps: 60,
            peak_bitrate_bps: 8_000_000,
        };
        let st = merged_stats(
            Stats::default(),
            &counters,
            Some(rung),
            Some(59.5),
            "screencapturekit",
            "active",
        );
        assert_eq!(st.encoder, "VideoToolbox H.264");
        assert_eq!(
            (st.width, st.height, st.fps, st.bitrate_bps),
            (884, 1920, 60, 8_000_000)
        );
        assert!(st.capture_fps_available);
        assert_eq!(st.capture_fps, 59.5);
        assert_eq!(st.capture_path, "screencapturekit");
        assert_eq!(st.audio_state, "active");
        // A rebuilt encoder is a freeze a viewer saw, as a desktop capture
        // rebuild is (Stats::capture_restarts).
        assert_eq!(st.capture_restarts, 2);
    }

    #[test]
    fn before_the_first_frame_nothing_is_invented() {
        let st = merged_stats(
            Stats::default(),
            &PipelineCounters::default(),
            None,
            None,
            "",
            "off",
        );
        assert_eq!((st.width, st.height), (0, 0));
        assert!(!st.capture_fps_available);
        assert_eq!(st.audio_state, "off");
    }

    /// docs/70 D11's Quality and Upload rows: the rung's rate and cap, and
    /// the upload reading, beside the frame counts.
    #[test]
    fn the_counters_carry_the_rung_and_the_upload_reading() {
        let c = PipelineCounters {
            pushed: 10,
            encoded: 9,
            width: 588,
            height: 1280,
            ..Default::default()
        };
        let rung = Rung {
            width: 588,
            height: 1280,
            fps: 30,
            peak_bitrate_bps: 3_000_000,
        };
        let got = broadcast_counters(
            c,
            Some(rung),
            Upload {
                bps: Some(2_400_000),
                warning: true,
            },
        );
        assert_eq!((got.pushed, got.encoded), (10, 9));
        assert_eq!((got.width, got.height), (588, 1280));
        assert_eq!((got.fps, got.peak_bitrate_bps), (30, 3_000_000));
        assert!(got.upload_available && got.uplink_warning);
        assert_eq!(got.upload_bps, 2_400_000);

        let before = broadcast_counters(PipelineCounters::default(), None, Upload::default());
        assert_eq!((before.fps, before.peak_bitrate_bps), (0, 0));
        assert!(!before.upload_available);
    }

    /// CODE-REVIEW: counters survive their owner. A quality change retires
    /// a pipeline; what it counted stays in the totals.
    #[test]
    fn a_retired_pipeline_s_counts_stay_in_the_totals() {
        let retired = PipelineCounters {
            pushed: 100,
            admitted: 90,
            dropped_no_content: 5,
            dropped_over_rate: 3,
            dropped_backpressure: 2,
            dropped_no_encoder: 1,
            encoder_restarts: 1,
            encoded: 88,
            width: 884,
            height: 1920,
        };
        let now = PipelineCounters {
            pushed: 10,
            admitted: 9,
            encoded: 8,
            width: 588,
            height: 1280,
            ..Default::default()
        };
        let t = fold(&retired, &now);
        assert_eq!((t.pushed, t.admitted, t.encoded), (110, 99, 96));
        assert_eq!(
            (
                t.dropped_no_content,
                t.dropped_over_rate,
                t.dropped_backpressure,
                t.dropped_no_encoder,
                t.encoder_restarts
            ),
            (5, 3, 2, 1, 1)
        );
        assert_eq!((t.width, t.height), (588, 1280), "the size in force");
    }

    #[test]
    fn the_upload_rate_needs_two_samples_and_counts_bits_per_second() {
        let mut r = UploadRate::default();
        let t0 = tokio::time::Instant::now();
        assert_eq!(r.sample(1_000, t0), None);
        let t1 = t0 + std::time::Duration::from_secs(1);
        assert_eq!(r.sample(301_000, t1), Some(2_400_000));
        // A longer window is divided by its length.
        let t2 = t1 + std::time::Duration::from_secs(2);
        assert_eq!(r.sample(601_000, t2), Some(1_200_000));
    }

    /// `bytes_sent` already holds the keyframe-stream and audio bytes, so
    /// adding `audio_bytes_sent` again would count audio twice.
    #[test]
    fn media_bytes_count_audio_once_and_parity_too() {
        let st = Stats {
            bytes_sent: 10_000,
            keyframe_bytes_sent: 6_000,
            audio_bytes_sent: 1_000,
            parity_bytes_sent: 500,
            ..Default::default()
        };
        assert_eq!(media_bytes(&st), 10_500);
    }

    #[test]
    fn a_gated_static_room_asks_for_its_key_until_attached() {
        let s = RoomSummary {
            attach_ok: false,
            dynamic: false,
            ..Default::default()
        };
        assert!(room_needs_key(&s, false));
        assert!(!room_needs_key(&s, true), "attached: nothing to ask");
        let dynamic = RoomSummary {
            dynamic: true,
            ..s.clone()
        };
        assert!(!room_needs_key(&dynamic, false), "minted rooms have no key");
        let open = RoomSummary {
            attach_ok: true,
            ..s
        };
        assert!(!room_needs_key(&open, false));
    }

    #[test]
    fn room_events_reach_swift_and_nothing_else_does() {
        let cases = [
            (
                EngineEvent::RoomCreated {
                    code: "QX7P2K".into(),
                    creator_token_hex: "5a".into(),
                },
                BroadcastRoomEvent::Created {
                    code: "QX7P2K".into(),
                    creator_token_hex: "5a".into(),
                },
            ),
            (EngineEvent::RoomAttached, BroadcastRoomEvent::Attached),
            (
                EngineEvent::RoomDetached {
                    reason: "r".into(),
                    by_creator: true,
                },
                BroadcastRoomEvent::Detached {
                    reason: "r".into(),
                    by_creator: true,
                },
            ),
            (
                EngineEvent::RoomEnded { reason: "e".into() },
                BroadcastRoomEvent::Ended { reason: "e".into() },
            ),
            (
                EngineEvent::RoomRejected {
                    reason: "r".into(),
                    message: "m".into(),
                },
                BroadcastRoomEvent::Rejected {
                    reason: "r".into(),
                    message: "m".into(),
                },
            ),
            (
                EngineEvent::RoomReconnecting { attempt: 2 },
                BroadcastRoomEvent::Reconnecting { attempt: 2 },
            ),
        ];
        for (engine, want) in cases {
            assert_eq!(room_event(&engine), Some(want));
        }
        assert_eq!(room_event(&EngineEvent::Resumed), None);
        assert_eq!(
            room_event(&EngineEvent::RoomState(RoomSummary::default())),
            None,
            "the picture has its own callback"
        );
    }
}

#[cfg(test)]
mod tests {
    use super::Identity;

    #[test]
    fn a_token_after_the_announce_is_paired_with_the_code() {
        let mut id = Identity::default();
        assert_eq!(id.on_announce("AB2CD3"), None);
        assert_eq!(
            id.on_token("aa".into()),
            Some(("AB2CD3".into(), "aa".into()))
        );
    }

    #[test]
    fn a_token_before_the_announce_is_kept_until_the_code_arrives() {
        let mut id = Identity::default();
        assert_eq!(id.on_token("aa".into()), None);
        assert_eq!(
            id.on_announce("AB2CD3"),
            Some(("AB2CD3".into(), "aa".into()))
        );
    }

    #[test]
    fn a_resumed_session_s_new_token_replaces_the_stored_one() {
        let mut id = Identity::default();
        id.on_announce("AB2CD3");
        id.on_token("aa".into());
        assert_eq!(
            id.on_token("bb".into()),
            Some(("AB2CD3".into(), "bb".into()))
        );
    }
}

/// "Stop broadcasting" must end the broadcast whatever the relay is doing:
/// the UI leaves its live screen only on `Ended`.
#[cfg(test)]
mod stop_tests {
    use super::*;
    use gawk_engine::media::AccessUnit;
    use gawk_engine::relay::{
        BoxFuture, KeyframeWriter, RelaySession, SendDatagramError, ServerStream, SessionClose,
    };
    use std::time::Duration;

    /// A relay that accepts the session and datagrams but never grants a
    /// keyframe stream: an open that waits on QUIC credit (stream count or
    /// connection flow control on a saturated uplink) for as long as the
    /// connection lives.
    struct StalledOpenRelay;

    impl RelaySession for StalledOpenRelay {
        fn send_datagram(&self, _: &[u8]) -> Result<(), SendDatagramError> {
            Ok(())
        }
        fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
            Box::pin(std::future::pending())
        }
        fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
            Box::pin(std::future::pending())
        }
        fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
            Box::pin(std::future::pending())
        }
        fn closed(&self) -> BoxFuture<'_, SessionClose> {
            Box::pin(std::future::pending())
        }
    }

    struct Statuses(mpsc::UnboundedSender<BroadcastStatus>);

    impl BroadcastListener for Statuses {
        fn on_status(&self, status: BroadcastStatus) {
            let _ = self.0.send(status);
        }
        fn on_identity(&self, _: String, _: String) {}
        fn on_viewer_count(&self, _: u32) {}
        fn on_room_state(&self, _: RoomView, _: bool, _: bool) {}
        fn on_room_event(&self, _: BroadcastRoomEvent) {}
        fn on_failure(&self, _: String) {}
    }

    pub(super) fn options() -> BroadcastOptions {
        BroadcastOptions {
            relay_url: "https://127.0.0.1:9".into(),
            publish_secret: String::new(),
            broadcast_id: String::new(),
            resume_token_hex: String::new(),
            quality: Quality::Standard,
            room_code: String::new(),
            room_attach_secret: String::new(),
            nickname: String::new(),
            insecure: true,
            telemetry: false,
            capture_source: String::new(),
            room_new: false,
            room_creator_token_hex: String::new(),
        }
    }

    /// Whether `Ended` arrives. Time is paused, so a run that can make no
    /// more progress fails at once rather than after the 30 s.
    async fn ended(rx: &mut mpsc::UnboundedReceiver<BroadcastStatus>) -> bool {
        tokio::time::timeout(Duration::from_secs(30), async {
            while let Some(s) = rx.recv().await {
                if matches!(s, BroadcastStatus::Ended { .. }) {
                    return true;
                }
            }
            false
        })
        .await
        .unwrap_or(false)
    }

    #[tokio::test(start_paused = true)]
    async fn stop_ends_the_broadcast_while_a_keyframe_stream_cannot_open() {
        let (tx, mut rx) = mpsc::unbounded_channel();
        let (b, wiring) = Broadcaster::wired();
        let (session_tx, session_rx) = oneshot::channel();
        tokio::spawn(run(
            options(),
            Arc::new(Statuses(tx)),
            wiring,
            move |cfg, clock| async move {
                let (s, ev) = Session::start_with_session(cfg, Arc::new(StalledOpenRelay), clock);
                let _ = session_tx.send(s.clone());
                Ok((s, ev))
            },
        ));
        let session = session_rx.await.unwrap();
        // A keyframe whose stream never opens: what a big IDR can meet on
        // a saturated uplink.
        session
            .sender()
            .send_video(AccessUnit {
                data: vec![0; 64],
                timestamp_us: 1,
                keyframe: true,
            })
            .await;
        tokio::time::sleep(Duration::from_millis(100)).await;

        b.stop();
        assert!(ended(&mut rx).await, "Stop never ended the broadcast");
    }

    #[tokio::test(start_paused = true)]
    async fn stop_while_connecting_ends_the_broadcast() {
        let (tx, mut rx) = mpsc::unbounded_channel();
        let (b, wiring) = Broadcaster::wired();
        tokio::spawn(run(options(), Arc::new(Statuses(tx)), wiring, |_, _| {
            std::future::pending::<Started>()
        }));
        tokio::time::sleep(Duration::from_millis(100)).await;
        // Nothing is up: the quality is the next start's.
        b.set_quality(Quality::Cellular);
        b.stop();
        assert!(ended(&mut rx).await, "Stop while connecting never ended it");
    }
}

/// The live broadcast against a scripted relay (the engine's seams): the
/// quality restart (docs/70 D12), the Upload row (D11), and the room
/// (D16–D18).
#[cfg(test)]
mod live_tests {
    use super::stop_tests::options;
    use super::*;
    use gawk_engine::media::AccessUnit;
    use gawk_engine::relay::{
        BoxFuture, CancelSignal, KeyframeOutcome, KeyframeWriter, PublishDialer, RelaySession,
        SendDatagramError, ServerStream, SessionClose, StartPhase,
    };
    use gawk_engine::room::{RoomConn, RoomDialer};
    use gawk_wire as wire;
    use std::collections::VecDeque;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::time::Duration;
    use tokio::sync::watch;

    const CODE: &str = "ABC234";
    const TOKEN: &str = "00112233445566778899aabbccddeeff";

    struct InstantWriter;

    impl KeyframeWriter for InstantWriter {
        fn write(
            self: Box<Self>,
            _: Vec<u8>,
            _: CancelSignal,
        ) -> BoxFuture<'static, KeyframeOutcome> {
            Box::pin(async { KeyframeOutcome::Sent })
        }
        fn abort(self: Box<Self>, _: u32) {}
    }

    struct Message(Option<Vec<u8>>);

    impl ServerStream for Message {
        fn read_to_end(&mut self, _: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
            let msg = self.0.take().unwrap_or_default();
            Box::pin(async move { Ok(msg) })
        }
    }

    /// One publish leg: announces the code and token, then serves until it
    /// is closed or killed.
    struct Leg {
        streams: Mutex<VecDeque<Vec<u8>>>,
        closed: AtomicBool,
        fail_datagrams: AtomicBool,
        death: watch::Sender<Option<SessionClose>>,
    }

    impl Leg {
        fn announcing() -> Arc<Self> {
            let mut announce = Vec::new();
            wire::append_broadcast_announce(&mut announce, CODE).unwrap();
            let mut token = Vec::new();
            let raw = gawk_engine::room::hex_decode(TOKEN).unwrap();
            wire::append_resume_token(&mut token, &raw).unwrap();
            Arc::new(Self {
                streams: Mutex::new(VecDeque::from([announce, token])),
                closed: AtomicBool::new(false),
                fail_datagrams: AtomicBool::new(false),
                death: watch::channel(None).0,
            })
        }

        fn kill(&self, cause: SessionClose) {
            self.death.send_replace(Some(cause));
        }
    }

    impl RelaySession for Leg {
        fn send_datagram(&self, _: &[u8]) -> Result<(), SendDatagramError> {
            if self.fail_datagrams.load(Ordering::SeqCst) {
                return Err(SendDatagramError::Failed("saturated".into()));
            }
            Ok(())
        }
        fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
            Box::pin(async { Ok(Box::new(InstantWriter) as Box<dyn KeyframeWriter>) })
        }
        fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
            Box::pin(async move {
                let next = self.streams.lock().unwrap().pop_front();
                match next {
                    Some(msg) => Ok(Box::new(Message(Some(msg))) as Box<dyn ServerStream>),
                    None => std::future::pending().await,
                }
            })
        }
        fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
            Box::pin(std::future::pending())
        }
        fn closed(&self) -> BoxFuture<'_, SessionClose> {
            let mut death = self.death.subscribe();
            Box::pin(async move {
                loop {
                    if let Some(cause) = death.borrow_and_update().clone() {
                        return cause;
                    }
                    if death.changed().await.is_err() {
                        return std::future::pending().await;
                    }
                }
            })
        }
        fn close(&self) {
            self.closed.store(true, Ordering::SeqCst);
        }
    }

    /// The reclaim dialer: records every URL and hands out the scripted legs.
    #[derive(Default)]
    struct Reclaims {
        urls: Mutex<Vec<String>>,
        legs: Mutex<VecDeque<Arc<Leg>>>,
    }

    impl PublishDialer for Reclaims {
        fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RelaySession>, StartError>> {
            self.urls.lock().unwrap().push(url.to_owned());
            let leg = self.legs.lock().unwrap().pop_front();
            Box::pin(async move {
                leg.map(|l| l as Arc<dyn RelaySession>).ok_or(StartError {
                    phase: StartPhase::Connect,
                    status: 0,
                    message: "no scripted leg".into(),
                })
            })
        }
    }

    // --- the room's end of the control stream -----------------------------

    struct RoomPipe {
        inbox: tokio::sync::Mutex<(mpsc::UnboundedReceiver<Vec<u8>>, Vec<u8>)>,
        outbox: mpsc::UnboundedSender<Vec<u8>>,
    }

    impl RoomConn for RoomPipe {
        fn write(&self, record: &[u8]) -> BoxFuture<'_, Result<(), String>> {
            let r = self
                .outbox
                .send(record.to_vec())
                .map_err(|_| "closed".to_string());
            Box::pin(async move { r })
        }
        fn read(&self, n: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
            Box::pin(async move {
                let mut g = self.inbox.lock().await;
                while g.1.len() < n {
                    match g.0.recv().await {
                        Some(chunk) => g.1.extend_from_slice(&chunk),
                        None => return Err("closed".into()),
                    }
                }
                Ok(g.1.drain(..n).collect())
            })
        }
        fn closed(&self) -> BoxFuture<'_, SessionClose> {
            Box::pin(std::future::pending())
        }
    }

    /// The relay's end of one room control session.
    struct RoomRelay {
        to_client: mpsc::UnboundedSender<Vec<u8>>,
        from_client: mpsc::UnboundedReceiver<Vec<u8>>,
    }

    impl RoomRelay {
        fn send(&self, msg: &[u8]) {
            let mut rec = Vec::new();
            wire::append_room_record(&mut rec, msg).unwrap();
            self.to_client.send(rec).unwrap();
        }
        fn send_state(&self, s: &wire::RoomState<'_>) {
            let mut msg = Vec::new();
            wire::append_room_state(&mut msg, s).unwrap();
            self.send(&msg);
        }
        fn send_event(&self, e: &wire::RoomEvent<'_>) {
            let mut msg = Vec::new();
            wire::append_room_event(&mut msg, e).unwrap();
            self.send(&msg);
        }
        /// The next record the client wrote, header stripped.
        async fn next(&mut self) -> Vec<u8> {
            let rec = tokio::time::timeout(Duration::from_secs(5), self.from_client.recv())
                .await
                .expect("a client record")
                .expect("the stream is open");
            rec[wire::ROOM_RECORD_HEADER_SIZE..].to_vec()
        }
    }

    #[derive(Default)]
    struct Rooms {
        urls: Mutex<Vec<String>>,
        conns: Mutex<VecDeque<Arc<dyn RoomConn>>>,
    }

    impl Rooms {
        fn with_one() -> (Arc<Self>, RoomRelay) {
            let (to_client, inbox) = mpsc::unbounded_channel();
            let (outbox, from_client) = mpsc::unbounded_channel();
            let conn: Arc<dyn RoomConn> = Arc::new(RoomPipe {
                inbox: tokio::sync::Mutex::new((inbox, Vec::new())),
                outbox,
            });
            let rooms = Arc::new(Self::default());
            rooms.conns.lock().unwrap().push_back(conn);
            (
                rooms,
                RoomRelay {
                    to_client,
                    from_client,
                },
            )
        }
    }

    impl RoomDialer for Rooms {
        fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RoomConn>, StartError>> {
            self.urls.lock().unwrap().push(url.to_owned());
            let conn = self.conns.lock().unwrap().pop_front();
            Box::pin(async move {
                conn.ok_or(StartError {
                    phase: StartPhase::Connect,
                    status: 0,
                    message: "no scripted room".into(),
                })
            })
        }
    }

    // --- the listener and the harness -------------------------------------

    #[derive(Debug, Clone, PartialEq)]
    enum Heard {
        Status(BroadcastStatus),
        Identity(String, String),
        RoomState(RoomView, bool, bool),
        RoomEvent(BroadcastRoomEvent),
    }

    struct Ears(mpsc::UnboundedSender<Heard>);

    impl BroadcastListener for Ears {
        fn on_status(&self, status: BroadcastStatus) {
            let _ = self.0.send(Heard::Status(status));
        }
        fn on_identity(&self, code: String, token: String) {
            let _ = self.0.send(Heard::Identity(code, token));
        }
        fn on_viewer_count(&self, _: u32) {}
        fn on_room_state(&self, room: RoomView, attached: bool, needs_key: bool) {
            let _ = self.0.send(Heard::RoomState(room, attached, needs_key));
        }
        fn on_room_event(&self, event: BroadcastRoomEvent) {
            let _ = self.0.send(Heard::RoomEvent(event));
        }
        fn on_failure(&self, _: String) {}
    }

    struct Harness {
        b: Arc<Broadcaster>,
        heard: mpsc::UnboundedReceiver<Heard>,
    }

    impl Harness {
        fn start(
            options: BroadcastOptions,
            first: Arc<Leg>,
            reclaims: Arc<Reclaims>,
            rooms: Arc<Rooms>,
        ) -> Self {
            let (tx, heard) = mpsc::unbounded_channel();
            let (b, wiring) = Broadcaster::wired();
            tokio::spawn(run(
                options,
                Arc::new(Ears(tx)),
                wiring,
                move |cfg, clock| async move {
                    Ok(Session::start_with_seams(
                        cfg, first, clock, rooms, reclaims,
                    ))
                },
            ));
            Self { b, heard }
        }

        /// Everything heard until `want` matches, `want`'s match last.
        async fn until(&mut self, want: impl Fn(&Heard) -> bool) -> Vec<Heard> {
            let mut seen = Vec::new();
            loop {
                let h = tokio::time::timeout(Duration::from_secs(30), self.heard.recv())
                    .await
                    .expect("heard in time")
                    .expect("the listener is open");
                let done = want(&h);
                seen.push(h);
                if done {
                    return seen;
                }
            }
        }

        /// Live, with the code and token stored.
        async fn live(&mut self) {
            self.until(|h| matches!(h, Heard::Identity(..))).await;
        }

        fn session(&self) -> Arc<Session> {
            self.b.session().expect("the session is up")
        }

        fn pipeline(&self) -> Arc<Pipeline> {
            self.b
                .live
                .lock()
                .unwrap()
                .as_ref()
                .unwrap()
                .pipeline
                .clone()
        }

        async fn stop(mut self) {
            self.b.stop();
            self.until(|h| matches!(h, Heard::Status(BroadcastStatus::Ended { .. })))
                .await;
        }
    }

    fn statuses(heard: &[Heard]) -> Vec<BroadcastStatus> {
        heard
            .iter()
            .filter_map(|h| match h {
                Heard::Status(s) => Some(s.clone()),
                _ => None,
            })
            .collect()
    }

    // --- K3: quality while live (docs/70 D12) ------------------------------

    #[tokio::test(start_paused = true)]
    async fn a_quality_change_republishes_on_the_same_code_without_narration() {
        let first = Leg::announcing();
        let second = Leg::announcing();
        let reclaims = Arc::new(Reclaims::default());
        reclaims.legs.lock().unwrap().push_back(second.clone());
        let mut h = Harness::start(
            options(),
            first.clone(),
            reclaims.clone(),
            Arc::new(Rooms::default()),
        );
        h.live().await;
        let before = h.pipeline();

        // The quality in force changes nothing; another one restarts.
        h.b.set_quality(Quality::Standard);
        h.b.set_quality(Quality::Cellular);
        let heard = h
            .until(|h| matches!(h, Heard::Status(BroadcastStatus::Live { .. })))
            .await;
        assert_eq!(
            statuses(&heard),
            [BroadcastStatus::Live {
                code: CODE.into(),
                join_link: format!("https://gawk.ioio.fi/#/view/{CODE}"),
            }],
            "the badge stays LIVE: no Resuming, the same code"
        );
        let urls = reclaims.urls.lock().unwrap().clone();
        assert_eq!(urls.len(), 1, "one restart: {urls:?}");
        assert!(
            urls[0].contains(&format!("/publish/{CODE}?"))
                && urls[0].contains(&format!("resume={TOKEN}")),
            "the same code, reclaimed with its token: {}",
            urls[0]
        );
        assert!(
            first.closed.load(Ordering::SeqCst),
            "the old leg closes cleanly"
        );
        assert!(!Arc::ptr_eq(&before, &h.pipeline()), "a new pipeline");
        assert_eq!(
            h.b.live.lock().unwrap().as_ref().unwrap().quality,
            Quality::Cellular
        );
        assert_eq!(h.session().broadcast_id(), CODE);

        // A real loss afterwards is narrated as before.
        second.kill(SessionClose::Abrupt("idle".into()));
        let heard = h
            .until(|h| matches!(h, Heard::Status(BroadcastStatus::Resuming { .. })))
            .await;
        assert_eq!(
            statuses(&heard).last(),
            Some(&BroadcastStatus::Resuming { attempt: 1 })
        );
        h.stop().await;
    }

    /// A quality asked for before the code and token are known (Swift's
    /// Connecting, review of #475) is kept and applied once they are, not
    /// dropped while Swift shows it as chosen.
    #[tokio::test(start_paused = true)]
    async fn a_quality_change_before_the_code_applies_once_it_arrives() {
        let first = Leg::announcing();
        let reclaims = Arc::new(Reclaims::default());
        reclaims.legs.lock().unwrap().push_back(Leg::announcing());
        let mut h = Harness::start(
            options(),
            first.clone(),
            reclaims.clone(),
            Arc::new(Rooms::default()),
        );
        // Before the dial has even returned.
        h.b.set_quality(Quality::Cellular);
        h.live().await;
        for _ in 0..100 {
            if !reclaims.urls.lock().unwrap().is_empty() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        let urls = reclaims.urls.lock().unwrap().clone();
        assert_eq!(urls.len(), 1, "one restart, once identified: {urls:?}");
        assert!(urls[0].contains(&format!("resume={TOKEN}")), "{}", urls[0]);
        assert_eq!(
            h.b.live.lock().unwrap().as_ref().unwrap().quality,
            Quality::Cellular
        );
        h.stop().await;
    }

    // --- K4: the Upload row (docs/70 D11) ----------------------------------

    /// One 3000-byte delta every 100 ms for `secs` seconds.
    async fn send_media(session: &Session, secs: u64) {
        for i in 0..secs * 10 {
            session
                .sender()
                .send_video(AccessUnit {
                    data: vec![0; 3000],
                    timestamp_us: i * 100_000,
                    keyframe: false,
                })
                .await;
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }

    #[tokio::test(start_paused = true)]
    async fn the_upload_row_reads_the_send_rate_and_the_uplink_warning() {
        let first = Leg::announcing();
        let reclaims = Arc::new(Reclaims::default());
        reclaims.legs.lock().unwrap().push_back(Leg::announcing());
        let mut h = Harness::start(
            options(),
            first.clone(),
            reclaims,
            Arc::new(Rooms::default()),
        );
        h.live().await;
        assert!(!h.b.counters().upload_available, "one sample is no rate");

        send_media(&h.session(), 3).await;
        let c = h.b.counters();
        assert!(c.upload_available);
        // 10 frames of 3000 bytes a second, plus the datagram headers.
        assert!(
            (200_000..=300_000).contains(&c.upload_bps),
            "{} bps",
            c.upload_bps
        );
        assert!(!c.uplink_warning);

        // Every send failing: frames drop at send, and after the streak the
        // engine's watchdog warns.
        first.fail_datagrams.store(true, Ordering::SeqCst);
        send_media(&h.session(), 7).await;
        assert!(h.b.counters().uplink_warning, "the upload can't keep up");

        // A quality change starts the watchdog over, and keeps the rate
        // shown until the new media has its own.
        h.b.set_quality(Quality::Cellular);
        tokio::time::sleep(Duration::from_millis(10)).await;
        let c = h.b.counters();
        assert!(!c.uplink_warning, "the warning starts over");
        assert!(c.upload_available, "the last rate stays");
        h.stop().await;
    }

    // --- K2: the broadcaster's room (docs/70 D16–D18) ----------------------

    fn ours() -> wire::RoomAttachment<'static> {
        wire::RoomAttachment {
            broadcast_id: CODE.into(),
            label: "Sam",
            live: true,
            viewer_count: 2,
        }
    }

    fn sam() -> wire::RoomParticipant<'static> {
        wire::RoomParticipant {
            id: 7,
            kind: wire::ROOM_CLIENT_NATIVE,
            flags: wire::ROOM_PARTICIPANT_FLAG_STREAMING,
            nickname: "Sam",
            identity: "",
        }
    }

    /// A `?rt=` creator grant rejoins the room as its creator (docs/60 D8):
    /// joined after the session is up, since the config has no token. The
    /// room's picture reaches Swift with this broadcast's attachment, and
    /// the creator removing it takes the broadcaster out of the room.
    #[tokio::test(start_paused = true)]
    async fn a_creator_grant_rejoins_and_the_room_reaches_the_listener() {
        let token = "5a".repeat(16);
        let (rooms, mut relay) = Rooms::with_one();
        let mut h = Harness::start(
            BroadcastOptions {
                room_code: "lan-party".into(),
                room_creator_token_hex: token.clone(),
                nickname: "Sam".into(),
                ..options()
            },
            Leg::announcing(),
            Arc::new(Reclaims::default()),
            rooms.clone(),
        );
        h.live().await;
        let hello = relay.next().await;
        assert_eq!(wire::parse_room_hello(&hello).unwrap().nickname, "Sam");
        assert_eq!(
            *rooms.urls.lock().unwrap(),
            [format!(
                "https://127.0.0.1:9/room/lan-party?creator={token}"
            )]
        );

        relay.send_state(&wire::RoomState {
            flags: wire::ROOM_STATE_FLAG_CREATOR | wire::ROOM_STATE_FLAG_ATTACH_OK,
            seq: 1,
            your_id: 7,
            code: "lan-party",
            display_name: "LAN party",
            participants: vec![sam()],
            attachments: vec![ours()],
            ..wire::RoomState::default()
        });
        let heard = h.until(|h| matches!(h, Heard::RoomState(..))).await;
        let Some(Heard::RoomState(room, attached, needs_key)) = heard.last() else {
            unreachable!()
        };
        assert!(*attached && !*needs_key);
        assert!(room.creator && !room.dynamic);
        assert_eq!((room.code.as_str(), room.your_id), ("lan-party", 7));
        assert_eq!(room.people[0].nickname, "Sam");
        assert!(room.people[0].streaming);
        assert_eq!(room.tiles[0].broadcast_id, CODE);

        relay.send_event(&wire::RoomEvent {
            seq: 2,
            kind: wire::ROOM_EVENT_ATTACHMENT_REMOVED,
            attachment: wire::RoomAttachment {
                broadcast_id: CODE.into(),
                ..wire::RoomAttachment::default()
            },
            reason: wire::ROOM_DETACH_REASON_CREATOR,
            ..wire::RoomEvent::default()
        });
        h.until(|h| {
            matches!(
                h,
                Heard::RoomEvent(BroadcastRoomEvent::Detached {
                    by_creator: true,
                    ..
                })
            )
        })
        .await;
        // Out of the room: what the relay says next reaches no one.
        relay.send_event(&wire::RoomEvent {
            seq: 3,
            kind: wire::ROOM_EVENT_PARTICIPANT_JOINED,
            participant: wire::RoomParticipant {
                id: 9,
                nickname: "Mika",
                ..wire::RoomParticipant::default()
            },
            ..wire::RoomEvent::default()
        });
        tokio::time::sleep(Duration::from_millis(300)).await;
        while let Ok(heard) = h.heard.try_recv() {
            assert!(
                !matches!(heard, Heard::RoomState(..)),
                "a room state after leaving: {heard:?}"
            );
        }
        // Joining again is heard again (this relay has no second room).
        h.b.room_join("lan-party".into(), String::new(), String::new());
        h.until(|h| matches!(h, Heard::RoomEvent(BroadcastRoomEvent::Ended { .. })))
            .await;
        assert_eq!(rooms.urls.lock().unwrap().len(), 2);
        h.stop().await;
    }

    /// docs/70 D16's "Create a new room": minted once the broadcast has its
    /// code, and the code and creator token reach Swift.
    #[tokio::test(start_paused = true)]
    async fn a_new_room_is_minted_and_its_grant_reaches_the_listener() {
        let (rooms, relay) = Rooms::with_one();
        let mut h = Harness::start(
            BroadcastOptions {
                room_new: true,
                room_code: "ignored".into(),
                nickname: "Sam".into(),
                ..options()
            },
            Leg::announcing(),
            Arc::new(Reclaims::default()),
            rooms.clone(),
        );
        h.live().await;
        let creator_token = [0x5a; wire::ROOM_CREATOR_TOKEN_SIZE];
        relay.send_state(&wire::RoomState {
            flags: wire::ROOM_STATE_FLAG_DYNAMIC
                | wire::ROOM_STATE_FLAG_CREATOR
                | wire::ROOM_STATE_FLAG_ATTACH_OK,
            seq: 1,
            your_id: 1,
            code: "QX7P2K",
            creator_token: &creator_token,
            attachments: vec![ours()],
            ..wire::RoomState::default()
        });
        let heard = h.until(|h| matches!(h, Heard::RoomState(..))).await;
        assert!(
            heard.contains(&Heard::RoomEvent(BroadcastRoomEvent::Created {
                code: "QX7P2K".into(),
                creator_token_hex: "5a".repeat(16),
            }))
        );
        assert!(matches!(
            heard.last(),
            Some(Heard::RoomState(room, true, false)) if room.creator && room.dynamic
        ));
        assert_eq!(
            *rooms.urls.lock().unwrap(),
            [format!(
                "https://127.0.0.1:9/room/new?broadcast={CODE}&resume={TOKEN}&label=Sam"
            )]
        );
        h.stop().await;
    }
}
