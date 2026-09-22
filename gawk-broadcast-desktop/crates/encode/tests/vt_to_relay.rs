//! R52 MB3 (docs/54 §8): real VideoToolbox output through the engine to the
//! REAL gawk-server — the trial-accepted session's Annex-B AUs as the relay
//! takes them, a rolling relay restart, and the forced IDR that re-primes
//! the reclaimed broadcast (docs/38 D5). What the capture adds on top is
//! MB2's, and the manual pass's.
//!
//! macOS-only and ignored by default: it needs a hardware encoder and the
//! Go toolchain. `cargo test -p gawk-encode --test vt_to_relay -- --ignored`.
#![cfg(target_os = "macos")]

#[path = "../../engine/tests/support/relay.rs"]
mod relay;

use gawk_encode::cascade;
use gawk_encode::h264;
use gawk_encode::vt::{self, EncodedAu, Encoder, EncoderParams, VtTrialRunner};
use gawk_engine::clock::MonotonicClock;
use gawk_engine::media::AccessUnit;
use gawk_engine::session::{EngineEvent, Session};
use relay::*;
use std::sync::Arc;
use std::sync::mpsc;
use std::time::Duration;

const FPS: u32 = 30;
const FRAME_100NS: i64 = 10_000_000 / FPS as i64;

fn au(e: &EncodedAu) -> AccessUnit {
    AccessUnit {
        data: e.data.clone(),
        timestamp_us: (e.time_100ns / 10) as u64,
        keyframe: e.keyframe,
    }
}

/// Encodes `n` synthetic frames starting at frame index `from` and returns
/// what VideoToolbox emitted for them.
fn encode(enc: &Encoder, out: &mpsc::Receiver<EncodedAu>, from: usize, n: usize) -> Vec<EncodedAu> {
    let params = (1280, 720);
    for i in from..from + n {
        let pb = vt::synthetic_frame(params.0, params.1, i as u8).expect("frame");
        enc.encode(&pb, i as i64 * FRAME_100NS, FRAME_100NS)
            .expect("encode");
    }
    (0..n)
        .map(|_| {
            out.recv_timeout(Duration::from_secs(5))
                .expect("an AU per frame (one-in-one-out)")
        })
        .collect()
}

#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs a hardware H.264 encoder and builds the real gawk-server"]
async fn videotoolbox_streams_to_the_real_relay_and_reprimes_on_resume() {
    let params = EncoderParams {
        width: 1280,
        height: 720,
        fps: FPS,
        peak_bitrate_bps: 4_000_000,
    };
    let accepted = cascade::choose(&vt::candidates(), None, &mut VtTrialRunner { params })
        .unwrap_or_else(|r| panic!("refused: {:?}", r.tried));

    let mut relay = Relay::start(&["-publish-secret", SECRET]);
    let (session, mut rx) = Session::start(config(&relay, "", ""), Arc::new(MonotonicClock::new()))
        .await
        .expect("publish");
    let (id, _token) = collect_identity(&mut rx).await;
    let sender = session.sender();
    sender.set_codec(&accepted.codec_string);

    let (tx, out) = mpsc::channel();
    let enc = Encoder::new(
        params,
        move |au| {
            let _ = tx.send(au);
        },
        |e| eprintln!("encoder error: {e}"),
    )
    .expect("live session");

    // A GOP and a half: the cadence IDR at frame 15 rides a keyframe stream
    // too, with its parameter sets in-band (D8).
    let aus = encode(&enc, &out, 0, 20);
    assert!(aus[0].keyframe && aus[15].keyframe, "cadence IDRs");
    for a in aus.iter().filter(|a| a.keyframe) {
        assert_eq!(
            h264::parse_codec_string(&a.data).as_deref(),
            Some(accepted.codec_string.as_str()),
            "every IDR carries the SPS the codec string came from"
        );
    }
    for a in &aus {
        sender.send_video(au(a)).await;
    }
    tokio::time::sleep(Duration::from_millis(300)).await;
    let before = session.stats();
    assert!(before.keyframe_streams_sent >= 2, "{before:?}");
    assert!(before.sent_frames >= 18, "{before:?}");

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

    // …and the shell's re-prime (Media::force_idr on Resumed) makes the very
    // next frame an IDR instead of waiting out the GOP.
    enc.force_idr();
    let next = encode(&enc, &out, 20, 1);
    assert!(next[0].keyframe, "the frame after a forced IDR is an IDR");
    assert!(h264::has_sps_pps(&next[0].data));
    sender.send_video(au(&next[0])).await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    let after = session.stats();
    assert!(
        after.keyframe_streams_sent > before.keyframe_streams_sent,
        "the re-prime reached the restarted relay: {after:?}"
    );

    enc.finish();
    session.stop().await;
    drop(relay);
}
