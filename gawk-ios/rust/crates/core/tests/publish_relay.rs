//! docs/70 IX4's integration test (K3, D12): a quality change while live,
//! through the core's own `Broadcaster` on the Mac host (real VideoToolbox,
//! Swift's calls made from a plain thread) against the Go relay. The code
//! and resume token stay the same, the status never leaves Live, and a
//! watching viewer re-primes on the new rung's config.
//!
//! Ignored by default (it builds and runs the Go relay):
//! `cargo test --workspace --test publish_relay -- --ignored`, the same
//! command that runs `gawk-broadcast`'s test of this name.

#![cfg(target_os = "macos")]

#[path = "../../../../../gawk-broadcast-desktop/crates/engine/tests/support/relay.rs"]
mod relay;

use gawk_core::broadcast::{
    BroadcastListener, BroadcastOptions, BroadcastRoomEvent, BroadcastStatus, Broadcaster, Quality,
    host_time_100ns,
};
use gawk_core::rooms::RoomView;
use gawk_encode::vt;
use gawk_viewer::decode::h264::H264Stream;
use gawk_viewer::pipeline::ViewerEvent;
use gawk_viewer::playout::PlayoutPreset;
use gawk_viewer::session::{ViewerClock, ViewerConfig, ViewerSink, WtSubscribeDialer, run};
use objc2_core_video::CVPixelBuffer;
use relay::{Relay, SECRET};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// What the Swift side would have heard.
#[derive(Default)]
struct Heard {
    statuses: Mutex<Vec<BroadcastStatus>>,
    identities: Mutex<Vec<(String, String)>>,
}

impl BroadcastListener for Heard {
    fn on_status(&self, status: BroadcastStatus) {
        self.statuses.lock().unwrap().push(status);
    }
    fn on_identity(&self, code: String, token: String) {
        self.identities.lock().unwrap().push((code, token));
    }
    fn on_viewer_count(&self, _: u32) {}
    fn on_room_state(&self, _: RoomView, _: bool, _: bool) {}
    fn on_room_event(&self, _: BroadcastRoomEvent) {}
    fn on_failure(&self, text: String) {
        panic!("the pipeline failed: {text}");
    }
}

/// What the watching viewer saw: frame IDs in order, and each H.264 format
/// the stream announced (a new config re-primes the decoder).
#[derive(Default)]
struct Seen {
    ids: Vec<u32>,
    configs: Vec<String>,
    formats: usize,
    stream: Option<H264Stream>,
}

#[derive(Default)]
struct Sink(Mutex<Seen>);

impl ViewerSink for Sink {
    fn state(&self, _: gawk_viewer::session::ViewerState) {}
    fn event(&self, e: ViewerEvent) {
        let mut seen = self.0.lock().unwrap();
        match e {
            ViewerEvent::VideoConfig(c) => {
                seen.configs.push(c.codec.clone());
                seen.stream = H264Stream::new(&c.codec, &c.extradata).ok();
            }
            ViewerEvent::VideoFrame { frame_id, data, .. } => {
                seen.ids.push(frame_id);
                if let Some(s) = seen.stream.as_mut()
                    && s.sample(&data).is_ok_and(|s| s.format_changed)
                {
                    seen.formats += 1;
                }
            }
            _ => {}
        }
    }
}

/// Whether this Mac has the hardware encoder `vt.rs` requires (see
/// `gawk-broadcast`'s `publish_relay.rs`).
fn hardware_encoder() -> bool {
    let mut runner = vt::VtTrialRunner {
        params: vt::EncoderParams {
            width: 1280,
            height: 720,
            fps: 30,
            peak_bitrate_bps: 3_000_000,
        },
    };
    let ok = gawk_encode::cascade::choose(&vt::candidates(), None, &mut runner).is_ok();
    if !ok {
        assert!(
            std::env::var("GAWK_REQUIRE_HW_ENCODER").as_deref() != Ok("1"),
            "no hardware H.264 encoder on this Mac"
        );
        eprintln!("SKIPPED: no hardware H.264 encoder on this Mac (a VM?)");
    }
    ok
}

/// Pushes `n` frames of a 2560 × 1440 capture at ~30 fps, each made at the
/// size the plan asks for, as the Swift capture code converts them.
fn push_frames(b: &Broadcaster, n: u32) {
    for i in 0..n {
        let pts = host_time_100ns();
        let plan = b.plan(2560, 1440, None, pts);
        let pb = vt::synthetic_frame(plan.width, plan.height, i as u8).expect("a 420v frame");
        b.push_video(&*pb as *const CVPixelBuffer as u64, pts, 0);
        std::thread::sleep(Duration::from_millis(33));
    }
}

