//! The broadcast media path below capture (docs/67 D6, D8–D11), the macOS
//! shell's pipeline (docs/54 §5) re-cut for iOS: admitted frames →
//! VideoToolbox (`encode/vt.rs`, D10) → the engine's producer gate → the
//! send pump; audio → the iOS shim and resampler (D11) → the shared lane.
//!
//! The capture code (Swift, ScreenCaptureKit or D27's test source) hands
//! over frames already upright and fitted to the rung: it asks [`Pipeline::plan`]
//! what to make of each capture, does the one-pass
//! `VTPixelTransferSession` conversion, and pushes the result. A new
//! upright size rebuilds the compression session inside the same publish
//! session (D9): a new lineage on the sender, the codec re-derived from its
//! first SPS, an IDR first.
//!
//! One clock (docs/54 D5): video and audio PTS are host-clock (the
//! capture's), mapped onto the session clock by the same mapper.

use crate::audio::{Asbd, Resampler, to_stereo_f32};
use crate::rotation::{Rotation, RotationDebounce};
use crate::rung::{Quality, Rung};
use gawk_audio::lane::{Block, Lane};
use gawk_capture::host;
use gawk_capture::sck_policy::{Admission, ENCODER_MAX_IN_FLIGHT, FrameStatus};
use gawk_encode::cascade;
use gawk_encode::vt::{self, Encoder, EncoderParams, VtTrialRunner};
use gawk_engine::clock::{Clock, QpcMapper};
use gawk_engine::gate::FrameGate;
use gawk_engine::media::AccessUnit;
use gawk_engine::sender::Sender;
use objc2_core_video::{CVPixelBuffer, CVPixelBufferGetHeight, CVPixelBufferGetWidth};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

/// What the capture code should make of one captured frame.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Plan {
    pub rotation: Rotation,
    /// The upright, fitted size to convert into.
    pub width: u32,
    pub height: u32,
}

/// The encoder for one upright size, and its lineage's codec state.
struct Lineage {
    encoder: Arc<Encoder>,
    rung: Rung,
}

struct Video {
    admission: Admission,
    rotation: RotationDebounce,
    lineage: Option<Lineage>,
    /// The last frame's status, to re-prime after a suspension (D7).
    last_status: Option<FrameStatus>,
    trial_codec: Option<String>,
    /// Lineages built so far: the first keeps the trial's codec string,
    /// later ones re-derive theirs (D9).
    lineages: u64,
}

struct Audio {
    lane: Option<Lane>,
    resampler: Option<Resampler>,
    stereo: Vec<f32>,
    resampled: Vec<f32>,
    bytes: Vec<u8>,
    state: &'static str,
}

/// See the module docs.
pub struct Pipeline {
    sender: Arc<Sender>,
    rt: tokio::runtime::Handle,
    clock: Arc<dyn Clock>,
    mapper: QpcMapper,
    quality: Quality,
    gate: Arc<Mutex<FrameGate>>,
    notify: Arc<tokio::sync::Notify>,
    send_task: tokio::task::JoinHandle<()>,
    video: Mutex<Video>,
    audio: Mutex<Audio>,
    failed: Arc<Mutex<Option<String>>>,
    pub dropped_backpressure: AtomicU64,
}

