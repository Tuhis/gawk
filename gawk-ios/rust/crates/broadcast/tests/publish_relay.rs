//! D24's publisher integration test (docs/67, IO2): the iOS broadcast
//! pipeline driven on the Mac host (real VideoToolbox, `encode/vt.rs`) with
//! synthetic capture-shaped `420v` frames, portrait then rotated, through a
//! rolling relay restart, on a current-thread runtime with `self-update`
//! off, as in the app. A `gawk-viewer` session watches what arrives.
//!
//! Ignored by default (it builds and runs the Go relay):
//! `cargo test -p gawk-broadcast --test publish_relay -- --ignored`.

#![cfg(target_os = "macos")]

#[path = "../../../../../gawk-broadcast-desktop/crates/engine/tests/support/relay.rs"]
mod relay;

use gawk_broadcast::pipeline::Pipeline;
use gawk_broadcast::rung::Quality;
use gawk_capture::host;
use gawk_encode::vt;
use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use gawk_viewer::decode::h264::H264Stream;
use gawk_viewer::pipeline::ViewerEvent;
use gawk_viewer::playout::PlayoutPreset;
use gawk_viewer::session::{
    ViewerClock, ViewerConfig, ViewerSink, ViewerState, WtSubscribeDialer, run,
};
use relay::{Relay, SECRET};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// What the watching viewer saw: frame IDs in order, and each H.264
/// format the stream announced in-band (an SPS change is a new size).
#[derive(Default)]
struct Seen {
    ids: Vec<u32>,
    formats: usize,
    stream: Option<H264Stream>,
    states: Vec<ViewerState>,
}

#[derive(Default)]
struct Sink(Mutex<Seen>);

impl ViewerSink for Sink {
    fn state(&self, s: ViewerState) {
        self.0.lock().unwrap().states.push(s);
    }
    fn event(&self, e: ViewerEvent) {
        let mut seen = self.0.lock().unwrap();
        match e {
            ViewerEvent::VideoConfig(c) => {
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

/// Pushes `n` synthetic frames of `w` × `h` at ~30 fps.
async fn push_frames(p: &Pipeline, w: u32, h: u32, n: u32) {
    for i in 0..n {
        let pb = vt::synthetic_frame(w, h, i as u8).expect("a synthetic 420v frame");
        p.push_video(&pb, Some(host::now_100ns()), 0);
        tokio::time::sleep(Duration::from_millis(33)).await;
    }
}

async fn wait_for(what: &str, within: Duration, mut cond: impl FnMut() -> bool) {
    let deadline = Instant::now() + within;
    while !cond() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

#[tokio::test(flavor = "current_thread")]
#[ignore = "builds and runs the Go relay"]
async fn the_ios_pipeline_rotates_and_resumes_on_the_same_code() {
    let relay = Relay::start(&["-publish-secret", SECRET]);
    let clock: Arc<dyn Clock> = Arc::new(MonotonicClock::new());
    let cfg = SessionConfig {
        relay_url: relay.url.clone(),
        publish_secret: SECRET.into(),
        origin: "gawk://ios".into(),
        insecure: true,
        ..SessionConfig::default()
    };
    let (session, mut events) = Session::start(cfg, clock.clone()).await.unwrap();
    let id = loop {
        if let Some(EngineEvent::Announce { broadcast_id }) = events.recv().await {
            break broadcast_id;
        }
    };
    let pipeline = Arc::new(Pipeline::new(
        session.sender(),
        tokio::runtime::Handle::current(),
        clock.clone(),
        Quality::Cellular,
    ));
    let resumed = Arc::new(Mutex::new(false));
    {
        let (pipeline, resumed) = (pipeline.clone(), resumed.clone());
        tokio::spawn(async move {
            while let Some(e) = events.recv().await {
                if e == EngineEvent::Resumed {
                    pipeline.force_idr();
                    *resumed.lock().unwrap() = true;
                }
            }
        });
    }

    // A viewer on the same broadcast, as a desktop Chrome one would be.
    let sink = Arc::new(Sink::default());
    let (_tx, rx) = tokio::sync::mpsc::unbounded_channel();
    tokio::spawn(run(
        ViewerConfig {
            relay_url: relay.url.clone(),
            broadcast_id: id.clone(),
            preset: PlayoutPreset::LowestLatency,
        },
        Arc::new(WtSubscribeDialer {
            origin: "gawk://ios".into(),
            insecure: true,
        }),
        sink.clone(),
        ViewerClock::new(),
        rx,
    ));

    // Portrait, then a rotation: a new size on the same publish session.
    push_frames(&pipeline, 360, 640, 45).await;
    assert_eq!(pipeline.current_size(), Some((360, 640)));
    push_frames(&pipeline, 640, 360, 45).await;
    assert_eq!(pipeline.current_size(), Some((640, 360)));
    wait_for("both sizes at the viewer", Duration::from_secs(10), || {
        sink.0.lock().unwrap().formats >= 2
    })
    .await;
    assert_eq!(session.broadcast_id(), id, "a rotation kept the code");
    let before = *sink.0.lock().unwrap().ids.last().unwrap();

    // A rolling restart: the relay drains (4002) and comes back on the same
    // port; the engine reclaims the code with its token.
    let mut relay = relay;
    relay.drain_and_stop();
    let _relay = relay.restart();
    let deadline = Instant::now() + Duration::from_secs(30);
    while !*resumed.lock().unwrap() {
        assert!(Instant::now() < deadline, "the engine never resumed");
        push_frames(&pipeline, 640, 360, 5).await;
    }
    assert_eq!(session.broadcast_id(), id, "resumed on the same code");
    // Capture keeps producing while the viewer climbs its reconnect ladder
    // (a 404 until the reclaim lands is part of it).
    let seen_before = sink.0.lock().unwrap().ids.len();
    let deadline = Instant::now() + Duration::from_secs(45);
    while sink.0.lock().unwrap().ids.len() < seen_before + 15 {
        assert!(
            Instant::now() < deadline,
            "no frames after the restart: viewer states {:?}",
            sink.0.lock().unwrap().states
        );
        push_frames(&pipeline, 640, 360, 10).await;
    }
    // Frame-ID continuity is the resume signal: the space carried on.
    let after = *sink.0.lock().unwrap().ids.last().unwrap();
    assert!(
        gawk_wire::frame_id_ahead(after, before),
        "frame IDs continued: {before} then {after}"
    );
    assert!(pipeline.take_failure().is_none());
    session.stop().await;
}
