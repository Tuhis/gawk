//! The live Linux media (docs/58 §5): the portal grant's PipeWire node →
//! the D4 video pipeline (the cascade × capture ladder, [`crate::video`]) →
//! the engine's producer gate → the send pump; and, beside it, the audio
//! pipeline → the shared libopus lane ([`crate::audio`]). One process, two
//! GStreamer pipelines, one PipeWire control connection, one clock, no
//! pipes.

use crate::audio::{AudioPart, AudioPlan};
use crate::video::{
    self, AttemptError, Launcher, Report, Running, Supervisor, SupervisorParams, Walk, WalkError,
};
use gawk_capture::gate::FpsMeter;
use gawk_capture::portal::Grant;
use gawk_capture::pwclock::{self, Mapper};
use gawk_encode::cascade::{self, TrialRunner};
use gawk_encode::gst::{self, GstTrialRunner, Live, LiveAu, LiveHooks};
use gawk_encode::gst_policy::{self, Candidate, CaptureRung, LiveParams};
use gawk_encode::h264;
use gawk_engine::gate::FrameGate;
use gawk_engine::media::AccessUnit;
use gawk_engine::sender::Sender;
use gawk_ui::messages::StartFailure;
use gawk_ui::shell::{Media, MediaEnv, MediaInfo, Thumb};
use std::any::Any;
use std::io::Write;
use std::os::fd::AsRawFd;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc;
use std::sync::{Arc, Mutex};

/// What a start needs, resolved on the GUI thread.
pub struct Params {
    pub grant: Grant,
    /// The fitted, even encode size (docs/39 D2).
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    pub peak_bps: u32,
    /// The `encoder` pin (OD12): the cascade reduced to it, still trialled.
    pub encoder_pin: String,
    pub last_good_encoder: String,
    pub audio: AudioPlan,
}

/// Everything an attempt's appsink callbacks feed, shared across attempts
/// (a rebuild's pipeline feeds the same gate, sender and meters).
struct FrameSink {
    gate: Arc<Mutex<FrameGate>>,
    notify: Arc<tokio::sync::Notify>,
    sender: Arc<Sender>,
    mapper: Mapper,
    fps: Mutex<FpsMeter>,
    thumb: Mutex<Option<Thumb>>,
    /// The codec the viewer was last told (D6: a rebuild's lineage may
    /// differ, and then `restart_codec` re-derives the config).
    codec: Mutex<String>,
}

impl FrameSink {
    /// A new attempt begins: nothing from the dead one may follow it out.
    fn begin_attempt(&self) {
        *self.gate.lock().unwrap() = FrameGate::new();
    }

    fn on_au(&self, au: LiveAu, first: &mut bool) {
        if *first {
            // Each attempt's first AU is an IDR carrying its SPS: the codec
            // string comes from the bitstream, never assumed (D5).
            if !au.idr {
                return;
            }
            if let Some(codec) = h264::parse_codec_string(&au.data) {
                let mut cur = self.codec.lock().unwrap();
                if *cur != codec {
                    if cur.is_empty() {
                        self.sender.set_codec(&codec);
                    } else {
                        log::info!("codec changed across a capture rebuild: {cur} → {codec}");
                        self.sender.restart_codec(&codec);
                    }
                    *cur = codec;
                }
            }
            *first = false;
        }
        self.gate.lock().unwrap().offer(AccessUnit {
            timestamp_us: self.mapper.to_session_us(au.capture_ns),
            keyframe: au.idr,
            data: au.data,
        });
        self.notify.notify_one();
    }
}

/// The appsink hooks every attempt gets: its AUs into the shared sink, its
/// frames into the fps meter, its thumbnails, and its errors tagged with the
/// attempt that raised them.
fn hooks_for(
    sink: &Arc<FrameSink>,
    errors: &mpsc::Sender<AttemptError>,
    attempt: u64,
) -> LiveHooks {
    sink.begin_attempt();
    let first = Mutex::new(true);
    LiveHooks {
        on_au: {
            let sink = sink.clone();
            Box::new(move |au| sink.on_au(au, &mut first.lock().unwrap()))
        },
        on_input: {
            let sink = sink.clone();
            Box::new(move |ns| {
                let us = sink.mapper.to_session_us(ns);
                sink.fps.lock().unwrap().observe(us);
            })
        },
        on_thumb: {
            let sink = sink.clone();
            Box::new(move |w, h, rgba| *sink.thumb.lock().unwrap() = Some((w, h, rgba)))
        },
        on_error: {
            let tx = errors.clone();
            Box::new(move |e| {
                let _ = tx.send((attempt, e));
            })
        },
    }
}

