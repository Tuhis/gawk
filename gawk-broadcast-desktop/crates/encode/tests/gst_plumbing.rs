//! The Linux encode plumbing CI can see (R56, docs/58 D12): the trial runner
//! and the live pipeline, driven by `videotestsrc` and a SOFTWARE encoder.
//! `x264enc` is test-only — never a production candidate, and never in the
//! cascade — standing in for the GPU a runner does not have. What this
//! proves is everything around the encoder: the planned tail, the forced-IDR
//! probe, the shared trial validator on real GStreamer output, one-clock
//! timestamps, error attribution and synchronous teardown.
//!
//! A machine without `x264enc` skips — unless `GAWK_REQUIRE_GST=1`, which CI
//! sets, so a missing element can never turn this into a silent pass
//! (docs/39 F11's lesson).
#![cfg(target_os = "linux")]

use gawk_encode::cascade::validate_trial;
use gawk_encode::gst::{self, Live, LiveHooks};
use gawk_encode::gst_policy::{self, Candidate, Culprit, Element, LivePlan};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

fn available() -> bool {
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

/// The stub encoder: the properties that make x264 behave like the hardware
/// candidates are asked to (no B-frames, no lookahead, GOP = fps/2).
fn x264(fps: u32) -> Element {
    Element::new("x264enc")
        .named(gst_policy::ENCODER)
        .prop("tune", "zerolatency")
        .prop("speed-preset", "ultrafast")
        .prop("bframes", 0)
        .prop("key-int-max", gst_policy::gop_frames(fps))
        .prop("bitrate", 2000)
}

fn source(fps: u32, w: u32, h: u32) -> Vec<Element> {
    vec![
        Element::new("videotestsrc")
            .prop("is-live", "true")
            .prop("pattern", "ball"),
        Element::caps(format!(
            "video/x-raw,format=I420,width={w},height={h},framerate={fps}/1"
        )),
    ]
}

// docs/65 D3: the preview runner, on the preview plan's gate and tail with
// a test source in place of the portal, delivers the thumbnail's RGBA.
#[test]
fn the_preview_delivers_an_rgba_thumbnail_of_the_planned_width() {
    if !available() {
        return;
    }
    let mut plan = source(30, 640, 360);
    plan.push(
        Element::new("videorate")
            .prop("drop-only", "true")
            .prop("max-rate", 1),
    );
    plan.extend(gst_policy::thumb_tail());
    let preview = gst::Preview::start(&plan).expect("preview starts");
    let deadline = Instant::now() + Duration::from_secs(5);
    let (w, h, rgba) = loop {
        if let Some(t) = preview.take() {
            break t;
        }
        assert!(Instant::now() < deadline, "no thumbnail within 5 s");
        std::thread::sleep(Duration::from_millis(50));
    };
    assert_eq!((w, h), (gst_policy::THUMB_WIDTH, 180));
    assert_eq!(rgba.len(), (w * h * 4) as usize);
    drop(preview);
}

#[test]
fn the_trial_runner_accepts_a_well_behaved_encoder_through_the_shared_validator() {
    if !available() {
        return;
    }
    let fps = 30;
    let mut specs = source(fps, gst_policy::TRIAL_WIDTH, gst_policy::TRIAL_HEIGHT);
    specs.push(x264(fps));
    specs.extend(gst_policy::h264_tail());
    let run = gst::run_trial(Candidate::Nvenc, fps, specs).expect("trial ran");
    let (frames, forced_at) = gawk_encode::vt_policy::trial_plan(fps);
    assert_eq!(run.inputs_fed, frames);
    assert_eq!(run.forced_idr_at, Some(forced_at));
    let v = validate_trial(&run).expect("x264 passes every invariant");
    assert!(v.codec_string.starts_with("avc1."), "{}", v.codec_string);
}

#[test]
fn the_trial_rejects_an_encoder_that_holds_frames_back() {
    if !available() {
        return;
    }
    // x264's default (non-zerolatency) lookahead retains frames: exactly the
    // encoder-internal latency the gate exists to refuse.
    let fps = 30;
    let mut specs = source(fps, gst_policy::TRIAL_WIDTH, gst_policy::TRIAL_HEIGHT);
    specs.push(
        Element::new("x264enc")
            .named(gst_policy::ENCODER)
            .prop("bframes", 0)
            .prop("rc-lookahead", 40)
            .prop("key-int-max", 15),
    );
    specs.extend(gst_policy::h264_tail());
    let run = gst::run_trial(Candidate::Nvenc, fps, specs).expect("trial ran");
    let err = validate_trial(&run).unwrap_err();
    // Either some frames came out and the rest were held, or the lookahead
    // swallowed the whole trial: both are the latency the gate refuses.
    assert!(
        err.contains("retains") || err.contains("no output"),
        "{err}"
    );
}

#[test]
fn a_missing_property_fails_the_candidate_instead_of_panicking() {
    if !available() {
        return;
    }
    let mut specs = source(30, 320, 240);
    specs.push(x264(30).prop("no-such-property", 1));
    specs.extend(gst_policy::h264_tail());
    let err = gst::run_trial(Candidate::Nvenc, 30, specs).unwrap_err();
    assert!(err.contains("no property"), "{err}");
}

/// CLOCK_MONOTONIC as the pipeline reads it: its clock is the monotonic
/// system clock — what `Instant` reads on Linux.
fn monotonic_ns() -> u64 {
    use gstreamer::prelude::*;
    gst::monotonic_clock().time().nseconds()
}

#[test]
fn the_live_pipeline_stamps_on_the_monotonic_clock_and_forces_idrs() {
    if !available() {
        return;
    }
    let fps = 30;
    let mut video = source(fps, 320, 240);
    video.push(x264(fps));
    video.extend(gst_policy::h264_tail());
    let plan = LivePlan {
        video,
        thumb: Vec::new(),
        tee: None,
    };
    let aus: Arc<Mutex<Vec<(bool, u64)>>> = Arc::default();
    let inputs = Arc::new(Mutex::new(0u64));
    let errors: Arc<Mutex<Vec<String>>> = Arc::default();
    let hooks = LiveHooks {
        on_au: {
            let aus = aus.clone();
            Box::new(move |au| aus.lock().unwrap().push((au.idr, au.capture_ns)))
        },
        on_input: {
            let inputs = inputs.clone();
            Box::new(move |_| *inputs.lock().unwrap() += 1)
        },
        on_thumb: Box::new(|_, _, _| {}),
        on_error: {
            let errors = errors.clone();
            Box::new(move |e| errors.lock().unwrap().push(e.text))
        },
    };
    let before = monotonic_ns();
    let live = Live::start(&plan, Candidate::Nvenc, fps, hooks).expect("started");
    wait_for(|| aus.lock().unwrap().len() >= 10);
    let after = monotonic_ns();
    {
        let got = aus.lock().unwrap();
        assert!(got[0].0, "the first AU is an IDR");
        for (_, ns) in got.iter() {
            assert!(
                *ns >= before && *ns <= after,
                "capture time {ns} outside [{before}, {after}] — not the monotonic clock"
            );
        }
        for w in got.windows(2) {
            assert!(w[1].1 > w[0].1, "strictly monotonic");
        }
    }

    // A forced IDR (the resume re-prime): one within 3 frames.
    let n = aus.lock().unwrap().len();
    live.force_idr();
    wait_for(|| aus.lock().unwrap().len() >= n + 4);
    let idr_soon = aus.lock().unwrap()[n..n + 4].iter().any(|(idr, _)| *idr);
    assert!(idr_soon, "forced IDR not produced within 3 frames");
    assert!(live.inputs() > 0 && *inputs.lock().unwrap() > 0);
    assert!(
        errors.lock().unwrap().is_empty(),
        "{:?}",
        errors.lock().unwrap()
    );

    // Teardown is synchronous: nothing arrives after stop returns.
    live.stop();
    let settled = aus.lock().unwrap().len();
    std::thread::sleep(Duration::from_millis(200));
    assert_eq!(aus.lock().unwrap().len(), settled);
}

#[test]
fn an_element_error_is_attributed_and_reported_from_the_bus() {
    if !available() {
        return;
    }
    // A source that errors once running: videotestsrc cannot produce this
    // format, so negotiation fails inside the capture half of the plan.
    let mut video = vec![
        Element::new("videotestsrc").prop("is-live", "true"),
        Element::caps("video/x-raw,format=I420,width=320,height=240,framerate=30/1"),
        Element::new("videorate"),
        Element::caps("video/x-raw,format=I420,width=320,height=240,framerate=0/0"),
    ];
    video.push(x264(30));
    video.extend(gst_policy::h264_tail());
    let plan = LivePlan {
        video,
        thumb: Vec::new(),
        tee: None,
    };
    let errors: Arc<Mutex<Vec<(Culprit, String)>>> = Arc::default();
    let hooks = LiveHooks {
        on_au: Box::new(|_| {}),
        on_input: Box::new(|_| {}),
        on_thumb: Box::new(|_, _, _| {}),
        on_error: {
            let errors = errors.clone();
            Box::new(move |e| errors.lock().unwrap().push((e.culprit, e.text)))
        },
    };
    match Live::start(&plan, Candidate::Nvenc, 30, hooks) {
        Err(text) => assert!(!text.is_empty()),
        Ok(live) => {
            wait_for(|| !errors.lock().unwrap().is_empty());
            let (culprit, text) = errors.lock().unwrap()[0].clone();
            assert!(text.contains(':'), "factory-prefixed: {text}");
            assert_ne!(culprit, Culprit::Encode, "{text}");
            live.stop();
        }
    }
}

fn wait_for(mut done: impl FnMut() -> bool) {
    let t = Instant::now();
    while !done() {
        assert!(t.elapsed() < Duration::from_secs(10), "timed out");
        std::thread::sleep(Duration::from_millis(10));
    }
}

/// Review finding (PR #398): an element that fails DURING the state change
/// posts its error synchronously; the bus thread must not swallow it before
/// the start failure is reported, or the caller gets a generic sentence and
/// the all-inside-pipewiresrc diagnosis breaks.
#[test]
fn a_start_failure_names_the_element_that_failed() {
    if !available() {
        return;
    }
    let plan = LivePlan {
        video: vec![
            Element::new("filesrc").prop("location", "/nonexistent/gawk-test"),
            Element::new("identity").named(gst_policy::ENCODER),
            Element::new("appsink").named(gst_policy::VIDEO_SINK),
        ],
        thumb: Vec::new(),
        tee: None,
    };
    // Several times: the race was the bus thread winning, not always.
    for _ in 0..5 {
        let hooks = LiveHooks {
            on_au: Box::new(|_| {}),
            on_input: Box::new(|_| {}),
            on_thumb: Box::new(|_, _, _| {}),
            on_error: Box::new(|_| {}),
        };
        match Live::start(&plan, Candidate::Nvenc, 30, hooks) {
            Err(text) => assert!(text.starts_with("filesrc:"), "{text}"),
            Ok(_) => panic!("a missing file cannot start"),
        }
    }
}
