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

use gawk_broadcast::audio::Asbd;
use gawk_broadcast::pipeline::Pipeline;
use gawk_broadcast::rotation::Rotation;
use gawk_broadcast::rung::Quality as RungQuality;
use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use gawk_engine::telemetry::{Hello, Reporter};
use std::sync::{Arc, Mutex};
use tokio::sync::oneshot;

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

/// Where frames went (the live status and diagnostics).
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
}

/// Implemented in Swift; called on the broadcaster's thread.
#[uniffi::export(with_foreign)]
pub trait BroadcastListener: Send + Sync {
    fn on_status(&self, status: BroadcastStatus);
    /// Persist for a reclaim within the grace (D17: Keychain).
    fn on_identity(&self, code: String, resume_token_hex: String);
    fn on_viewer_count(&self, count: u32);
    /// The room attach, in words for the live status (D21).
    fn on_room(&self, text: String);
    /// The pipeline's first video failure; the broadcast ends.
    fn on_failure(&self, text: String);
}

struct Live {
    session: Arc<Session>,
    pipeline: Arc<Pipeline>,
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
}

#[uniffi::export]
impl Broadcaster {
    /// Starts the publish session; media may be pushed once the status is
    /// [`BroadcastStatus::Live`] (pushes before then are dropped).
    #[uniffi::constructor]
    pub fn start(options: BroadcastOptions, listener: Arc<dyn BroadcastListener>) -> Arc<Self> {
        let live: Arc<Mutex<Option<Live>>> = Arc::default();
        let (stop_tx, stop_rx) = oneshot::channel();
        let this = Arc::new(Self {
            live: live.clone(),
            stop: Mutex::new(Some(stop_tx)),
        });
        std::thread::Builder::new()
            .name("gawk-broadcast".into())
            .spawn(move || {
                let rt = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("a tokio runtime");
                rt.block_on(run(options, listener, live, stop_rx));
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

    /// Where frames went so far; zeros before the session is up.
    pub fn counters(&self) -> BroadcastCounters {
        let c = self
            .live
            .lock()
            .unwrap()
            .as_ref()
            .map(|l| l.pipeline.counters())
            .unwrap_or_default();
        BroadcastCounters {
            pushed: c.pushed,
            admitted: c.admitted,
            dropped_no_content: c.dropped_no_content,
            dropped_over_rate: c.dropped_over_rate,
            dropped_backpressure: c.dropped_backpressure,
            encoded: c.encoded,
            width: c.width,
            height: c.height,
        }
    }

    /// The network path changed (D19): reconnect with the resume token now
    /// instead of waiting out idle timeouts. QUIC migration is off (D12).
    pub fn path_changed(&self) {
        if let Some(l) = self.live.lock().unwrap().as_ref() {
            l.session.republish();
        }
    }

    /// The capture resumed after a pause (a call): re-prime with an IDR.
    pub fn force_idr(&self) {
        if let Some(l) = self.live.lock().unwrap().as_ref() {
            l.pipeline.force_idr();
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

async fn run(
    options: BroadcastOptions,
    listener: Arc<dyn BroadcastListener>,
    live: Arc<Mutex<Option<Live>>>,
    mut stop: oneshot::Receiver<()>,
) {
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
    let cfg = SessionConfig {
        relay_url: options.relay_url,
        broadcast_id: options.broadcast_id,
        resume_token_hex: options.resume_token_hex,
        publish_secret: options.publish_secret,
        origin: gawk_engine::defaults::origin().to_owned(),
        insecure: options.insecure,
        room_code: options.room_code,
        room_attach_secret: options.room_attach_secret,
        nickname: options.nickname,
        ..SessionConfig::default()
    };
    let (session, mut events) = match Session::start(cfg, clock.clone()).await {
        Ok(s) => s,
        Err(e) => {
            listener.on_status(BroadcastStatus::Ended {
                error: Some(e.to_string()),
                reclaim_status: (e.status != 0).then_some(e.status),
            });
            return;
        }
    };
    let pipeline = Arc::new(Pipeline::new(
        session.sender(),
        tokio::runtime::Handle::current(),
        clock,
        options.quality.into(),
    ));
    *live.lock().unwrap() = Some(Live {
        session: session.clone(),
        pipeline: pipeline.clone(),
    });
    let mut code = String::new();
    let mut identity = Identity::default();
    let mut reclaim_status = None;
    let mut failure_check = tokio::time::interval(std::time::Duration::from_millis(250));
    loop {
        tokio::select! {
            _ = &mut stop => {
                session.stop().await;
                reporter.event("ended", "");
                reporter.finish();
                break;
            }
            _ = failure_check.tick() => {
                reporter.report(session.stats());
                reporter.tick();
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
                match event {
                    EngineEvent::Announce { broadcast_id } => {
                        code = broadcast_id.clone();
                        if let Some((c, t)) = identity.on_announce(&broadcast_id) {
                            listener.on_identity(c, t);
                        }
                        listener.on_status(BroadcastStatus::Live {
                            join_link: gawk_engine::join_link(gawk_engine::defaults::APP_URL, &broadcast_id),
                            code: broadcast_id,
                        });
                    }
                    EngineEvent::ResumeToken { token_hex } => {
                        if let Some((c, t)) = identity.on_token(token_hex) {
                            listener.on_identity(c, t);
                        }
                    }
                    EngineEvent::ViewerCount(n) => listener.on_viewer_count(n),
                    EngineEvent::Resuming { attempt } => {
                        reporter.event("resuming", "");
                        listener.on_status(BroadcastStatus::Resuming { attempt });
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
                        // Re-prime the relay's invalidated keyframe cache.
                        pipeline.force_idr();
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
                    EngineEvent::RoomAttached => listener.on_room("Attached to the room".into()),
                    EngineEvent::RoomDetached { reason, .. } => listener.on_room(format!("Detached: {reason}")),
                    EngineEvent::RoomEnded { reason } => listener.on_room(format!("Room ended: {reason}")),
                    EngineEvent::RoomRejected { message, .. } => listener.on_room(format!("Room refused: {message}")),
                    _ => {}
                }
            }
        }
    }
    listener.on_status(BroadcastStatus::Ended {
        error: None,
        reclaim_status,
    });
    live.lock().unwrap().take();
}

/// The host clock the capture's PTS are on, in 100 ns, for the test source
/// (D27) to stamp its frames like ScreenCaptureKit would.
#[uniffi::export]
pub fn host_time_100ns() -> i64 {
    gawk_capture::host::now_100ns()
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