impl Pipeline {
    /// The send pump starts now; the encoder is built for the first frame's
    /// size. `rt` drives the pump (the engine's runtime).
    pub fn new(
        sender: Arc<Sender>,
        rt: tokio::runtime::Handle,
        clock: Arc<dyn Clock>,
        quality: Quality,
    ) -> Self {
        let gate = Arc::new(Mutex::new(FrameGate::new()));
        let notify = Arc::new(tokio::sync::Notify::new());
        // docs/38 D5's offer policy: never blocks the encoder, GOP-drops,
        // keyframe-flushes.
        let send_task = {
            let (gate, notify, sender) = (gate.clone(), notify.clone(), sender.clone());
            rt.spawn(async move {
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
        let lane = Lane::new("sck-ios").ok();
        let mapper = host::mapper(&*clock);
        Self {
            sender,
            rt,
            clock,
            mapper,
            quality,
            gate,
            notify,
            send_task,
            video: Mutex::new(Video {
                admission: Admission::new(quality.fps()),
                rotation: RotationDebounce::new(Rotation::R0),
                lineage: None,
                last_status: None,
                trial_codec: None,
                lineages: 0,
            }),
            audio: Mutex::new(Audio {
                state: if lane.is_some() {
                    "active"
                } else {
                    "unavailable"
                },
                lane,
                resampler: None,
                stereo: Vec::new(),
                resampled: Vec::new(),
                bytes: Vec::new(),
            }),
            failed: Arc::default(),
            dropped_backpressure: AtomicU64::new(0),
        }
    }

    /// What to make of a `w` × `h` capture that wants `rotation` (`None`:
    /// face up/down or unknown) at host time `pts_100ns`: D9's debounced
    /// rotation and D8's rung for the upright size.
    pub fn plan(&self, w: u32, h: u32, rotation: Option<Rotation>, pts_100ns: i64) -> Plan {
        let ts = self.session_us(Some(pts_100ns));
        let r = self.video.lock().unwrap().rotation.observe(rotation, ts);
        let (uw, uh) = r.upright(w, h);
        let rung = self.quality.rung(uw, uh);
        Plan {
            rotation: r,
            width: rung.width,
            height: rung.height,
        }
    }

    fn session_us(&self, pts_100ns: Option<i64>) -> u64 {
        pts_100ns.map_or_else(|| self.clock.now_us(), |p| self.mapper.to_session_us(p))
    }

    /// One upright frame, with its capture status (`SCFrameStatus` raw).
    pub fn push_video(&self, pixels: &CVPixelBuffer, pts_100ns: Option<i64>, status_raw: i64) {
        let ts = self.session_us(pts_100ns);
        let status = FrameStatus::from_raw(status_raw);
        let mut v = self.video.lock().unwrap();
        // D7: frames resuming after a suspension (a call) re-prime with an
        // IDR; the session and keepalive carried on meanwhile.
        let resumed = status == FrameStatus::Complete
            && v.last_status.is_some_and(|s| s == FrameStatus::Suspended);
        v.last_status = Some(status);
        if v.admission.judge(status, ts).is_err() {
            return;
        }
        let (w, h) = (
            CVPixelBufferGetWidth(pixels) as u32,
            CVPixelBufferGetHeight(pixels) as u32,
        );
        let rebuild = v
            .lineage
            .as_ref()
            .is_none_or(|l| (l.rung.width, l.rung.height) != (w, h));
        if rebuild && let Err(e) = self.rebuild(&mut v, w, h) {
            self.fail(format!("encoder start: {e}"));
            return;
        }
        let Some(lineage) = &v.lineage else { return };
        let encoder = lineage.encoder.clone();
        let fps = lineage.rung.fps;
        drop(v);
        if resumed {
            encoder.force_idr();
        }
        // Backpressure (D10): at the limit, drop and count; the capture's
        // buffer is released as soon as this call returns.
        if encoder.in_flight() >= ENCODER_MAX_IN_FLIGHT {
            self.dropped_backpressure.fetch_add(1, Ordering::Relaxed);
            return;
        }
        let frame_100ns = 10_000_000 / i64::from(fps.max(1));
        if let Err(e) = encoder.encode(pixels, ts as i64 * 10, frame_100ns) {
            self.fail(format!("video pipeline error: {e}"));
        }
    }

    /// Builds the encoder for a new upright size: the trial gate the first
    /// time (enumeration is not acceptance, D10), then a new lineage per
    /// size (D9).
    fn rebuild(&self, v: &mut Video, w: u32, h: u32) -> Result<(), String> {
        let rung = Rung {
            width: w,
            height: h,
            ..self.quality.rung(w, h)
        };
        let params = EncoderParams {
            width: w,
            height: h,
            fps: rung.fps,
            peak_bitrate_bps: rung.peak_bitrate_bps,
        };
        if v.trial_codec.is_none() {
            let mut runner = VtTrialRunner { params };
            let accepted = cascade::choose(&vt::candidates(), None, &mut runner)
                .map_err(|r| format!("no hardware H.264 encoder: {:?}", r.tried))?;
            v.trial_codec = Some(accepted.codec_string);
        }
        if let Some(old) = v.lineage.take() {
            drop(old); // the old session finishes when its last Arc goes
        }
        v.lineages += 1;
        let first = v.lineages == 1;
        // A rotation keeps the publish session (D9): the same frame IDs and
        // audio lane, the video config re-derived from the new encoder's
        // first SPS (`restart_codec`, below) before that IDR is offered.
        // Not `new_lineage`, which would also drop the audio config the lane
        // advertises only once.
        if first {
            self.sender
                .set_codec(v.trial_codec.as_deref().unwrap_or_default());
        }
        let (gate, notify, sender) = (self.gate.clone(), self.notify.clone(), self.sender.clone());
        let mut codec_known = first;
        let failed = self.failed.clone();
        let encoder = Encoder::new(
            params,
            move |au| {
                if !codec_known
                    && au.keyframe
                    && let Some(codec) = gawk_encode::h264::parse_codec_string(&au.data)
                {
                    sender.restart_codec(&codec);
                    codec_known = true;
                }
                gate.lock().unwrap().offer(AccessUnit {
                    data: au.data,
                    timestamp_us: (au.time_100ns / 10).max(0) as u64,
                    keyframe: au.keyframe,
                });
                notify.notify_one();
            },
            move |e| {
                failed.lock().unwrap().get_or_insert(e);
            },
        )?;
        let encoder = Arc::new(encoder);
        // Every lineage opens on an IDR.
        encoder.force_idr();
        v.lineage = Some(Lineage { encoder, rung });
        Ok(())
    }

    /// One audio buffer as captured (D11). Audio never fails a broadcast
    /// (R25 Decision 6): any failure silences the lane and says so.
    pub fn push_audio(
        &self,
        asbd: &Asbd,
        buffers: &[&[u8]],
        frames: usize,
        pts_100ns: Option<i64>,
    ) {
        let ts = pts_100ns.map_or_else(
            || {
                self.clock
                    .now_us()
                    .saturating_sub(frames as u64 * 1_000_000 / asbd.sample_rate.max(1.0) as u64)
            },
            |p| self.mapper.to_session_us(p),
        );
        let mut a = self.audio.lock().unwrap();
        let a = &mut *a;
        if a.lane.is_none() {
            return;
        }
        a.stereo.clear();
        if let Err(e) = to_stereo_f32(asbd, buffers, frames, &mut a.stereo) {
            a.lane = None;
            a.state = "error";
            log::warn!("audio: {e}; broadcast continues without audio");
            return;
        }
        let rate = asbd.sample_rate.round() as u32;
        if a.resampler.as_ref().is_none_or(|r| r.in_rate() != rate) {
            match Resampler::new(rate) {
                Ok(r) => a.resampler = Some(r),
                Err(e) => {
                    a.lane = None;
                    a.state = "error";
                    log::warn!("audio: {e}; broadcast continues without audio");
                    return;
                }
            }
        }
        a.resampled.clear();
        if let Some(r) = a.resampler.as_mut() {
            r.process(&a.stereo, &mut a.resampled);
        }
        a.bytes.clear();
        a.bytes
            .extend(a.resampled.iter().flat_map(|s| s.to_le_bytes()));
        let out_frames = a.resampled.len() / 2;
        let bufs = [a.bytes.as_slice()];
        let block = Block::f32_stereo(&bufs, out_frames, ts);
        let Some(lane) = a.lane.as_mut() else { return };
        let out = lane.feed(Ok(block));
        if let Some(format) = out.advertise {
            self.sender.set_audio_format(format);
        }
        for packet in out.packets {
            self.sender.send_audio(packet);
        }
        if let Some(why) = out.failed {
            log::warn!("audio: {why}; broadcast continues without audio");
            a.state = "error";
        }
    }

    /// The engine resumed on a fresh session: re-prime the relay's
    /// invalidated keyframe cache.
    pub fn force_idr(&self) {
        if let Some(l) = &self.video.lock().unwrap().lineage {
            l.encoder.force_idr();
        }
    }

    /// "off" | "unavailable" | "active" | "error", for the live status.
    pub fn audio_state(&self) -> &'static str {
        self.audio.lock().unwrap().state
    }

    /// The first video failure, once.
    pub fn take_failure(&self) -> Option<String> {
        self.failed.lock().unwrap().take()
    }

    fn fail(&self, text: String) {
        self.failed.lock().unwrap().get_or_insert(text);
    }

    /// The upright size and codec in force, for the status line.
    pub fn current_size(&self) -> Option<(u32, u32)> {
        let v = self.video.lock().unwrap();
        v.lineage.as_ref().map(|l| (l.rung.width, l.rung.height))
    }

    /// The runtime the pump runs on.
    pub fn runtime(&self) -> &tokio::runtime::Handle {
        &self.rt
    }
}

impl Drop for Pipeline {
    fn drop(&mut self) {
        self.send_task.abort();
    }
}