/// The real launcher: GStreamer, on the held portal grant.
struct GstLauncher {
    live: LiveParams,
    sink: Arc<FrameSink>,
    errors: mpsc::Sender<AttemptError>,
    runner: GstTrialRunner,
}

struct LiveRun(Live);

impl Running for LiveRun {
    fn force_idr(&self) {
        self.0.force_idr();
    }
    fn stop(self: Box<Self>) {
        self.0.stop();
    }
}

impl Launcher for GstLauncher {
    fn trial(&mut self, c: Candidate) -> Result<(), String> {
        let run = self.runner.run(&cascade::Candidate {
            id: c.element().into(),
        })?;
        cascade::validate_trial(&run).map(|_| ())
    }

    fn launch(
        &mut self,
        c: Candidate,
        rung: CaptureRung,
        attempt: u64,
    ) -> Result<Box<dyn Running>, String> {
        let plan = gst_policy::live_plan(c, rung, self.live);
        let hooks = hooks_for(&self.sink, &self.errors, attempt);
        Live::start(&plan, c, self.live.fps, hooks)
            .map(|l| Box::new(LiveRun(l)) as Box<dyn Running>)
    }
}

struct Reports {
    restarts: Arc<AtomicU64>,
    failed: Arc<Mutex<Option<String>>>,
}

impl Report for Reports {
    fn rebuilt(&self, c: Candidate, rung: CaptureRung) {
        let n = self.restarts.fetch_add(1, Ordering::Relaxed) + 1;
        log::info!(
            "capture rebuilt ({n} this broadcast): {} on {}",
            c.element(),
            rung.label()
        );
    }
    fn failed(&self, text: String) {
        self.failed.lock().unwrap().get_or_insert(text);
    }
}

pub struct Pipeline {
    info: MediaInfo,
    supervisor: Option<Supervisor>,
    sink: Arc<FrameSink>,
    restarts: Arc<AtomicU64>,
    failed: Arc<Mutex<Option<String>>>,
    audio: AudioPart,
    share_mode: &'static str,
    send_task: tokio::task::JoinHandle<()>,
    /// Held for the broadcast (cascade retries and rebuilds reuse it, D3),
    /// released when the media shuts down — every broadcast asks again.
    grant: Option<Grant>,
}

/// Makes the launcher once the shared sink and the error channel exist.
type MakeLauncher =
    Box<dyn FnOnce(Arc<FrameSink>, mpsc::Sender<AttemptError>) -> Box<dyn Launcher>>;

/// Everything `assemble` needs besides the launcher: the start's decisions,
/// independent of where frames come from.
struct Assembly {
    order: Vec<Candidate>,
    pinned: bool,
    last_good: Option<Candidate>,
    width: u32,
    height: u32,
    share_mode: &'static str,
    audio: AudioPlan,
    grant: Option<Grant>,
    probe: std::time::Duration,
}

