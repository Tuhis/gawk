//! The in-process GStreamer half of the Linux encode path (R56, docs/58
//! D4/D5): builds what [`crate::gst_policy`] plans, runs the trial gate
//! through the shared [`cascade::TrialRunner`], and runs the live pipeline —
//! appsink callbacks, forced IDRs, and a bus thread that attributes every
//! error to its element.
//!
//! In-process by decision (OD3), reversing docs/19 D3's subprocess: the
//! frame still never touches our address space on a zero-copy path — DMA-BUFs
//! go from the portal to the GPU's encode block, and what reaches the appsink
//! is already H.264. What changed is the crash posture, which is exactly what
//! the on-hardware pass measures (docs/58 V-1).

use crate::cascade::{self, TrialAu, TrialRun};
use crate::gst_policy::{self, Candidate, Culprit, Element, LivePlan};
use crate::h264;
use crate::vt_policy::KeyframeCadence;
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;
use gstreamer_video as gst_video;
use std::panic::AssertUnwindSafe;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

/// Initialises GStreamer once per process. The error is a sentence: with no
/// GStreamer there is no capture at all.
pub fn init() -> Result<(), String> {
    static INIT: OnceLock<Result<(), String>> = OnceLock::new();
    INIT.get_or_init(|| {
        gst::init().map_err(|e| format!("GStreamer could not start: {e}"))?;
        log::info!("GStreamer {}", gst::version_string());
        Ok(())
    })
    .clone()
}

/// Factories in `elements` that this machine does not have, deduplicated.
pub fn missing(elements: &[Element]) -> Vec<&'static str> {
    let mut out: Vec<&'static str> = Vec::new();
    for e in elements {
        if gst::ElementFactory::find(e.factory).is_none() && !out.contains(&e.factory) {
            out.push(e.factory);
        }
    }
    out
}

/// Makes one planned element, properties parsed against their real types.
/// A property the element lacks is an error, never a panic: a GStreamer
/// version that renamed one must fail this candidate, not the app.
pub fn make(spec: &Element) -> Result<gst::Element, String> {
    let mut b = gst::ElementFactory::make(spec.factory);
    if let Some(name) = spec.name {
        b = b.name(name);
    }
    let el = b
        .build()
        .map_err(|_| format!("{}: element not available", spec.factory))?;
    for (key, value) in &spec.props {
        let pspec = el
            .find_property(key)
            .ok_or_else(|| format!("{}: no property \"{key}\"", spec.factory))?;
        let v = gst::glib::Value::deserialize_with_pspec(value, &pspec)
            .map_err(|_| format!("{}: bad value {key}={value}", spec.factory))?;
        el.set_property_from_value(key, &v);
    }
    Ok(el)
}

/// Builds and links a chain into `bin`, returning its elements in order.
fn add_chain(bin: &gst::Pipeline, specs: &[Element]) -> Result<Vec<gst::Element>, String> {
    let els = specs.iter().map(make).collect::<Result<Vec<_>, _>>()?;
    bin.add_many(&els)
        .map_err(|e| format!("could not assemble the pipeline: {e}"))?;
    for pair in els.windows(2) {
        pair[0].link(&pair[1]).map_err(|_| {
            format!(
                "could not link {} to {}",
                factory_of(&pair[0]),
                factory_of(&pair[1])
            )
        })?;
    }
    Ok(els)
}

fn factory_of(el: &gst::Element) -> String {
    el.factory()
        .map(|f| f.name().to_string())
        .unwrap_or_else(|| el.name().to_string())
}

/// The one clock (D4): every pipeline runs on the system clock in its
/// monotonic mode, so `base_time + running time` is `CLOCK_MONOTONIC` — the clock the
/// engine's `Instant` reads on Linux. `pipewiresrc` provides a clock of its
/// own and a pipeline would otherwise pick it; this pins ours.
pub fn monotonic_clock() -> gst::Clock {
    let clock = gst::SystemClock::obtain();
    clock.set_property("clock-type", gst::ClockType::Monotonic);
    clock
}

/// Builds a pipeline from a plan, on the monotonic system clock.
pub fn build(plan: &LivePlan) -> Result<gst::Pipeline, String> {
    let pipeline = gst::Pipeline::new();
    pipeline.use_clock(Some(&monotonic_clock()));
    add_chain(&pipeline, &plan.video)?;
    if let Some(tee) = plan.tee {
        let tee = pipeline
            .by_name(tee)
            .ok_or("the plan's tee is missing from the pipeline")?;
        let branch = add_chain(&pipeline, &plan.thumb)?;
        tee.link(&branch[0])
            .map_err(|_| "could not link the thumbnail branch".to_owned())?;
    }
    Ok(pipeline)
}

