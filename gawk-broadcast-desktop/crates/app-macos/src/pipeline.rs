//! The live macOS media pipeline (R52 MB3/MB4, docs/54 §5): ScreenCaptureKit →
//! admission (idle-drop + drop-only fps gate) → VideoToolbox → the engine's
//! producer gate → the send pump; and, on the same stream's audio output,
//! PCM shim → framer → libopus → the audio lane (D6). One process, two
//! capture queues, one encoder, no pipes.
//!
//! The encoder is fed *session-clock* timestamps (the host PTS mapped once,
//! D5), so what VideoToolbox returns needs no second mapping.

use gawk_audio::lane::{Block, Lane};
use gawk_capture::host;
use gawk_capture::sck::{AudioBlock, Capture, Frame, OnAudio, StreamSettings};
use gawk_capture::sck_picker::Picked;
use gawk_capture::sck_policy::{Admission, ENCODER_MAX_IN_FLIGHT, nv12_thumbnail};
use gawk_encode::cascade;
use gawk_encode::vt::{self, Encoder, EncoderParams, VtTrialRunner};
use gawk_engine::clock::Clock;
use gawk_engine::clock::QpcMapper;
use gawk_engine::gate::FrameGate;
use gawk_engine::media::{AUDIO_SAMPLE_RATE, AccessUnit};
use gawk_engine::sender::Sender;
use gawk_ui::messages::StartFailure;
use gawk_ui::shell::{Media, MediaEnv, MediaInfo, Thumb};
use std::any::Any;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

const THUMB_W: u32 = 320;
const THUMB_H: u32 = 180;
const THUMB_EVERY_US: u64 = 1_000_000;

/// What a start needs, resolved on the GUI thread.
pub struct Params {
    pub picked: Picked,
    pub stream: StreamSettings,
    pub peak_bitrate_bps: u32,
    pub last_good_encoder: Option<String>,
    /// The config's audio switch (D6); off means byte-identical to a
    /// video-only broadcaster.
    pub audio: bool,
}

/// GUI-readable audio state (the shell's audio line and silence hint).
struct AudioShared {
    /// "off" | "unavailable" | "active" | "error"
    state: Mutex<String>,
    /// The lane, fed on the audio queue; the GUI reads its level meter.
    lane: Mutex<Option<Lane>>,
}

// The GUI reads below run on the UI thread every tick. The audio queue
// holds the lane's lock while it feeds it, so a panic in the audio path —
// caught by the capture's audio fence — leaves the mutex poisoned; that
// lane is dead (D6: audio stops, video runs on), so a poisoned lock reads
// as silence instead of unwrapping into a UI-thread panic.
impl AudioShared {
    fn level(&self) -> f32 {
        match self.lane.lock() {
            Ok(lane) => lane.as_ref().map_or(0.0, |l| l.level().level()),
            Err(_) => 0.0,
        }
    }

    fn silence_hint(&self) -> bool {
        match self.lane.lock() {
            Ok(lane) => lane.as_ref().is_some_and(|l| l.level().silence_hint()),
            Err(_) => false,
        }
    }
}

/// What the capture queue measures and the GUI reads.
struct Shared {
    admission: Admission,
    last_thumb_us: Option<u64>,
    thumb: Option<Thumb>,
}

pub struct Pipeline {
    info: MediaInfo,
    capture: Option<Capture>,
    encoder: Arc<Encoder>,
    shared: Arc<Mutex<Shared>>,
    clock: Arc<dyn Clock>,
    failed: Arc<Mutex<Option<String>>>,
    /// Frames the backpressure gate dropped (D10), for the debug log.
    dropped_backpressure: Arc<AtomicU64>,
    audio: Arc<AudioShared>,
    /// "Use whole-system audio" was clicked: the platform re-runs the
    /// picker in display mode for the live stream (D6).
    wants_system_audio: AtomicBool,
    send_task: tokio::task::JoinHandle<()>,
}