impl Pipeline {
    /// Heavyweight — trials, the live probe, the audio pre-flight — so it
    /// runs on the shell's start thread.
    pub fn build(params: Params, env: MediaEnv) -> Result<Self, StartFailure> {
        gst::init().map_err(StartFailure::Capture)?;
        let order = gst_policy::cascade_for(&params.encoder_pin).map_err(StartFailure::Capture)?;
        let pinned = order.len() == 1 && !params.encoder_pin.trim().is_empty();
        let last_good = gst_policy::find(params.last_good_encoder.trim());
        log::info!(
            "pipeline build: {:?} node {} → {}x{}@{} {} bps, encoder pin {:?}, last-good {:?}",
            params.grant.kind,
            params.grant.node_id,
            params.width,
            params.height,
            params.fps,
            params.peak_bps,
            params.encoder_pin,
            last_good.map(Candidate::element)
        );
        let live = LiveParams {
            fd: params.grant.fd.as_raw_fd(),
            node_id: params.grant.node_id,
            width: params.width,
            height: params.height,
            fps: params.fps,
            peak_bps: params.peak_bps,
        };
        let runner = GstTrialRunner {
            fps: params.fps,
            peak_bps: params.peak_bps,
        };
        let share_mode = match params.grant.kind {
            gawk_capture::portal::SourceKind::Window => "window",
            gawk_capture::portal::SourceKind::Monitor => "screen",
        };
        Self::assemble(
            Assembly {
                order,
                pinned,
                last_good,
                width: params.width,
                height: params.height,
                share_mode,
                audio: params.audio,
                grant: Some(params.grant),
                probe: gst_policy::LIVE_PROBE,
            },
            env,
            Box::new(move |sink, errors| {
                Box::new(GstLauncher {
                    live,
                    sink,
                    errors,
                    runner,
                })
            }),
        )
    }

    /// The build proper, with the launcher injected: the real one runs on
    /// the portal grant; the tests' runs `videotestsrc` through a software
    /// encoder, and everything around it is this function's.
    fn assemble(a: Assembly, env: MediaEnv, make: MakeLauncher) -> Result<Self, StartFailure> {
        // The audio pre-flight first, as the Go app did: it opens no dialog
        // and touches no GPU, and it can never fail the start (R25 D6).
        let audio_source = crate::audio::select(&a.audio);

        let gate = Arc::new(Mutex::new(FrameGate::new()));
        let notify = Arc::new(tokio::sync::Notify::new());
        let dump = open_dump();
        let send_task = {
            let gate = gate.clone();
            let notify = notify.clone();
            let sender = env.sender.clone();
            env.rt.spawn(async move {
                let mut dump = dump;
                loop {
                    notify.notified().await;
                    loop {
                        let au = gate.lock().unwrap().pop();
                        let Some(au) = au else { break };
                        if let Some(f) = dump.as_mut()
                            && f.write_all(&au.data).is_err()
                        {
                            dump = None;
                        }
                        sender.send_video(au).await;
                    }
                }
            })
        };

        let mapper = pwclock::mapper(&*env.clock);
        let sink = Arc::new(FrameSink {
            gate,
            notify,
            sender: env.sender.clone(),
            mapper,
            fps: Mutex::new(FpsMeter::default()),
            thumb: Mutex::new(None),
            codec: Mutex::new(String::new()),
        });
        let (errors_tx, errors_rx) = mpsc::channel();
        let mut launcher = make(sink.clone(), errors_tx);
        let mut next_attempt = 0;
        let adopted = video::walk(
            &mut *launcher,
            &errors_rx,
            &mut next_attempt,
            &|| false,
            Walk {
                order: &a.order,
                last_good: a.last_good,
                trusted: None,
                pinned: a.pinned,
                probe: a.probe,
            },
        );
        let adopted = match adopted {
            Ok(adopted) => adopted,
            Err(e) => {
                send_task.abort();
                return Err(start_failure(e));
            }
        };
        let info = MediaInfo {
            family: adopted.candidate.api(),
            encoder: adopted.candidate.element().into(),
            codec: sink.codec.lock().unwrap().clone(),
            capture_path: adopted.rung.path().into(),
            width: a.width,
            height: a.height,
            show_thumbnail: gst_policy::thumbnail_on(adopted.rung),
        };
        log::info!(
            "pipeline ready: {} ({}) on {}, codec {}",
            info.encoder,
            info.family,
            info.capture_path,
            info.codec
        );

        let restarts = Arc::new(AtomicU64::new(0));
        let failed: Arc<Mutex<Option<String>>> = Arc::default();
        let clock = env.clock.clone();
        let supervisor = Supervisor::start(
            launcher,
            errors_rx,
            adopted,
            SupervisorParams {
                order: a.order,
                pinned: a.pinned,
                probe: a.probe,
                budget: gst_policy::REBUILD_BUDGET,
            },
            Box::new(Reports {
                restarts: restarts.clone(),
                failed: failed.clone(),
            }),
            Box::new(move || clock.now_us()),
        );

        let audio = AudioPart::start(
            a.audio,
            audio_source,
            env.sender.clone(),
            mapper,
            env.clock.clone(),
        );
        Ok(Self {
            info,
            supervisor: Some(supervisor),
            sink,
            restarts,
            failed,
            audio,
            share_mode: a.share_mode,
            send_task,
            grant: a.grant,
        })
    }
}

