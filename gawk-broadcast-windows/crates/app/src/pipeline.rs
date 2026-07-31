//! The live media pipeline (Windows only): WGC → VideoProcessor → MFT →
//! engine sender, plus the audio lane (WASAPI → Opus → sender). One
//! process, three pumps, no pipes (docs/38 D3/§5).
//!
//! Panic hygiene (D3): the pump callbacks run under `catch_unwind`; a panic
//! surfaces as a pipeline error through the same channel as any other
//! failure — it must not take the GUI down silently or leave a zombie
//! capture running (the exact failure class R14's `finish()` incident
//! documents).

use crate::messages::StartFailure;
use gawk_audio::framer::{Framer, packet_timestamp_us};
use gawk_audio::level::LevelMeter;
use gawk_audio::opusenc::OpusEncoder;
use gawk_audio::wasapi::LoopbackCapture;
use gawk_audio::{AudioMode, advertised_format, toc};
use gawk_capture::d3d::{Converter, GpuDevice};
use gawk_capture::gate::{FpsGate, FpsMeter};
use gawk_capture::qpc;
use gawk_capture::wgc::{self, CaptureTarget};
use gawk_encode::cascade::{self, TrialRunner};
use gawk_encode::mft::{self, EncoderInput, EncoderParams, EncoderSession};
use gawk_engine::clock::Clock;
use gawk_engine::gate::FrameGate;
use gawk_engine::media::{AccessUnit, AudioPacket};
use gawk_engine::sender::Sender;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

/// A downscaled RGBA thumbnail: width, height, pixels.
pub type Thumb = (u32, u32, Vec<u8>);

// The ring-soundness pin (see both constants' docs): the backpressure gate
// must trip before the converter ring can wrap onto an in-flight texture.
const _: () = assert!(mft::MAX_IN_FLIGHT < gawk_capture::d3d::RING_SLOTS);

/// Aborts the send pump if `build` fails after spawning it; disarmed into
/// the finished `Pipeline` on success. Without this, every failed Start
/// attempt (capture target gone, encoder start error) leaked one
/// ever-running task holding the gate and sender Arcs for the app's
/// lifetime.
struct SendTaskGuard(Option<tokio::task::JoinHandle<()>>);

impl SendTaskGuard {
    fn disarm(mut self) -> tokio::task::JoinHandle<()> {
        self.0.take().expect("guard disarmed once")
    }
}

impl Drop for SendTaskGuard {
    fn drop(&mut self) {
        if let Some(task) = &self.0 {
            task.abort();
        }
    }
}

pub struct PipelineParams {
    pub target: CaptureTarget,
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    pub bitrate_bps: u32,
    pub last_good_encoder: Option<String>,
    pub audio_mode: AudioMode,
}

/// What the shell shows about a built pipeline.
#[derive(Clone)]
pub struct PipelineInfo {
    pub encoder: String,
    pub codec: String,
    pub capture_path: &'static str,
}

/// Shared, GUI-readable audio state.
struct AudioShared {
    state: Mutex<String>, // "off" | "unavailable" | "active" | "error"
    level: Mutex<LevelMeter>,
}

// Built on the start thread, then handed once to the UI thread and driven
// from there. The WinRT capture objects are agile and the D3D device is
// multithread-protected; nothing here is thread-affine.
unsafe impl Send for Pipeline {}

pub struct Pipeline {
    pub info: PipelineInfo,
    gpu: GpuDevice,
    capture: Option<wgc::Capture>,
    audio: Mutex<Option<LoopbackCapture>>,
    audio_shared: Arc<AudioShared>,
    encoder: Arc<EncoderSession>,
    force_idr: Arc<AtomicBool>,
    thumb: Arc<Mutex<Option<Thumb>>>,
    capture_fps: Arc<Mutex<FpsMeter>>,
    /// Set once by any pump that dies; the shell polls it.
    failed: Arc<Mutex<Option<String>>>,
    send_task: tokio::task::JoinHandle<()>,
    sender: Arc<Sender>,
    clock: Arc<dyn Clock>,
    hwnd: Option<isize>,
}