impl Pipeline {
    /// Trial, encoder, send pump, capture — in that order. Heavyweight (the
    /// trial encodes ~50 frames); runs on the shell's start thread.
    pub fn build(params: Params, env: MediaEnv) -> Result<Self, StartFailure> {
        let s = params.stream;
        log::info!(
            "pipeline build: {} → {}x{}@{} {} bps, last-good encoder {:?}",
            params.picked.summary,
            s.width,
            s.height,
            s.fps,
            params.peak_bitrate_bps,
            params.last_good_encoder
        );
        let enc_params = EncoderParams {
            width: s.width,
            height: s.height,
            fps: s.fps,
            peak_bitrate_bps: params.peak_bitrate_bps,
        };

        // The trial gate (D7): enumeration is not acceptance.
        let mut runner = VtTrialRunner { params: enc_params };
        let accepted = cascade::choose(
            &vt::candidates(),
            params.last_good_encoder.as_deref(),
            &mut runner,
        )
        .map_err(|refusal| {
            // The trail in the debug log is the only record of *why*.
            for (id, why) in &refusal.tried {
                log::error!("encoder candidate rejected: {id}: {why}");
            }
            StartFailure::NoHardwareEncoder
        })?;
        env.sender.set_codec(&accepted.codec_string);

        // Encoder output → producer gate → send pump (docs/38 D5's offer
        // policy: never blocks the encoder, GOP-drops, keyframe-flushes).
        let gate = Arc::new(Mutex::new(FrameGate::new()));
        let notify = Arc::new(tokio::sync::Notify::new());
        let send_task = {
            let gate = gate.clone();
            let notify = notify.clone();
            let sender = env.sender.clone();
            env.rt.spawn(async move {
                loop {
                    notify.notified().await;
                    loop {
                        let au = gate.lock().unwrap().pop();
                        match au {
                            Some(au) => sender.send_video(au).await,
                            None => break,
                        }
                    }
                }
            })
        };

        let failed: Arc<Mutex<Option<String>>> = Arc::default();
        let record = {
            let failed = failed.clone();
            move |text: String| {
                failed.lock().unwrap().get_or_insert(text);
            }
        };
        let encoder = Encoder::new(
            enc_params,
            {
                let gate = gate.clone();
                let notify = notify.clone();
                move |au| {
                    gate.lock().unwrap().offer(AccessUnit {
                        data: au.data,
                        // Session-clock µs, as fed (see the module docs).
                        timestamp_us: (au.time_100ns / 10).max(0) as u64,
                        keyframe: au.keyframe,
                    });
                    notify.notify_one();
                }
            },
            record.clone(),
        );
        let encoder = match encoder {
            Ok(e) => Arc::new(e),
            Err(e) => {
                send_task.abort();
                return Err(StartFailure::Capture(format!("encoder start: {e}")));
            }
        };

        // Capture. The host mapper is THE clock join (D5).
        let mapper = host::mapper(&*env.clock);
        let shared = Arc::new(Mutex::new(Shared {
            admission: Admission::new(s.fps),
            last_thumb_us: None,
            thumb: None,
        }));
        let dropped_backpressure = Arc::new(AtomicU64::new(0));
        let frame_100ns = 10_000_000 / i64::from(s.fps.max(1));
        let on_frame = {
            let shared = shared.clone();
            let encoder = encoder.clone();
            let clock = env.clock.clone();
            let dropped = dropped_backpressure.clone();
            let record = record.clone();
            move |frame: &Frame<'_>| {
                let ts_us = frame
                    .pts_100ns
                    .map(|p| mapper.to_session_us(p))
                    .unwrap_or_else(|| clock.now_us());
                let mut sh = shared.lock().unwrap();
                if sh.admission.judge(frame.status, ts_us).is_err() {
                    return;
                }
                let Some(pixels) = frame.pixel_buffer() else {
                    return;
                };
                // Backpressure (D10): at the in-flight limit, drop rather
                // than queue — a pending forced IDR stays latched in the
                // encoder for the next admitted frame.
                if encoder.in_flight() >= ENCODER_MAX_IN_FLIGHT {
                    dropped.fetch_add(1, Ordering::Relaxed);
                    return;
                }
                // The 1 Hz thumbnail, from the frame already in hand (D11).
                let due = sh
                    .last_thumb_us
                    .is_none_or(|t| ts_us.saturating_sub(t) >= THUMB_EVERY_US);
                if due
                    && let Some(t) = frame
                        .with_planes(|y, uv, w, h| nv12_thumbnail(y, uv, w, h, THUMB_W, THUMB_H))
                {
                    sh.thumb = Some(t);
                    sh.last_thumb_us = Some(ts_us);
                }
                drop(sh);
                if let Err(e) = encoder.encode(pixels, ts_us as i64 * 10, frame_100ns) {
                    record(format!("video pipeline error: {e}"));
                }
            }
        };
        // Audio, strictly subordinate (D6): anything that goes wrong drops
        // audio, notifies through the audio line, and leaves video running.
        let audio = Arc::new(AudioShared {
            state: Mutex::new("off".into()),
            lane: Mutex::new(None),
        });
        let on_audio = if params.audio {
            audio_lane(
                &audio,
                env.sender.clone(),
                mapper,
                env.clock.clone(),
                params.picked.style.capture_mode(),
            )
        } else {
            None
        };
        let capture = match Capture::start(&params.picked, s, on_frame, on_audio, record) {
            Ok(c) => c,
            Err(e) => {
                send_task.abort();
                return Err(StartFailure::Capture(e));
            }
        };
        log::info!(
            "pipeline ready: encoder {}, codec {}",
            accepted.id,
            accepted.codec_string
        );

