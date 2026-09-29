//! R56 (docs/58 D12, LX3's G10 row): the live GStreamer pipeline through the
//! engine to the REAL gawk-server — the D4 tail's Annex-B AUs, stamped on the
//! one clock, through the producer gate and the send pump; a rolling relay
//! restart; and the forced IDR that re-primes the reclaimed broadcast. The
//! encoder is `x264enc`, TEST-ONLY — the plumbing around it is what CI can
//! prove; the hardware candidates are the on-hardware pass's (LX7).
//!
//! Ignored by default like the engine's relay suite (it builds the relay
//! with Go): `cargo test -p gawk-encode --test gst_to_relay -- --ignored`.
#![cfg(target_os = "linux")]

#[path = "../../engine/tests/support/relay.rs"]
mod relay;

use gawk_encode::gst::{self, Live, LiveHooks};
use gawk_encode::gst_policy::{self, Candidate, Element, LivePlan};
use gawk_encode::h264;
use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::gate::FrameGate;
use gawk_engine::media::AccessUnit;
use gawk_engine::session::{EngineEvent, Session};
use relay::*;
use std::sync::{Arc, Mutex};
use std::time::Duration;

const FPS: u32 = 30;

fn plan() -> LivePlan {
    let mut video = vec![
        Element::new("videotestsrc")
            .prop("is-live", "true")
            .prop("pattern", "ball"),
        Element::caps(format!(
            "video/x-raw,format=I420,width=640,height=360,framerate={FPS}/1"
        )),
        Element::new("x264enc")
            .named(gst_policy::ENCODER)
            .prop("tune", "zerolatency")
            .prop("speed-preset", "ultrafast")
            .prop("bframes", 0)
            .prop("key-int-max", gst_policy::gop_frames(FPS))
            .prop("bitrate", 1500),
    ];
    video.extend(gst_policy::h264_tail());
    LivePlan {
        video,
        thumb: Vec::new(),
        tee: None,
    }
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "builds the real gawk-server"]
async fn gstreamer_streams_to_the_real_relay_and_reprimes_on_resume() {
    gst::init().unwrap();
    if !gst::missing(&[Element::new("x264enc")]).is_empty() {
        assert!(
            std::env::var("GAWK_REQUIRE_GST").is_err(),
            "GAWK_REQUIRE_GST is set but x264enc is missing"
        );
        eprintln!("SKIP: x264enc not installed");
        return;
    }
    let clock = Arc::new(MonotonicClock::new());
    let mut relay = Relay::start(&["-publish-secret", SECRET]);
    let (session, mut rx) = Session::start(config(&relay, "", ""), clock.clone())
        .await
        .expect("publish");
    let (id, _token) = collect_identity(&mut rx).await;
    let sender = session.sender();

    // The Linux shell's wiring, minus the portal: appsink → one-clock
    // stamp → gate → pump.
    let gate = Arc::new(Mutex::new(FrameGate::new()));
    let notify = Arc::new(tokio::sync::Notify::new());
    let codec: Arc<Mutex<Option<String>>> = Arc::default();
    let idrs: Arc<Mutex<Vec<u64>>> = Arc::default();
    let pump = {
        let (gate, notify, sender) = (gate.clone(), notify.clone(), sender.clone());
        tokio::spawn(async move {
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
    let mapper = gawk_capture_mapper(&*clock);
    let live = {
        let (gate, notify, codec, idrs, sender) = (
            gate.clone(),
            notify.clone(),
            codec.clone(),
            idrs.clone(),
            sender.clone(),
        );
        Live::start(
            &plan(),
            Candidate::Nvenc,
            FPS,
            LiveHooks {
                on_au: Box::new(move |au| {
                    let mut c = codec.lock().unwrap();
                    if c.is_none() {
                        let s = h264::parse_codec_string(&au.data).expect("first AU has an SPS");
                        sender.set_codec(&s);
                        *c = Some(s);
                    }
                    let ts = mapper(au.capture_ns);
                    if au.idr {
                        assert!(h264::has_sps_pps(&au.data), "SPS/PPS before every IDR");
                        idrs.lock().unwrap().push(ts);
                    }
                    gate.lock().unwrap().offer(AccessUnit {
                        data: au.data,
                        timestamp_us: ts,
                        keyframe: au.idr,
                    });
                    notify.notify_one();
                }),
                on_input: Box::new(|_| {}),
                on_thumb: Box::new(|_, _, _| {}),
                on_error: Box::new(|e| panic!("pipeline error: {}", e.text)),
            },
        )
        .expect("live")
    };

    tokio::time::sleep(Duration::from_millis(1500)).await;
    let before = session.stats();
    assert!(before.keyframe_streams_sent >= 2, "{before:?}");
    assert!(before.sent_frames >= 30, "{before:?}");
    assert!(before.codec.starts_with("avc1."), "{before:?}");
    // Timestamps are session-clock µs from capture, close to now.
    let now = clock.now_us();
    let last = *idrs.lock().unwrap().last().unwrap();
    assert!(
        now >= last && now - last < 2_000_000,
        "now {now}, last IDR {last}"
    );

    // A rolling restart: the session reclaims its code on its own…
    relay.drain_and_stop();
    let relay = relay.restart();
    loop {
        match next_event(&mut rx, "resumed", 30).await {
            EngineEvent::Resumed => break,
            EngineEvent::Ended { error } => panic!("ended instead of resuming: {error:?}"),
            _ => {}
        }
    }
    assert_eq!(session.broadcast_id(), id, "the same code, never a mint");

    // …and the re-prime (Media::force_idr on Resumed — new on Linux, G10)
    // puts an IDR out within a few frames instead of waiting out the GOP.
    let kf_before = session.stats().keyframe_streams_sent;
    let idrs_before = idrs.lock().unwrap().len();
    live.force_idr();
    tokio::time::sleep(Duration::from_millis(200)).await;
    assert!(
        idrs.lock().unwrap().len() > idrs_before,
        "a forced IDR within 200 ms"
    );
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        session.stats().keyframe_streams_sent > kf_before,
        "the re-prime reached the restarted relay"
    );

    live.stop();
    pump.abort();
    session.stop().await;
    drop(relay);
}

/// The Linux mapper (`gawk_capture::pwclock`) re-derived here, so this test
/// does not pull the capture crate in: CLOCK_MONOTONIC ns → session µs.
fn gawk_capture_mapper(clock: &dyn Clock) -> impl Fn(u64) -> u64 + Send + Sync + 'static {
    use gstreamer::prelude::*;
    let mono_ns = gst::monotonic_clock().time().nseconds();
    let session_us = clock.now_us();
    move |ns: u64| {
        let delta = ns as i64 - mono_ns as i64;
        (session_us as i64 + delta / 1000).max(0) as u64
    }
}
