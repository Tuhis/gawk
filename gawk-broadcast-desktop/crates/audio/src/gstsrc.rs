//! Audio capture on Linux (docs/58 D7): the Go app's system-audio cascade in
//! GStreamer, and the app-sink monitor of docs/39, delivered as F32
//! interleaved 48 kHz stereo into the shared [`crate::lane::Lane`]. Encoding
//! moved out of GStreamer: the R25 contract (libopus, 20 ms, CBR, the TOC
//! gate) is implemented once for all three OSes, and gst's `opusenc` is
//! retired.
//!
//! A separate pipeline from video, on the same monotonic system clock: an
//! audio failure can never touch video (R25 Decision 6), and nothing needs
//! muxing any more.

use gawk_engine::media::{AUDIO_CHANNELS, AUDIO_SAMPLE_RATE};
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// One place audio can come from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Source {
    /// PipeWire capture of the default output's monitor — follows a device
    /// switch.
    PipewireMonitor,
    /// pipewire-pulse / PulseAudio's default monitor, bound at start.
    PulseDefaultMonitor,
    /// The `audioDevice` pin (OD12): a named PulseAudio device.
    PulseDevice(String),
    /// Our capture sink's monitor, by `object.serial` (docs/39 D3).
    AppSinkMonitor(u32),
}

/// The system cascade, in order (the Go `audioCascade`).
pub const SYSTEM_CASCADE: [Source; 2] = [Source::PipewireMonitor, Source::PulseDefaultMonitor];

impl Source {
    /// The stable name: the `lastGoodAudioSource` cache value, the
    /// advertised format's `source`, the diagnostics' `audioSource`.
    pub fn name(&self) -> &'static str {
        match self {
            Self::PipewireMonitor => "pipewire-monitor",
            Self::PulseDefaultMonitor => "pulse-default-monitor",
            Self::PulseDevice(_) => "pulse-device",
            Self::AppSinkMonitor(_) => "app-sink-monitor",
        }
    }

    /// A cascade entry by its cached name (never the device pin or the app
    /// sink, which are not cacheable).
    pub fn cached(name: &str) -> Option<Self> {
        SYSTEM_CASCADE.into_iter().find(|s| s.name() == name)
    }

    /// The source element and its properties.
    pub fn element(&self) -> (&'static str, Vec<(&'static str, String)>) {
        match self {
            Self::PipewireMonitor => (
                "pipewiresrc",
                vec![("stream-properties", "props,stream.capture.sink=true".into())],
            ),
            Self::PulseDefaultMonitor => ("pulsesrc", vec![("device", "@DEFAULT_MONITOR@".into())]),
            Self::PulseDevice(d) => ("pulsesrc", vec![("device", d.clone())]),
            Self::AppSinkMonitor(serial) => (
                "pipewiresrc",
                vec![
                    ("target-object", serial.to_string()),
                    ("stream-properties", "props,stream.capture.sink=true".into()),
                ],
            ),
        }
    }
}

/// The order to try for a start (the Go `SelectAudioSource`): a device pin
/// alone; else the cached winner first, then the cascade.
pub fn order(device: &str, last_good: &str) -> Vec<Source> {
    let device = device.trim();
    if !device.is_empty() {
        return vec![Source::PulseDevice(device.to_owned())];
    }
    let mut out = Vec::new();
    if let Some(s) = Source::cached(last_good) {
        out.push(s);
    }
    for s in SYSTEM_CASCADE {
        if !out.contains(&s) {
            out.push(s);
        }
    }
    out
}

/// The fixed caps the appsink takes: what the shared lane's F32 path reads.
pub fn caps() -> String {
    format!(
        "audio/x-raw,format=F32LE,rate={AUDIO_SAMPLE_RATE},channels={AUDIO_CHANNELS},layout=interleaved"
    )
}

/// Buffers a trial must see (~500 ms at the default quantum).
pub const TRIAL_BUFFERS: usize = 25;
/// The whole audio pre-flight's budget, across every candidate — one budget
/// keeps the worst case flat as candidates are added.
pub const PROBE_BUDGET: Duration = Duration::from_secs(8);

fn make(factory: &str, props: &[(&str, String)]) -> Result<gst::Element, String> {
    let el = gst::ElementFactory::make(factory)
        .build()
        .map_err(|_| format!("{factory}: element not available"))?;
    for (key, value) in props {
        let pspec = el
            .find_property(key)
            .ok_or_else(|| format!("{factory}: no property \"{key}\""))?;
        let v = gst::glib::Value::deserialize_with_pspec(value, &pspec)
            .map_err(|_| format!("{factory}: bad value {key}={value}"))?;
        el.set_property_from_value(key, &v);
    }
    Ok(el)
}