/// A bus error, attributed to the element that raised it.
#[derive(Debug, Clone)]
pub struct BusError {
    pub culprit: Culprit,
    /// "<factory>: <message>" — the factory first, so the failure trail and
    /// `gst_policy::all_inside_pipewiresrc` can read it.
    pub text: String,
}

fn bus_error(c: Candidate, msg: &gst::message::Error) -> BusError {
    let factory = msg
        .src()
        .and_then(|s| s.downcast_ref::<gst::Element>())
        .map(factory_of)
        .unwrap_or_else(|| "pipeline".into());
    let mut text = format!("{factory}: {}", msg.error());
    if let Some(debug) = msg.debug() {
        log::debug!("{factory} error detail: {debug}");
    }
    if text.len() > 300 {
        text.truncate(300);
    }
    BusError {
        culprit: gst_policy::culprit(c, &factory),
        text,
    }
}

/// A buffer's running time under `segment` — the time the pipeline clock
/// relates to, `base_time + running time` being the clock reading. Raw PTS
/// is not it: encoders may shift their output segment (x264enc and other
/// GstVideoEncoder subclasses move it by 1000 hours to keep DTS positive),
/// so every timestamp is taken through the segment of the pad it was seen on.
fn running_time(segment: Option<&gst::Segment>, pts: gst::ClockTime) -> Option<gst::ClockTime> {
    match segment {
        Some(seg) => seg
            .downcast_ref::<gst::ClockTime>()
            .and_then(|s| s.to_running_time(pts)),
        None => Some(pts),
    }
}

/// The running time of a buffer seen on `pad`, through its sticky segment.
fn pad_running_time(pad: &gst::Pad, pts: gst::ClockTime) -> Option<gst::ClockTime> {
    let seg = pad
        .sticky_event::<gst::event::Segment>(0)
        .map(|e| e.segment().clone());
    running_time(seg.as_ref(), pts)
}

/// Converts a buffer time to the 100 ns ticks the shared trial validator
/// compares in.
fn to_100ns(t: gst::ClockTime) -> i64 {
    (t.nseconds() / 100) as i64
}

/// The keyframe rule at the encoder's input (D5): the Vulkan candidate's
/// forced cadence and every candidate's on-demand IDR (resume re-prime,
/// capture rebuild) go through one probe, so the two can never race.
struct KeyRule {
    cadence: Option<KeyframeCadence>,
}

impl KeyRule {
    fn new(c: Candidate, fps: u32) -> Self {
        Self {
            cadence: c.needs_forced_cadence().then(|| KeyframeCadence::new(fps)),
        }
    }

    /// Whether this input frame must be forced to an IDR.
    fn next(&mut self, forced: bool) -> bool {
        match &mut self.cadence {
            Some(c) => c.next(forced),
            None => forced,
        }
    }
}

fn force_key_unit() -> gst::Event {
    gst_video::DownstreamForceKeyUnitEvent::builder()
        .all_headers(true)
        .build()
}

/// The trial gate for one candidate (docs/58 D5): `videotestsrc` through the
/// candidate's convert and encoder, judged by the shared
/// [`cascade::validate_trial`]. It feeds a fixed number of frames, forces an
/// IDR half a GOP after the first cadence boundary, and snapshots the output
/// BEFORE any drain — so an encoder holding frames back (lookahead, B-frame
/// reordering) fails the "≤ 1 frame retained" row instead of hiding behind
/// an EOS flush.
pub struct GstTrialRunner {
    pub fps: u32,
    pub peak_bps: u32,
}

/// How long a trial may take in all, before it is a failure.
const TRIAL_BUDGET: Duration = Duration::from_secs(8);
/// After the last input, how long outputs may still trickle in.
const TRIAL_SETTLE: Duration = Duration::from_millis(400);

impl cascade::TrialRunner for GstTrialRunner {
    fn run(&mut self, candidate: &cascade::Candidate) -> Result<TrialRun, String> {
        let c = gst_policy::find(&candidate.id)
            .ok_or_else(|| format!("{} is not a cascade element", candidate.id))?;
        init()?;
        let specs = gst_policy::trial_plan(c, self.fps, self.peak_bps);
        let missing = missing(&specs);
        if !missing.is_empty() {
            return Err(format!(
                "{} not installed (package {})",
                missing.join(", "),
                c.package()
            ));
        }
        run_trial(c, self.fps, specs)
    }
}

#[derive(Default)]
struct TrialState {
    inputs: Vec<i64>,
    aus: Vec<TrialAu>,
}