impl Pipeline {
    /// Assembles and starts the whole pipeline. Heavyweight (trial encodes
    /// run here) — call off the GUI thread.
    pub fn build(
        params: PipelineParams,
        sender: Arc<Sender>,
        clock: Arc<dyn Clock>,
        rt: tokio::runtime::Handle,
    ) -> Result<Self, StartFailure> {
        log::info!(
            "pipeline build: {}x{}@{} {} bps, target {:?}, last-good encoder {:?}, audio {:?}",
            params.width,
            params.height,
            params.fps,
            params.bitrate_bps,
            params.target,
            params.last_good_encoder,
            params.audio_mode
        );
        let gpu = GpuDevice::hardware().map_err(|e| {
            log::error!("D3D11 hardware device creation failed: {e}");
            StartFailure::Capture(format!("no usable graphics device: {e}"))
        })?;
        log::info!("D3D11 device on adapter: {}", gpu.adapter_summary());

        // The cascade with real trials (enumeration is not acceptance).
        let entries = mft::enumerate_hardware().map_err(|e| {
            log::error!("hardware MFT enumeration failed: {e}");
            StartFailure::Capture(format!("encoder enumeration failed: {e}"))
        })?;
        log::info!(
            "hardware H.264 encoder MFTs enumerated: {} [{}]",
            entries.len(),
            entries
                .iter()
                .map(|e| e.name.as_str())
                .collect::<Vec<_>>()
                .join(", ")
        );
        let candidates = mft::candidates(&entries);
        if candidates.is_empty() {
            log::error!(
                "refusing to start: MFTEnumEx(MFT_ENUM_FLAG_HARDWARE) returned no H.264 \
                 encoder MFTs — driver missing/outdated, remote session, or Windows N \
                 without the Media Feature Pack are the known causes"
            );
            return Err(StartFailure::NoHardwareEncoder);
        }
        let enc_params = EncoderParams {
            width: params.width,
            height: params.height,
            fps: params.fps,
            peak_bitrate_bps: params.bitrate_bps,
            gop_frames: (params.fps / 2).max(1),
        };
        let mut runner = mft::MftTrialRunner {
            device: &gpu.device,
            params: enc_params,
            entries: &entries,
        };
        let accepted = cascade::choose(
            &candidates,
            params.last_good_encoder.as_deref(),
            &mut runner as &mut dyn TrialRunner,
        )
        .map_err(|refusal| {
            // Encoders may exist yet none survived the invariant gate: the
            // user sees the same refusal either way, so the trail in the
            // debug log is the ONLY record distinguishing "nothing
            // enumerated" from "everything rejected, and why".
            log::error!(
                "refusing to start: no candidate survived the trial gate ({} tried)",
                refusal.tried.len()
            );
            for (id, why) in &refusal.tried {
                log::error!("encoder candidate rejected: {id}: {why}");
            }
            StartFailure::NoHardwareEncoder
        })?;
        sender.set_codec(&accepted.codec_string);

        let entry = entries
            .iter()
            .find(|e| e.name == accepted.id)
            .expect("accepted candidate exists");

        // Encoder output → producer gate → send pump. The gate is the Go
        // source's offer policy: never blocks the encoder, GOP-drops,
        // keyframe-flushes (docs/38 D5).
        let gate = Arc::new(Mutex::new(FrameGate::new()));
        let notify = Arc::new(tokio::sync::Notify::new());
        let send_task = SendTaskGuard(Some({
            let gate = gate.clone();
            let notify = notify.clone();
            let sender = sender.clone();
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
        }));

        let failed: Arc<Mutex<Option<String>>> = Arc::new(Mutex::new(None));
        let prepend = accepted.prepend_headers.clone();
        let encoder = {
            let gate = gate.clone();
            let notify = notify.clone();
            Arc::new(
                EncoderSession::start(
                    entry,
                    &gpu.device,
                    enc_params,
                    Box::new(move |au| {
                        let data = cascade::ensure_idr_headers(au.data, au.keyframe, &prepend);
                        gate.lock().unwrap().offer(AccessUnit {
                            data,
                            timestamp_us: au.time_us,
                            keyframe: au.keyframe,
                        });
                        notify.notify_one();
                    }),
                )
                .map_err(|e| StartFailure::Capture(format!("encoder start: {e}")))?,
            )
        };

        // Capture. The QPC mapper is THE clock join (docs/38 D7): video
        // here and audio below map device stamps through mappers built
        // against the same session clock.
        let mapper = qpc::mapper(&*clock);
        let force_idr = Arc::new(AtomicBool::new(false));
        let thumb: Arc<Mutex<Option<Thumb>>> = Arc::new(Mutex::new(None));
        let capture_fps = Arc::new(Mutex::new(FpsMeter::default()));
        let item = wgc::create_item(params.target).map_err(|e| {
            StartFailure::Capture(format!("could not open the capture target: {e}"))
        })?;
        let mode1 = matches!(params.target, CaptureTarget::Window { .. });
        let hwnd = match params.target {
            CaptureTarget::Window { hwnd, .. } => Some(hwnd),
            CaptureTarget::Monitor { .. } => None,
        };

        let capture = {
            let gpu = gpu.clone();
            let encoder = encoder.clone();
            let force_idr = force_idr.clone();
            let thumb = thumb.clone();
            let capture_fps = capture_fps.clone();
            let failed = failed.clone();
            let mut fps_gate = FpsGate::new(params.fps);
            let mut converter: Option<Converter> = None;
            let mut last_thumb_us = 0u64;
            let frame_us = 1_000_000 / u64::from(params.fps.max(1));
            let (out_w, out_h) = (params.width, params.height);

            wgc::Capture::start(&gpu.clone(), item, move |frame| {
                // Panic hygiene (D3): a poisoned frame must not kill the
                // pool thread silently.
                let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    if frame.width == 0 || frame.height == 0 {
                        return Ok(());
                    }
                    let ts = mapper.to_session_us(frame.system_relative_100ns);
                    if !fps_gate.admit(ts) {
                        return Ok(());
                    }
                    capture_fps.lock().unwrap().observe(ts);

                    // Backpressure (the ring-soundness gate): when the MFT
                    // lags capture — GPU contention from the captured game
                    // is the normal cause — DROP the frame rather than
                    // convert into a ring slot the encoder may still be
                    // reading. A pending force_idr stays latched for the
                    // next admitted frame.
                    if encoder.in_flight() >= mft::MAX_IN_FLIGHT {
                        return Ok(());
                    }

                    if !converter
                        .as_ref()
                        .is_some_and(|c| c.matches_input(frame.width, frame.height))
                    {
                        converter = Some(Converter::new(
                            &gpu,
                            frame.width,
                            frame.height,
                            out_w,
                            out_h,
                        )?);
                    }
                    let conv = converter.as_mut().expect("converter just ensured");
                    let nv12 = conv.convert_rotating(&frame.texture)?;

                    // The 1 Hz confidence thumbnail (mode 1 only): a frame
                    // already in hand, downscaled off the encode path.
                    if mode1 && ts.saturating_sub(last_thumb_us) >= 1_000_000 {
                        last_thumb_us = ts;
                        if let Ok(t) = conv.thumbnail_rgba(&frame.texture, 320) {
                            *thumb.lock().unwrap() = Some(t);
                        }
                    }

                    encoder.send(EncoderInput {
                        texture: nv12,
                        time_us: ts,
                        duration_us: frame_us,
                        force_idr: force_idr.swap(false, Ordering::SeqCst),
                    });
                    Ok::<(), windows::core::Error>(())
                }));
                match result {
                    Ok(Ok(())) => {}
                    Ok(Err(e)) => {
                        failed
                            .lock()
                            .unwrap()
                            .get_or_insert_with(|| format!("video pipeline error: {e}"));
                    }
                    Err(_) => {
                        failed
                            .lock()
                            .unwrap()
                            .get_or_insert_with(|| "video pipeline panicked".to_string());
                    }
                }
            })
            .map_err(|e| StartFailure::Capture(format!("screen capture start: {e}")))?
        };

        // Audio, strictly subordinate (D8): a probe failure or a live
        // failure leaves video running; off-wire it is byte-identical to a
        // video-only broadcaster.
        let audio_shared = Arc::new(AudioShared {
            state: Mutex::new("off".into()),
            level: Mutex::new(LevelMeter::default()),
        });
        let audio = match params.audio_mode {
            AudioMode::Off => None,
            mode => start_audio(mode, &sender, &clock, &audio_shared, mapper),
        };

        Ok(Self {
            info: PipelineInfo {
                encoder: accepted.id,
                codec: accepted.codec_string,
                capture_path: "zero-copy",
            },
            gpu,
            capture: Some(capture),
            audio: Mutex::new(audio),
            audio_shared,
            encoder,
            force_idr,
            thumb,
            capture_fps,
            failed,
            send_task: send_task.disarm(),
            sender,
            clock,
            hwnd,
        })
    }

    /// Resume re-prime (D5): the relay's caches were invalidated; the next
    /// frame carries an IDR instead of waiting out the GOP.
    pub fn force_idr(&self) {
        self.force_idr.store(true, Ordering::SeqCst);
        self.encoder.force_idr();
    }

    pub fn take_thumbnail(&self) -> Option<Thumb> {
        self.thumb.lock().unwrap().take()
    }

    pub fn capture_fps(&self) -> Option<f64> {
        self.capture_fps.lock().unwrap().fps()
    }

    pub fn audio_state(&self) -> String {
        self.audio_shared.state.lock().unwrap().clone()
    }

    pub fn audio_level(&self) -> f32 {
        self.audio_shared.level.lock().unwrap().level()
    }

    pub fn audio_silence_hint(&self) -> bool {
        *self.audio_shared.state.lock().unwrap() == "active"
            && self.audio_shared.level.lock().unwrap().silence_hint()
    }

    /// The D8 one-click switch: per-app audio → whole-system, mid-session.
    /// A new capture, not a renegotiation — same Opus stream, same seq
    /// space; viewers notice nothing.
    pub fn switch_audio_to_system(&self) {
        let mut audio = self.audio.lock().unwrap();
        if let Some(old) = audio.take() {
            old.stop();
        }
        let mapper = qpc::mapper(&*self.clock);
        *audio = start_audio(
            AudioMode::SystemLoopback,
            &self.sender,
            &self.clock,
            &self.audio_shared,
            mapper,
        );
    }

    /// Whether the captured window is minimized (mode 1): WGC delivers no
    /// frames for minimized windows; the GUI hint owns that honesty.
    pub fn minimized(&self) -> bool {
        self.hwnd.is_some_and(wgc::is_minimized)
    }

    /// A pump died; the broadcast should end with this message.
    pub fn take_failure(&self) -> Option<String> {
        self.failed.lock().unwrap().take()
    }

    /// Tears the media down in dependency order: capture stops feeding,
    /// audio stops, the encoder drains. No zombie capture (the `finish()`
    /// incident class).
    pub fn shutdown(mut self) {
        drop(self.capture.take());
        if let Some(a) = self.audio.lock().unwrap().take() {
            a.stop();
        }
        self.send_task.abort();
        let _ = &self.gpu;
        // EncoderSession's Drop closes input and joins the pump.
    }
}