/// Pushes frames until `cond` holds; how many it pushed.
fn push_until(b: &Broadcaster, what: &str, within: Duration, cond: impl Fn() -> bool) -> u64 {
    let deadline = Instant::now() + within;
    let mut pushed = 0;
    while !cond() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        push_frames(b, 5);
        pushed += 5;
    }
    pushed
}

#[test]
#[ignore = "builds and runs the Go relay"]
fn a_quality_change_keeps_the_code_and_the_viewer_re_primes() {
    if !hardware_encoder() {
        return;
    }
    gawk_core::initialize_core();
    let relay = Relay::start(&["-publish-secret", SECRET]);
    let heard = Arc::new(Heard::default());
    let b = Broadcaster::start(
        BroadcastOptions {
            relay_url: relay.url.clone(),
            publish_secret: SECRET.into(),
            broadcast_id: String::new(),
            resume_token_hex: String::new(),
            quality: Quality::Standard,
            room_code: String::new(),
            room_attach_secret: String::new(),
            nickname: String::new(),
            insecure: true,
            telemetry: false,
            capture_source: "test-source".into(),
            room_new: false,
            room_creator_token_hex: String::new(),
        },
        heard.clone(),
    );
    let deadline = Instant::now() + Duration::from_secs(15);
    let (code, token) = loop {
        if let Some(id) = heard.identities.lock().unwrap().first() {
            break id.clone();
        }
        assert!(Instant::now() < deadline, "no identity");
        std::thread::sleep(Duration::from_millis(50));
    };

    // A viewer on the broadcast, on a runtime of its own.
    let sink = Arc::new(Sink::default());
    {
        let (sink, url, code) = (sink.clone(), relay.url.clone(), code.clone());
        std::thread::spawn(move || {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            let (_tx, rx) = tokio::sync::mpsc::unbounded_channel();
            rt.block_on(run(
                ViewerConfig {
                    relay_url: url,
                    broadcast_id: code,
                    preset: PlayoutPreset::LowestLatency,
                },
                Arc::new(WtSubscribeDialer {
                    origin: "gawk://ios".into(),
                    insecure: true,
                }),
                sink,
                ViewerClock::new(),
                rx,
            ));
        });
    }

    // Standard: 1920 on the long edge, 60 fps, 8 Mbps peak.
    let mut pushed = push_until(&b, "frames at the viewer", Duration::from_secs(20), || {
        sink.0.lock().unwrap().ids.len() >= 15
    });
    let c = b.counters();
    assert_eq!((c.width, c.height, c.fps), (1920, 1080, 60));
    assert_eq!(c.peak_bitrate_bps, 8_000_000);
    let (configs_before, formats_before, last_before) = {
        let s = sink.0.lock().unwrap();
        (s.configs.len(), s.formats, *s.ids.last().unwrap())
    };
    let statuses_before = heard.statuses.lock().unwrap().len();

    // Cellular while live: 1280 on the long edge, 30 fps, 3 Mbps peak.
    b.set_quality(Quality::Cellular);
    pushed += push_until(
        &b,
        "the viewer re-primed on the new rung",
        Duration::from_secs(30),
        || {
            let s = sink.0.lock().unwrap();
            s.configs.len() > configs_before && s.formats > formats_before && {
                let n = s.ids.len();
                n >= 15
                    && s.ids[n - 15..]
                        .iter()
                        .all(|&id| gawk_wire::frame_id_ahead(id, last_before))
            }
        },
    );
    let c = b.counters();
    assert_eq!((c.width, c.height, c.fps), (1280, 720, 30));
    assert_eq!(c.peak_bitrate_bps, 3_000_000);
    // Every push counts, across both pipelines; one racing the swap may
    // land in the retired one after it closed.
    assert!(
        (pushed - 1..=pushed).contains(&c.pushed),
        "{} of {pushed} pushes counted",
        c.pushed
    );

    let after: Vec<_> = heard.statuses.lock().unwrap()[statuses_before..].to_vec();
    assert!(
        after
            .iter()
            .all(|s| matches!(s, BroadcastStatus::Live { code: c, .. } if *c == code)),
        "the badge stays LIVE on the same code: {after:?}"
    );
    assert!(
        heard
            .identities
            .lock()
            .unwrap()
            .iter()
            .all(|id| *id == (code.clone(), token.clone())),
        "the code and the resume token are unchanged"
    );
    let s = sink.0.lock().unwrap();
    eprintln!(
        "viewer configs {:?}, {} frames, {} formats",
        s.configs,
        s.ids.len(),
        s.formats
    );
    drop(s);

    b.stop();
    let deadline = Instant::now() + Duration::from_secs(10);
    while !heard
        .statuses
        .lock()
        .unwrap()
        .iter()
        .any(|s| matches!(s, BroadcastStatus::Ended { .. }))
    {
        assert!(Instant::now() < deadline, "Stop never ended the broadcast");
        std::thread::sleep(Duration::from_millis(50));
    }
}