/// Runs the trial over a given plan. [`GstTrialRunner`] passes
/// [`gst_policy::trial_plan`]; the CI integration test substitutes a software
/// encoder (`x264enc`, test-only — never a production candidate) to prove the
/// plumbing a runner without a GPU can see.
pub fn run_trial(c: Candidate, fps: u32, specs: Vec<Element>) -> Result<TrialRun, String> {
    let (frames, forced_at) = crate::vt_policy::trial_plan(fps);
    let pipeline = gst::Pipeline::new();
    pipeline.use_clock(Some(&monotonic_clock()));
    let result = (|| {
        add_chain(&pipeline, &specs)?;
        let state = Arc::new(Mutex::new(TrialState::default()));
        let encoder = pipeline
            .by_name(gst_policy::ENCODER)
            .ok_or("no encoder in the trial")?;
        let sink_pad = encoder
            .static_pad("sink")
            .ok_or("encoder has no sink pad")?;
        {
            let state = state.clone();
            let rule = Mutex::new(KeyRule::new(c, fps));
            // A BLOCKING probe: after the last trial frame the source's
            // streaming thread parks here (Ok = stay blocked) until teardown
            // flushes it, so the encoder sees exactly `frames` inputs. Every
            // earlier frame passes (Pass). Dropping instead trips a
            // gst_mini_object_unref(NULL) critical in the binding's
            // trampoline.
            sink_pad.add_probe(
                gst::PadProbeType::BLOCK | gst::PadProbeType::BUFFER,
                move |pad, info| {
                    let n = state.lock().unwrap().inputs.len();
                    if n >= frames {
                        return gst::PadProbeReturn::Ok;
                    }
                    let Some(pts) = info
                        .buffer()
                        .and_then(|b| b.pts())
                        .and_then(|p| pad_running_time(pad, p))
                    else {
                        return gst::PadProbeReturn::Pass;
                    };
                    if rule.lock().unwrap().next(n == forced_at) {
                        pad.send_event(force_key_unit());
                    }
                    state.lock().unwrap().inputs.push(to_100ns(pts));
                    gst::PadProbeReturn::Pass
                },
            );
        }
        let sink = appsink(&pipeline, gst_policy::VIDEO_SINK)?;
        {
            let state = state.clone();
            sink.set_callbacks(
                gst_app::AppSinkCallbacks::builder()
                    .new_sample(move |s| {
                        let sample = s.pull_sample().map_err(|_| gst::FlowError::Eos)?;
                        if let Some(buf) = sample.buffer()
                            && let Some(pts) =
                                buf.pts().and_then(|p| running_time(sample.segment(), p))
                            && let Ok(map) = buf.map_readable()
                        {
                            state.lock().unwrap().aus.push(TrialAu {
                                data: map.as_slice().to_vec(),
                                time_100ns: to_100ns(pts),
                            });
                        }
                        Ok(gst::FlowSuccess::Ok)
                    })
                    .build(),
            );
        }
        pipeline
            .set_state(gst::State::Playing)
            .map_err(|_| "the trial pipeline would not start".to_owned())?;
        let bus = pipeline.bus().ok_or("no bus")?;
        let started = Instant::now();
        let mut fed_at: Option<Instant> = None;
        loop {
            if let Some(msg) = bus.timed_pop_filtered(
                gst::ClockTime::from_mseconds(20),
                &[gst::MessageType::Error],
            ) && let gst::MessageView::Error(e) = msg.view()
            {
                return Err(bus_error(c, e).text);
            }
            let (fed, out) = {
                let st = state.lock().unwrap();
                (st.inputs.len(), st.aus.len())
            };
            if fed >= frames && fed_at.is_none() {
                fed_at = Some(Instant::now());
            }
            if let Some(at) = fed_at
                && (at.elapsed() >= TRIAL_SETTLE || out >= fed)
            {
                break;
            }
            if started.elapsed() > TRIAL_BUDGET {
                return Err(format!(
                    "trial timed out ({fed} frames in, {out} out after {}s)",
                    TRIAL_BUDGET.as_secs()
                ));
            }
        }
        let st = state.lock().unwrap();
        Ok(TrialRun {
            inputs_fed: st.inputs.len(),
            aus: st.aus.clone(),
            input_times_100ns: st.inputs.clone(),
            forced_idr_at: Some(forced_at),
            sequence_header: Vec::new(),
        })
    })();
    let _ = pipeline.set_state(gst::State::Null);
    result
}

fn appsink(pipeline: &gst::Pipeline, name: &str) -> Result<gst_app::AppSink, String> {
    pipeline
        .by_name(name)
        .and_then(|e| e.dynamic_cast::<gst_app::AppSink>().ok())
        .ok_or_else(|| format!("no appsink \"{name}\" in the pipeline"))
}

