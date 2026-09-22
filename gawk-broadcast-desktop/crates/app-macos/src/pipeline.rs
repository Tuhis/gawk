//! The live macOS media pipeline (R52 MB3, docs/54 §5): ScreenCaptureKit →
//! admission (idle-drop + drop-only fps gate) → VideoToolbox → the engine's
//! producer gate → the send pump. One process, one capture queue, one
//! encoder, no pipes. Audio is MB4's.
//!
//! The encoder is fed *session-clock* timestamps (the host PTS mapped once,
//! D5), so what VideoToolbox returns needs no second mapping.

use gawk_capture::host;
use gawk_capture::sck::{Capture, Frame, StreamSettings};
use gawk_capture::sck_picker::Picked;
use gawk_capture::sck_policy::{Admission, ENCODER_MAX_IN_FLIGHT, nv12_thumbnail};
use gawk_encode::cascade;
use gawk_encode::vt::{self, Encoder, EncoderParams, VtTrialRunner};
use gawk_engine::clock::Clock;
use gawk_engine::gate::FrameGate;
use gawk_engine::media::AccessUnit;
use gawk_ui::messages::StartFailure;
use gawk_ui::shell::{Media, MediaEnv, MediaInfo, Thumb};
use std::any::Any;
use std::sync::atomic::{AtomicU64, Ordering};
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
        let capture = match Capture::start(&params.picked, s, on_frame, record) {
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
            send_task,
        })
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

    // Audio arrives in MB4: until then a broadcast is video-only, which on
    // the wire is byte-identical to an audio-off one (D6).
    fn audio_state(&self) -> String {
        "off".into()
    }

    fn audio_level(&self) -> f32 {
        0.0
    }

    fn audio_silence_hint(&self) -> bool {
        false
    }

    fn switch_audio_to_system(&self) {}

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