        Ok(Self {
            info: MediaInfo {
                family: "VideoToolbox",
                encoder: accepted.id,
                codec: accepted.codec_string,
                capture_path: "zero-copy".into(),
                width: s.width,
                height: s.height,
                show_thumbnail: true,
            },
            capture: Some(capture),
            encoder,
            shared,
            clock: env.clock,
            failed,
            dropped_backpressure,
            audio,
            wants_system_audio: AtomicBool::new(false),
            send_task,
        })
    }

    /// Takes a pending "use whole-system audio" request (see
    /// `wants_system_audio`).
    pub fn take_system_audio_request(&self) -> bool {
        self.wants_system_audio.swap(false, Ordering::AcqRel)
    }

    /// Points the running capture at newly picked content (D4's re-pick
    /// while live). The encoder's size is fixed for the session, so the
    /// stream keeps its size and SCK letterboxes a new aspect.
    pub fn repick(&self, picked: &Picked) {
        if let Some(c) = &self.capture {
            c.update(picked, c.settings());
        }
    }

    /// The live capture, for the picker's present-for-stream.
    pub fn capture(&self) -> Option<&Capture> {
        self.capture.as_ref()
    }
}

impl Media for Pipeline {
    fn info(&self) -> &MediaInfo {
        &self.info
    }

    fn force_idr(&self) {
        self.encoder.force_idr();
    }

    fn take_thumbnail(&self) -> Option<Thumb> {
        self.shared.lock().unwrap().thumb.take()
    }

    fn capture_fps(&self) -> Option<f64> {
        self.shared.lock().unwrap().admission.fps()
    }

    fn audio_state(&self) -> String {
        if self.capture.as_ref().is_some_and(|c| c.audio_failed()) {
            return "error".into();
        }
        self.audio.state.lock().unwrap().clone()
    }

    fn audio_level(&self) -> f32 {
        self.audio.level()
    }

    fn audio_silence_hint(&self) -> bool {
        self.audio_state() == "active" && self.audio.silence_hint()
    }

    /// D6: system audio in ScreenCaptureKit comes from a display filter, so
    /// the switch is a re-pick in display mode — the platform presents it
    /// on its next tick. A new filter on the same stream: same Opus stream,
    /// same seq space, viewers notice nothing.
    fn switch_audio_to_system(&self) {
        self.wants_system_audio.store(true, Ordering::Release);
    }

    fn minimized(&self) -> bool {
        self.shared
            .lock()
            .unwrap()
            .admission
            .stale(self.clock.now_us())
    }