/// One encoded access unit off the live appsink.
pub struct LiveAu {
    /// Annex-B, SPS/PPS before every IDR (`h264parse config-interval=-1`).
    pub data: Vec<u8>,
    /// `base_time + running time` on the pipeline's monotonic clock, ns — capture
    /// time on `CLOCK_MONOTONIC` (D4's one clock).
    pub capture_ns: u64,
    pub idr: bool,
}

/// What the live pipeline reports, from its streaming and bus threads. Each
/// call runs under a `catch_unwind` fence (docs/38 D3): a panic becomes an
/// error through the normal path instead of a thread silently gone.
pub struct LiveHooks {
    pub on_au: Box<dyn Fn(LiveAu) + Send + Sync>,
    /// Every frame admitted to the encoder: `CLOCK_MONOTONIC` ns.
    pub on_input: Box<dyn Fn(u64) + Send + Sync>,
    /// A 1 Hz RGBA thumbnail: width, height, pixels.
    pub on_thumb: Box<dyn Fn(u32, u32, Vec<u8>) + Send + Sync>,
    pub on_error: Box<dyn Fn(BusError) + Send + Sync>,
}

/// A running live pipeline.
pub struct Live {
    pipeline: gst::Pipeline,
    force: Arc<AtomicBool>,
    stop: Arc<AtomicBool>,
    bus_thread: Option<std::thread::JoinHandle<()>>,
    inputs: Arc<AtomicU64>,
}

impl Live {
    /// Builds and starts one attempt. Returns once the pipeline is PLAYING
    /// (or failed to get there); the live-probe window is the caller's.
    pub fn start(
        plan: &LivePlan,
        c: Candidate,
        fps: u32,
        hooks: LiveHooks,
    ) -> Result<Self, String> {
        init()?;
        let missing = missing(&plan.video);
        if !missing.is_empty() {
            return Err(format!(
                "{} not installed (package {})",
                missing.join(", "),
                c.package()
            ));
        }
        let pipeline = build(plan)?;
        let hooks = Arc::new(hooks);
        let force = Arc::new(AtomicBool::new(false));
        let inputs = Arc::new(AtomicU64::new(0));

        let encoder = pipeline
            .by_name(gst_policy::ENCODER)
            .ok_or("no encoder in the plan")?;
        let sink_pad = encoder
            .static_pad("sink")
            .ok_or("encoder has no sink pad")?;
        {
            let force = force.clone();
            let hooks = hooks.clone();
            let inputs = inputs.clone();
            let rule = Mutex::new(KeyRule::new(c, fps));
            let enc = encoder.clone();
            sink_pad.add_probe(gst::PadProbeType::BUFFER, move |pad, info| {
                let forced = force.swap(false, Ordering::AcqRel);
                if rule.lock().unwrap().next(forced) {
                    pad.send_event(force_key_unit());
                }
                inputs.fetch_add(1, Ordering::Relaxed);
                if let (Some(rt), Some(base)) = (
                    info.buffer()
                        .and_then(|b| b.pts())
                        .and_then(|p| pad_running_time(pad, p)),
                    enc.base_time(),
                ) {
                    let ns = (base + rt).nseconds();
                    fence("input", || (hooks.on_input)(ns));
                }
                gst::PadProbeReturn::Ok
            });
        }

        let video = appsink(&pipeline, gst_policy::VIDEO_SINK)?;
        {
            let hooks = hooks.clone();
            video.set_callbacks(
                gst_app::AppSinkCallbacks::builder()
                    .new_sample(move |s| {
                        let sample = s.pull_sample().map_err(|_| gst::FlowError::Eos)?;
                        let Some(buf) = sample.buffer() else {
                            return Ok(gst::FlowSuccess::Ok);
                        };
                        let (Some(rt), Some(base)) = (
                            buf.pts().and_then(|p| running_time(sample.segment(), p)),
                            s.base_time(),
                        ) else {
                            return Ok(gst::FlowSuccess::Ok);
                        };
                        let Ok(map) = buf.map_readable() else {
                            return Ok(gst::FlowSuccess::Ok);
                        };
                        let data = map.as_slice().to_vec();
                        let au = LiveAu {
                            idr: h264::has_idr(&data),
                            data,
                            capture_ns: (base + rt).nseconds(),
                        };
                        fence("video", || (hooks.on_au)(au));
                        Ok(gst::FlowSuccess::Ok)
                    })
                    .build(),
            );
        }
        if plan.tee.is_some() {
            let thumb = appsink(&pipeline, gst_policy::THUMB_SINK)?;
            let hooks = hooks.clone();
            thumb.set_callbacks(
                gst_app::AppSinkCallbacks::builder()
                    .new_sample(move |s| {
                        let sample = s.pull_sample().map_err(|_| gst::FlowError::Eos)?;
                        let info = sample
                            .caps()
                            .and_then(|c| gst_video::VideoInfo::from_caps(c).ok());
                        if let (Some(info), Some(buf)) = (info, sample.buffer())
                            && let Ok(map) = buf.map_readable()
                        {
                            let (w, h) = (info.width(), info.height());
                            let stride = info.stride()[0] as usize;
                            let mut rgba = Vec::with_capacity((w * h * 4) as usize);
                            for row in map.as_slice().chunks(stride).take(h as usize) {
                                rgba.extend_from_slice(&row[..(w * 4) as usize]);
                            }
                            fence("thumbnail", || (hooks.on_thumb)(w, h, rgba));
                        }
                        Ok(gst::FlowSuccess::Ok)
                    })
                    .build(),
            );
        }

        let stop = Arc::new(AtomicBool::new(false));
        let bus = pipeline.bus().ok_or("no bus")?;
        let bus_thread = {
            let stop = stop.clone();
            let hooks = hooks.clone();
            std::thread::Builder::new()
                .name("gst-bus".into())
                .spawn(move || {
                    while !stop.load(Ordering::Acquire) {
                        let Some(msg) = bus.timed_pop_filtered(
                            gst::ClockTime::from_mseconds(100),
                            &[gst::MessageType::Error, gst::MessageType::Eos],
                        ) else {
                            continue;
                        };
                        let err = match msg.view() {
                            gst::MessageView::Error(e) => bus_error(c, e),
                            gst::MessageView::Eos(_) => BusError {
                                culprit: Culprit::Capture,
                                text: "pipewiresrc: the capture stream ended".into(),
                            },
                            _ => continue,
                        };
                        fence("bus", || (hooks.on_error)(err));
                    }
                })
                .map_err(|e| format!("could not start the bus thread: {e}"))?
        };

        let live = Self {
            pipeline,
            force,
            stop,
            bus_thread: Some(bus_thread),
            inputs,
        };
        if live.pipeline.set_state(gst::State::Playing).is_err() {
            // The bus carries the element's own reason; give it a moment.
            return Err(live.stop_with_error("the capture pipeline would not start"));
        }
        Ok(live)
    }