fn build(source: &Source) -> Result<(gst::Pipeline, gst_app::AppSink), String> {
    gst::init().map_err(|e| format!("GStreamer could not start: {e}"))?;
    let (factory, props) = source.element();
    let src = make(factory, &props)?;
    let convert = make("audioconvert", &[])?;
    let resample = make("audioresample", &[])?;
    let filter = make("capsfilter", &[("caps", caps())])?;
    let sink = gst_app::AppSink::builder().sync(false).build();
    let pipeline = gst::Pipeline::new();
    // The one clock (docs/58 D4): pipewiresrc offers its own and a pipeline
    // would otherwise pick it.
    let clock = gst::SystemClock::obtain();
    clock.set_property("clock-type", gst::ClockType::Monotonic);
    pipeline.use_clock(Some(&clock));
    pipeline
        .add_many([&src, &convert, &resample, &filter, sink.upcast_ref()])
        .map_err(|e| format!("could not assemble the audio pipeline: {e}"))?;
    gst::Element::link_many([&src, &convert, &resample, &filter, sink.upcast_ref()])
        .map_err(|_| format!("{factory}: could not link the audio pipeline"))?;
    Ok((pipeline, sink))
}

fn first_error(pipeline: &gst::Pipeline) -> Option<String> {
    let bus = pipeline.bus()?;
    let msg = bus.pop_filtered(&[gst::MessageType::Error])?;
    match msg.view() {
        gst::MessageView::Error(e) => Some(format!(
            "{}: {}",
            e.src().map(|s| s.name().to_string()).unwrap_or_default(),
            e.error()
        )),
        _ => None,
    }
}

/// Trials one source: does it negotiate and produce samples? Loudness is
/// not the test — a monitor of an idle sink clocks out silence, and what
/// the trial proves is the capture path.
pub fn trial(source: &Source, deadline: Instant) -> Result<(), String> {
    let (pipeline, sink) = build(source)?;
    let got = Arc::new(Mutex::new(0usize));
    {
        let got = got.clone();
        sink.set_callbacks(
            gst_app::AppSinkCallbacks::builder()
                .new_sample(move |s| {
                    let _ = s.pull_sample();
                    *got.lock().unwrap() += 1;
                    Ok(gst::FlowSuccess::Ok)
                })
                .build(),
        );
    }
    let result = (|| {
        pipeline
            .set_state(gst::State::Playing)
            .map_err(|_| first_error(&pipeline).unwrap_or_else(|| "would not start".into()))?;
        loop {
            if *got.lock().unwrap() >= TRIAL_BUFFERS {
                return Ok(());
            }
            if let Some(e) = first_error(&pipeline) {
                return Err(e);
            }
            if Instant::now() > deadline {
                return Err(format!(
                    "no samples within the audio probe budget ({} buffers seen)",
                    *got.lock().unwrap()
                ));
            }
            std::thread::sleep(Duration::from_millis(20));
        }
    })();
    let _ = pipeline.set_state(gst::State::Null);
    result
}

/// Picks the first source in `order` whose trial passes, within one budget.
/// Every failure names its source, for the debug log.
pub fn select(order: &[Source]) -> Result<Source, Vec<String>> {
    let deadline = Instant::now() + PROBE_BUDGET;
    let mut tried = Vec::new();
    for s in order {
        match trial(s, deadline) {
            Ok(()) => return Ok(s.clone()),
            Err(why) => tried.push(format!("{}: {why}", s.name())),
        }
    }
    Err(tried)
}

/// One block of PCM off the appsink.
pub struct Pcm<'a> {
    /// F32LE interleaved stereo bytes.
    pub bytes: &'a [u8],
    pub frames: usize,
    /// `base_time + running time`, CLOCK_MONOTONIC ns; `None` when the
    /// buffer carried no time (the caller stamps on arrival).
    pub capture_ns: Option<u64>,
}

/// A running audio capture.
pub struct Capture {
    pipeline: gst::Pipeline,
    stop: Arc<AtomicBool>,
    bus: Option<std::thread::JoinHandle<()>>,
}