/// The start failure a walk that adopted nothing becomes. Each trail line
/// goes to the debug log: it is the only record of WHY.
fn start_failure(e: WalkError) -> StartFailure {
    match e {
        WalkError::NoHardwareEncoder(trail) => {
            for line in &trail {
                log::error!("encoder candidate rejected: {line}");
            }
            StartFailure::NoHardwareEncoder
        }
        WalkError::CaptureFormat(trail) => {
            for line in &trail {
                log::error!("capture attempt failed: {line}");
            }
            StartFailure::Capture(gst_policy::CAPTURE_FORMAT_MESSAGE.into())
        }
        WalkError::PinnedFailed(name, trail) => {
            for line in &trail {
                log::error!("pinned encoder failed: {line}");
            }
            StartFailure::Capture(format!(
                "the encoder pinned in the config ({name}) failed to start; \
clear \"encoder\" in broadcast.json to let the cascade choose"
            ))
        }
        WalkError::Stopped => StartFailure::Capture("stopped".into()),
    }
}

/// `GAWK_DUMP_H264=<path>` (OD12): every AU as sent, Annex-B, read once at
/// Start. Linux-only and shell-level, like the Go app's.
fn open_dump() -> Option<std::fs::File> {
    let path = std::env::var_os("GAWK_DUMP_H264")?;
    match std::fs::File::create(&path) {
        Ok(f) => {
            log::info!(
                "dumping the H.264 elementary stream to {}",
                path.to_string_lossy()
            );
            Some(f)
        }
        Err(e) => {
            log::warn!(
                "GAWK_DUMP_H264 set but {} could not be created: {e}",
                path.to_string_lossy()
            );
            None
        }
    }
}

impl Media for Pipeline {
    fn info(&self) -> &MediaInfo {
        &self.info
    }

    fn force_idr(&self) {
        if let Some(s) = &self.supervisor {
            s.force_idr();
        }
    }

    fn take_thumbnail(&self) -> Option<Thumb> {
        self.sink.thumb.lock().unwrap().take()
    }

    fn capture_fps(&self) -> Option<f64> {
        self.sink.fps.lock().unwrap().fps()
    }

    fn audio_state(&self) -> String {
        self.audio.state()
    }

    fn audio_level(&self) -> f32 {
        self.audio.level()
    }

    fn audio_silence_hint(&self) -> bool {
        self.audio.silence_hint()
    }

    fn switch_audio_to_system(&self) {
        self.audio.switch_to_system();
    }

    /// Portal capture is damage-driven: a still screen delivers nothing,
    /// exactly like a minimized window, so no frame gap can tell them apart.
    /// The hint stays off rather than guess.
    fn minimized(&self) -> bool {
        false
    }

    fn capture_restarts(&self) -> u64 {
        self.restarts.load(Ordering::Relaxed)
    }