    /// Stops the attempt and returns its first bus error, or `fallback`.
    fn stop_with_error(mut self, fallback: &str) -> String {
        self.stop.store(true, Ordering::Release);
        if let Some(t) = self.bus_thread.take() {
            let _ = t.join();
        }
        let text = self
            .pipeline
            .bus()
            .and_then(|b| b.pop_filtered(&[gst::MessageType::Error]))
            .and_then(|m| match m.view() {
                gst::MessageView::Error(e) => Some(format!(
                    "{}: {}",
                    e.src()
                        .and_then(|s| s.downcast_ref::<gst::Element>())
                        .map(factory_of)
                        .unwrap_or_else(|| "pipeline".into()),
                    e.error()
                )),
                _ => None,
            })
            .unwrap_or_else(|| fallback.to_owned());
        let _ = self.pipeline.set_state(gst::State::Null);
        text
    }

    /// The next frame into the encoder becomes an IDR (the resume re-prime,
    /// docs/38 D5 — new on Linux, G10).
    pub fn force_idr(&self) {
        self.force.store(true, Ordering::Release);
    }

    /// Frames admitted to the encoder so far.
    pub fn inputs(&self) -> u64 {
        self.inputs.load(Ordering::Relaxed)
    }

    /// Stops synchronously: NULL before returning, so no capture outlives
    /// the call (the `finish()` incident class, docs/19).
    pub fn stop(mut self) {
        self.stop.store(true, Ordering::Release);
        let _ = self.pipeline.set_state(gst::State::Null);
        if let Some(t) = self.bus_thread.take() {
            let _ = t.join();
        }
    }
}

impl Drop for Live {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        let _ = self.pipeline.set_state(gst::State::Null);
        if let Some(t) = self.bus_thread.take() {
            let _ = t.join();
        }
    }
}

/// Runs a hook under a panic fence. A panicking hook is logged and the
/// streaming thread carries on; the hook owner decides what a panic means.
fn fence(what: &str, f: impl FnOnce()) {
    if std::panic::catch_unwind(AssertUnwindSafe(f)).is_err() {
        log::error!("{what} callback panicked");
    }
}