impl Capture {
    /// Starts `source`, delivering PCM to `on_pcm` on the streaming thread
    /// and the first error (then nothing) to `on_error`. Both run under a
    /// panic fence: audio is subordinate, and a panic in it must never take
    /// a thread the broadcast needs.
    pub fn start(
        source: &Source,
        on_pcm: impl Fn(Pcm<'_>) + Send + Sync + 'static,
        on_error: impl Fn(String) + Send + Sync + 'static,
    ) -> Result<Self, String> {
        let (pipeline, sink) = build(source)?;
        let failed = Arc::new(AtomicBool::new(false));
        let on_error = Arc::new(on_error);
        {
            let failed = failed.clone();
            let on_error = on_error.clone();
            sink.set_callbacks(
                gst_app::AppSinkCallbacks::builder()
                    .new_sample(move |s| {
                        let sample = s.pull_sample().map_err(|_| gst::FlowError::Eos)?;
                        if failed.load(Ordering::Acquire) {
                            return Ok(gst::FlowSuccess::Ok);
                        }
                        let Some(buf) = sample.buffer() else {
                            return Ok(gst::FlowSuccess::Ok);
                        };
                        let rt = buf.pts().and_then(|pts| {
                            sample
                                .segment()
                                .and_then(|seg| seg.downcast_ref::<gst::ClockTime>())
                                .and_then(|seg| seg.to_running_time(pts))
                        });
                        let capture_ns = rt.zip(s.base_time()).map(|(rt, b)| (b + rt).nseconds());
                        let Ok(map) = buf.map_readable() else {
                            return Ok(gst::FlowSuccess::Ok);
                        };
                        let bytes = map.as_slice();
                        let frames = bytes.len() / (4 * usize::from(AUDIO_CHANNELS));
                        let pcm = Pcm {
                            bytes,
                            frames,
                            capture_ns,
                        };
                        if std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| on_pcm(pcm)))
                            .is_err()
                        {
                            failed.store(true, Ordering::Release);
                            on_error("the audio lane panicked".into());
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
            let on_error = on_error.clone();
            std::thread::Builder::new()
                .name("gst-audio-bus".into())
                .spawn(move || {
                    while !stop.load(Ordering::Acquire) {
                        let Some(msg) = bus.timed_pop_filtered(
                            gst::ClockTime::from_mseconds(100),
                            &[gst::MessageType::Error, gst::MessageType::Eos],
                        ) else {
                            continue;
                        };
                        let text = match msg.view() {
                            gst::MessageView::Error(e) => format!(
                                "{}: {}",
                                e.src().map(|s| s.name().to_string()).unwrap_or_default(),
                                e.error()
                            ),
                            _ => "the audio stream ended".into(),
                        };
                        if !failed.swap(true, Ordering::AcqRel) {
                            let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                                on_error(text)
                            }));
                        }
                    }
                })
                .map_err(|e| format!("could not start the audio bus thread: {e}"))?
        };
        let cap = Self {
            pipeline,
            stop,
            bus: Some(bus_thread),
        };
        cap.pipeline
            .set_state(gst::State::Playing)
            .map_err(|_| "the audio pipeline would not start".to_owned())?;
        Ok(cap)
    }

    /// Stops synchronously.
    pub fn stop(mut self) {
        self.shutdown();
    }

    fn shutdown(&mut self) {
        self.stop.store(true, Ordering::Release);
        let _ = self.pipeline.set_state(gst::State::Null);
        if let Some(t) = self.bus.take() {
            let _ = t.join();
        }
    }
}

impl Drop for Capture {
    fn drop(&mut self) {
        self.shutdown();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_cascade_is_the_go_apps_and_the_pin_stands_alone() {
        assert_eq!(
            order("", "").iter().map(Source::name).collect::<Vec<_>>(),
            ["pipewire-monitor", "pulse-default-monitor"]
        );
        assert_eq!(
            order("", "pulse-default-monitor")
                .iter()
                .map(Source::name)
                .collect::<Vec<_>>(),
            ["pulse-default-monitor", "pipewire-monitor"],
            "the cached winner re-verified first"
        );
        assert_eq!(
            order(" alsa.monitor ", "pipewire-monitor"),
            [Source::PulseDevice("alsa.monitor".into())]
        );
        assert_eq!(
            Source::cached("pulse-device"),
            None,
            "a pin is not cacheable"
        );
        assert_eq!(Source::cached("app-sink-monitor"), None);
    }

    #[test]
    fn sources_are_the_go_elements() {
        assert_eq!(
            Source::PipewireMonitor.element(),
            (
                "pipewiresrc",
                vec![("stream-properties", "props,stream.capture.sink=true".into())]
            )
        );
        assert_eq!(
            Source::PulseDefaultMonitor.element(),
            ("pulsesrc", vec![("device", "@DEFAULT_MONITOR@".into())])
        );
        let (f, p) = Source::AppSinkMonitor(4242).element();
        assert_eq!(f, "pipewiresrc");
        assert_eq!(p[0], ("target-object", "4242".into()), "by object.serial");
        assert_eq!(
            caps(),
            "audio/x-raw,format=F32LE,rate=48000,channels=2,layout=interleaved"
        );
    }
}