fn start_audio(
    mode: AudioMode,
    sender: &Arc<Sender>,
    clock: &Arc<dyn Clock>,
    shared: &Arc<AudioShared>,
    mapper: gawk_engine::clock::QpcMapper,
) -> Option<LoopbackCapture> {
    let source = match mode {
        AudioMode::ProcessLoopback { .. } => "process-loopback",
        AudioMode::SystemLoopback => "system-loopback",
        AudioMode::Off => return None,
    };

    // The R25 probe: prove open+encode before going live.
    if let Err(e) = gawk_audio::wasapi::probe(mode) {
        log::warn!("audio probe failed ({source}): {e}; broadcasting without audio");
        *shared.state.lock().unwrap() = "unavailable".into();
        return None;
    }

    let mut opus = match OpusEncoder::new() {
        Ok(e) => e,
        Err(e) => {
            log::warn!("opus encoder: {e}; broadcasting without audio");
            *shared.state.lock().unwrap() = "unavailable".into();
            return None;
        }
    };

    let format = advertised_format(source);
    sender.set_audio_format(format.clone());

    let mut framer = Framer::new();
    let mut checked = false;
    let mut errored = false;
    let clock = clock.clone();
    let sender = sender.clone();
    let shared_cb = shared.clone();
    let shared_err = shared.clone();

    let capture = LoopbackCapture::start(
        mode,
        Box::new(move |pkt| {
            if errored {
                return;
            }
            let ts = packet_timestamp_us(
                pkt.qpc_100ns.map(|q| mapper.to_session_us(q)),
                clock.now_us(),
                pkt.buffered_ahead_samples,
            );
            for frame in framer.push(&pkt.interleaved, ts) {
                shared_cb
                    .level
                    .lock()
                    .unwrap()
                    .observe(&frame.interleaved, frame.timestamp_us);
                let Ok(data) = opus.encode(&frame.interleaved) else {
                    continue;
                };
                if !checked {
                    checked = true;
                    // R25 Decision 10: the first packet's bitstream must
                    // agree with the advertised config — disagreement logs
                    // loudly and marks audio errored, video runs on.
                    if let Err(e) = toc::verify_against_config(&data, &format) {
                        log::warn!("audio config verification failed: {e}; dropping audio");
                        *shared_cb.state.lock().unwrap() = "error".into();
                        errored = true;
                        return;
                    }
                }
                sender.send_audio(AudioPacket {
                    data,
                    timestamp_us: frame.timestamp_us,
                });
            }
        }),
        Box::new(move |e| {
            log::warn!("audio capture died: {e}; broadcast continues without audio");
            *shared_err.state.lock().unwrap() = "error".into();
        }),
    );
    match capture {
        Ok(c) => {
            *shared.state.lock().unwrap() = "active".into();
            Some(c)
        }
        Err(e) => {
            log::warn!("audio capture start failed: {e}; broadcasting without audio");
            *shared.state.lock().unwrap() = "unavailable".into();
            None
        }
    }
}