    fn share_mode(&self) -> Option<&'static str> {
        Some(self.share_mode)
    }

    fn audio_app(&self) -> Option<String> {
        self.audio.app()
    }

    fn audio_source_to_cache(&self) -> Option<String> {
        self.audio.cacheable_source()
    }

    fn take_failure(&self) -> Option<String> {
        self.failed.lock().unwrap().take()
    }

    fn shutdown(mut self: Box<Self>) {
        // Video to NULL first (synchronously), then audio and the control
        // plane, then the pump; the grant goes last, ending the portal
        // session and the compositor's sharing indicator.
        if let Some(s) = self.supervisor.take() {
            s.stop();
        }
        self.audio.stop();
        self.send_task.abort();
        if let Some(g) = self.grant.take() {
            g.release();
        }
    }

    fn as_any(&self) -> &dyn Any {
        self
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use gawk_encode::gst::BusError;
    use gawk_encode::gst_policy::{CASCADE, Culprit, Element, LivePlan};
    use gawk_engine::clock::{Clock, MonotonicClock};
    use gawk_engine::relay::{
        BoxFuture, CancelSignal, KeyframeOutcome, KeyframeWriter, RelaySession, SendDatagramError,
        ServerStream, SessionClose,
    };
    use std::time::{Duration, Instant};

    /// A relay that takes everything: datagrams accepted, every keyframe
    /// stream written at once, nothing ever received.
    pub(crate) struct NullRelay;
    struct NullWriter;
    impl KeyframeWriter for NullWriter {
        fn write(
            self: Box<Self>,
            _m: Vec<u8>,
            _c: CancelSignal,
        ) -> BoxFuture<'static, KeyframeOutcome> {
            Box::pin(async { KeyframeOutcome::Sent })
        }
        fn abort(self: Box<Self>, _code: u32) {}
    }
    impl RelaySession for NullRelay {
        fn send_datagram(&self, _d: &[u8]) -> Result<(), SendDatagramError> {
            Ok(())
        }
        fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
            Box::pin(async { Ok(Box::new(NullWriter) as Box<dyn KeyframeWriter>) })
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

    /// A media environment on the null relay, with its runtime.
    pub(crate) fn env() -> (MediaEnv, tokio::runtime::Runtime) {
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        let clock: Arc<dyn Clock> = Arc::new(MonotonicClock::new());
        let sender = Arc::new(Sender::new(Arc::new(NullRelay), clock.clone()));
        (
            MediaEnv {
                sender,
                clock,
                rt: rt.handle().clone(),
            },
            rt,
        )
    }

    pub(crate) fn x264_available() -> bool {
        gst::init().unwrap();
        let ok = gst::missing(&[Element::new("x264enc"), Element::new("videotestsrc")]).is_empty();
        if !ok {
            assert!(
                std::env::var("GAWK_REQUIRE_GST").is_err(),
                "GAWK_REQUIRE_GST is set but x264enc/videotestsrc are missing"
            );
            eprintln!("SKIP: x264enc or videotestsrc not installed");
        }
        ok
    }

    /// Everything the test controls about its launcher.
    #[derive(Default)]
    struct Control {
        fail_trials: Vec<Candidate>,
        last_attempt: u64,
        errors: Option<mpsc::Sender<AttemptError>>,
        launches: Vec<String>,
    }

    /// `videotestsrc` → a tee with the 1 Hz thumbnail branch → `x264enc`
    /// (test-only) → the planned tail. Only frames change hands; the walk,
    /// the supervisor, the sink, the gate and the pump are the product's.
    struct TestLauncher {
        sink: Arc<FrameSink>,
        errors: mpsc::Sender<AttemptError>,
        control: Arc<Mutex<Control>>,
    }

    fn test_plan() -> LivePlan {
        let video = vec![
            Element::new("videotestsrc")
                .prop("is-live", "true")
                .prop("pattern", "ball"),
            Element::caps("video/x-raw,format=I420,width=320,height=240,framerate=30/1"),
            Element::new("tee").named("t"),
            Element::new("queue")
                .prop("max-size-buffers", 2)
                .prop("max-size-time", 0)
                .prop("max-size-bytes", 0),
            // A GOP longer than any test waits: every IDR after the first is
            // a forced one.
            Element::new("x264enc")
                .named(gst_policy::ENCODER)
                .prop("tune", "zerolatency")
                .prop("speed-preset", "ultrafast")
                .prop("bframes", 0)
                .prop("key-int-max", 600)
                .prop("bitrate", 800),
        ];
        let mut video = video;
        video.extend(gst_policy::h264_tail());
        let thumb = vec![
            Element::new("queue")
                .prop("leaky", "downstream")
                .prop("max-size-buffers", 1)
                .prop("max-size-time", 0)
                .prop("max-size-bytes", 0),
            Element::new("videorate")
                .prop("drop-only", "true")
                .prop("max-rate", 1),
            Element::new("videoconvertscale"),
            Element::caps(format!(
                "video/x-raw,format=RGBA,width={},pixel-aspect-ratio=1/1",
                gst_policy::THUMB_WIDTH
            )),
            Element::new("appsink")
                .named(gst_policy::THUMB_SINK)
                .prop("sync", "false")
                .prop("drop", "true")
                .prop("max-buffers", 1),
        ];
        LivePlan {
            video,
            thumb,
            tee: Some("t"),
        }
    }

    impl Launcher for TestLauncher {
        fn trial(&mut self, c: Candidate) -> Result<(), String> {
            if self.control.lock().unwrap().fail_trials.contains(&c) {
                Err(format!("{} scripted to fail", c.element()))
            } else {
                Ok(())
            }
        }
        fn launch(
            &mut self,
            c: Candidate,
            rung: CaptureRung,
            attempt: u64,
        ) -> Result<Box<dyn Running>, String> {
            {
                let mut ctl = self.control.lock().unwrap();
                ctl.last_attempt = attempt;
                ctl.errors = Some(self.errors.clone());
                ctl.launches
                    .push(format!("{}/{}", c.element(), rung.label()));
            }
            let hooks = hooks_for(&self.sink, &self.errors, attempt);
            Live::start(&test_plan(), Candidate::Nvenc, 30, hooks)
                .map(|l| Box::new(LiveRun(l)) as Box<dyn Running>)
        }
    }

    fn assembly(order: Vec<Candidate>, pinned: bool, audio: AudioPlan) -> Assembly {
        Assembly {
            order,
            pinned,
            last_good: None,
            width: 320,
            height: 240,
            share_mode: "window",
            audio,
            grant: None,
            probe: Duration::from_millis(300),
        }
    }

    fn launcher(control: &Arc<Mutex<Control>>) -> MakeLauncher {
        let control = control.clone();
        Box::new(move |sink, errors| {
            Box::new(TestLauncher {
                sink,
                errors,
                control,
            })
        })
    }

    #[track_caller]
    fn wait_for(what: &str, mut done: impl FnMut() -> bool) {
        let t = Instant::now();
        while !done() {
            assert!(
                t.elapsed() < Duration::from_secs(15),
                "timed out waiting for {what}"
            );
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    /// The Linux media end to end, minus only the portal and the GPU: the
    /// cascade walk (a failed trial advancing it), frames through the sink,
    /// the gate and the pump, the codec from the live SPS, capture fps, the
    /// thumbnail, the resume re-prime, a capture rebuild that keeps the
    /// broadcast, an encoder death that ends it, and a synchronous stop.
    #[test]
    fn the_linux_media_runs_end_to_end_on_a_test_encoder() {
        if !x264_available() {
            return;
        }
        let (env, _rt) = env();
        let sender = env.sender.clone();
        let control = Arc::new(Mutex::new(Control {
            fail_trials: vec![Candidate::Vulkan],
            ..Default::default()
        }));
        let media = Pipeline::assemble(
            assembly(CASCADE.to_vec(), false, AudioPlan::Off),
            env,
            launcher(&control),
        )
        .unwrap_or_else(|_| panic!("the walk adopted nothing"));
        let media: Box<dyn Media> = Box::new(media);

        // The failed trial advanced the cascade; the first rung was adopted.
        assert_eq!(media.info().encoder, "nvh264enc");
        assert_eq!(media.info().family, "NVENC");
        assert_eq!(control.lock().unwrap().launches, ["nvh264enc/auto-capped"]);
        assert_eq!((media.info().width, media.info().height), (320, 240));

        wait_for("frames at the sender", || {
            sender.stats().encoded_frames >= 15
        });
        let st = sender.stats();
        assert!(
            st.codec.starts_with("avc1."),
            "the codec from the live SPS: {st:?}"
        );
        assert_eq!(st.keyframes, 1, "one IDR so far: the GOP outlasts the test");
        wait_for("capture fps", || {
            media.capture_fps().is_some_and(|f| f > 5.0)
        });
        wait_for("a thumbnail", || {
            media
                .take_thumbnail()
                .is_some_and(|(w, h, rgba)| w == 320 && h == 240 && rgba.len() == 320 * 240 * 4)
        });

        // The resume re-prime (Media::force_idr): an IDR within a few frames.
        media.force_idr();
        wait_for("a forced IDR", || sender.stats().keyframes >= 2);

        // A capture death mid-broadcast rebuilds on the same "grant": the
        // broadcast carries on and the rebuild is counted.
        let (attempt, errors) = {
            let c = control.lock().unwrap();
            (c.last_attempt, c.errors.clone().unwrap())
        };
        errors
            .send((
                attempt,
                BusError {
                    culprit: Culprit::Capture,
                    text: "pipewiresrc: stream error: unhandled format".into(),
                },
            ))
            .unwrap();
        wait_for("the rebuild", || media.capture_restarts() == 1);
        let after = sender.stats().encoded_frames;
        wait_for("frames after the rebuild", || {
            sender.stats().encoded_frames > after + 10
        });
        assert!(media.take_failure().is_none());
        assert_eq!(
            control.lock().unwrap().launches,
            ["nvh264enc/auto-capped", "nvh264enc/auto-capped"],
            "rebuilt on the encoder that was working, without re-trialling it"
        );

        // An encoder death is not a capture problem: the broadcast ends.
        let (attempt, errors) = {
            let c = control.lock().unwrap();
            (c.last_attempt, c.errors.clone().unwrap())
        };
        errors
            .send((
                attempt,
                BusError {
                    culprit: Culprit::Encode,
                    text: "nvh264enc: device lost".into(),
                },
            ))
            .unwrap();
        wait_for("the failure", || {
            media
                .take_failure()
                .is_some_and(|t| t.contains("hardware encoder stopped"))
        });

        assert_eq!(media.audio_state(), "off");
        assert_eq!(media.share_mode(), Some("window"));
        assert_eq!(media.audio_app(), None);
        assert_eq!(media.audio_source_to_cache(), None);
        assert!(!media.minimized());
        assert!(!media.audio_silence_hint());

        media.shutdown();
        let settled = sender.stats().encoded_frames;
        std::thread::sleep(Duration::from_millis(300));
        assert_eq!(
            sender.stats().encoded_frames,
            settled,
            "nothing after shutdown"
        );
    }

    #[test]
    fn nothing_passing_its_trial_is_the_refusal_and_a_pin_says_so() {
        if !x264_available() {
            return;
        }
        let control = Arc::new(Mutex::new(Control {
            fail_trials: CASCADE.to_vec(),
            ..Default::default()
        }));
        let (env, _rt) = env();
        let r = Pipeline::assemble(
            assembly(CASCADE.to_vec(), false, AudioPlan::Off),
            env,
            launcher(&control),
        );
        assert!(matches!(r, Err(StartFailure::NoHardwareEncoder)));

        let (env2, _rt2) = super::tests::env();
        let r = Pipeline::assemble(
            assembly(vec![Candidate::Va], true, AudioPlan::Off),
            env2,
            launcher(&control),
        );
        match r {
            Err(StartFailure::Capture(t)) => {
                assert!(t.contains("pinned in the config (vah264enc)"), "{t}")
            }
            _ => panic!("a pinned encoder that fails is named"),
        }
    }

    #[test]
    fn walk_errors_map_to_their_sentences() {
        assert!(matches!(
            start_failure(WalkError::CaptureFormat(vec!["pipewiresrc: x".into()])),
            StartFailure::Capture(t) if t.contains("could not agree on a frame format")
        ));
        assert!(matches!(
            start_failure(WalkError::Stopped),
            StartFailure::Capture(_)
        ));
    }

    #[test]
    fn the_dump_tap_is_opt_in() {
        // Unset in the test environment: no file, no error.
        if std::env::var_os("GAWK_DUMP_H264").is_none() {
            assert!(open_dump().is_none());
        }
    }
}