    fn take_failure(&self) -> Option<String> {
        self.failed.lock().unwrap().take()
    }

    fn shutdown(mut self: Box<Self>) {
        // Capture stops feeding first, then the encoder drains, then the
        // pump goes: no zombie capture (the `finish()` incident class).
        if let Some(c) = self.capture.take() {
            c.stop();
        }
        let dropped = self.dropped_backpressure.load(Ordering::Relaxed);
        if dropped > 0 {
            log::info!("backpressure dropped {dropped} frames this broadcast");
        }
        self.send_task.abort();
        // The encoder finishes when its last Arc (this one, once the
        // capture's output has released its copy) drops.
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

/// Builds the audio callback (D6) around a [`Lane`], or `None` when
/// libopus will not open — then the broadcast is video-only and says so.
fn audio_lane(
    shared: &Arc<AudioShared>,
    sender: Arc<Sender>,
    mapper: QpcMapper,
    clock: Arc<dyn Clock>,
    capture_mode: &'static str,
) -> Option<Box<OnAudio>> {
    let source = if capture_mode == "app" {
        "sck-app"
    } else {
        "sck-system"
    };
    match Lane::new(source) {
        Ok(lane) => *shared.lane.lock().unwrap() = Some(lane),
        Err(e) => {
            log::warn!("opus encoder: {e}; broadcasting without audio");
            *shared.state.lock().unwrap() = "unavailable".into();
            return None;
        }
    }
    *shared.state.lock().unwrap() = "active".into();
    let shared = shared.clone();
    let mut announced = false;
    let feed = move |block: Result<&AudioBlock<'_>, String>| {
        let block = block.map(|b| {
            if !announced {
                announced = true;
                log::info!(
                    "audio: {} Hz, {} ch, {}-bit, flags {:#x} from ScreenCaptureKit ({source})",
                    b.sample_rate,
                    b.channels,
                    b.bits_per_channel,
                    b.format_flags
                );
            }
            // One clock (D5): the SAME mapper as video. A buffer without a
            // time is stamped on arrival, minus its own duration.
            let timestamp_us = b
                .pts_100ns
                .map(|p| mapper.to_session_us(p))
                .unwrap_or_else(|| {
                    clock
                        .now_us()
                        .saturating_sub(b.frames as u64 * 1_000_000 / u64::from(AUDIO_SAMPLE_RATE))
                });
            Block {
                format_id: b.format_id,
                format_flags: b.format_flags,
                sample_rate: b.sample_rate,
                channels: b.channels,
                bits_per_channel: b.bits_per_channel,
                frames: b.frames,
                buffers: &b.buffers,
                timestamp_us,
            }
        });
        let out = match shared.lane.lock().unwrap().as_mut() {
            Some(lane) => lane.feed(block),
            None => return,
        };
        if let Some(format) = out.advertise {
            sender.set_audio_format(format);
        }
        for packet in out.packets {
            sender.send_audio(packet);
        }
        if let Some(why) = out.failed {
            log::warn!("audio: {why}; broadcast continues without audio");
            *shared.state.lock().unwrap() = "error".into();
        }
    };
    Some(Box::new(feed))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// D6: an audio panic stops audio, never the broadcast. The audio queue
    /// holds the lane's lock while it feeds it, so a panic there poisons
    /// the mutex — and the GUI reads the level every 250 ms on the UI
    /// thread. Reading a poisoned lane must not take the app down.
    #[test]
    fn a_panic_inside_the_lane_does_not_poison_the_gui_reads() {
        let shared = AudioShared {
            state: Mutex::new("active".into()),
            lane: Mutex::new(Some(Lane::new("sck-app").unwrap())),
        };
        let prev = std::panic::take_hook();
        std::panic::set_hook(Box::new(|_| {})); // keep the output clean
        let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _held = shared.lane.lock().unwrap();
            panic!("injected inside Lane::feed");
        }));
        std::panic::set_hook(prev);
        assert!(shared.lane.is_poisoned(), "the setup really poisoned it");
        assert_eq!(shared.level(), 0.0);
        assert!(!shared.silence_hint());
    }
}
