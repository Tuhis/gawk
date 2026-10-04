//! Publishes one of the viewer's recorded fixtures (docs/67 IO5), looping,
//! through the shared engine: what G7 needs in the Simulator, where the
//! only other local source, `gawk-pubsim`, sends H.264 alone.
//!
//!   cargo run -p gawk-devpub -- --codec vp9 --url https://127.0.0.1:4433 --insecure
//!
//! Prints `GAWK_DEVPUB_ID=<ID>` once the relay announces the broadcast.
//! The fixtures are `crates/viewer/tests/fixtures/` (their README says how
//! they were made): 320×240, 30 frames, keyframes at 0 and 15.

use gawk_engine::clock::MonotonicClock;
use gawk_engine::media::AccessUnit;
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

const FPS: u64 = 30;

fn fixtures() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../viewer/tests/fixtures")
}

/// IVF frames (VP8/VP9): 32-byte file header, then 12-byte frame headers.
fn ivf_frames(b: &[u8]) -> Vec<Vec<u8>> {
    assert_eq!(&b[..4], b"DKIF", "not an IVF file");
    let mut off = usize::from(u16::from_le_bytes([b[6], b[7]]));
    let mut out = Vec::new();
    while off + 12 <= b.len() {
        let size = u32::from_le_bytes(b[off..off + 4].try_into().unwrap()) as usize;
        off += 12;
        out.push(b[off..off + size].to_vec());
        off += size;
    }
    out
}

/// An Annex-B stream's access units, split at each access unit delimiter.
fn h264_access_units(b: &[u8]) -> Vec<Vec<u8>> {
    let mut units: Vec<Vec<&[u8]>> = Vec::new();
    for nal in gawk_encode::h264::annex_b_nals(b) {
        if nal.first().is_some_and(|h| h & 0x1f == 9) || units.is_empty() {
            units.push(Vec::new());
        }
        units.last_mut().unwrap().push(nal);
    }
    units
        .into_iter()
        .map(gawk_encode::h264::annex_b_from_nals)
        .collect()
}

/// A VP8 keyframe has bit 0 of its first byte clear; a VP9 one has
/// `frame_type` 0 in its uncompressed header (profile 0, no show-existing).
fn vpx_keyframe(codec: &str, frame: &[u8]) -> bool {
    match codec {
        "vp8" => frame.first().is_some_and(|b| b & 0x01 == 0),
        _ => frame.first().is_some_and(|b| (b >> 2) & 0x01 == 0),
    }
}

#[tokio::main(flavor = "current_thread")]
async fn main() {
    let mut args = std::env::args().skip(1);
    let (mut codec, mut url, mut secret, mut insecure) = (
        "h264".to_owned(),
        "https://127.0.0.1:4433".to_owned(),
        String::new(),
        false,
    );
    while let Some(a) = args.next() {
        match a.as_str() {
            "--codec" => codec = args.next().expect("--codec vp8|vp9|h264"),
            "--url" => url = args.next().expect("--url <relay>"),
            "--secret" => secret = args.next().expect("--secret <publish secret>"),
            "--insecure" => insecure = true,
            other => panic!("unknown argument {other}"),
        }
    }
    let (codec_string, units, keyframes): (String, Vec<Vec<u8>>, Vec<bool>) = match codec.as_str() {
        "h264" => {
            let units =
                h264_access_units(&std::fs::read(fixtures().join("h264-320x240.h264")).unwrap());
            let codec = gawk_encode::h264::parse_codec_string(&units[0]).expect("an SPS up front");
            let keys = units
                .iter()
                .map(|u| gawk_encode::h264::has_idr(u))
                .collect();
            (codec, units, keys)
        }
        "vp8" | "vp9" => {
            let file = format!("{codec}-320x240.ivf");
            let units = ivf_frames(&std::fs::read(fixtures().join(file)).unwrap());
            let name = if codec == "vp8" {
                "vp8"
            } else {
                "vp09.00.10.08"
            };
            let keys = units.iter().map(|u| vpx_keyframe(&codec, u)).collect();
            (name.to_owned(), units, keys)
        }
        other => panic!("unknown codec {other}: vp8, vp9 or h264"),
    };
    let cfg = SessionConfig {
        relay_url: url.clone(),
        publish_secret: secret,
        origin: gawk_engine::defaults::origin().to_owned(),
        insecure,
        ..SessionConfig::default()
    };
    let (session, mut events) = Session::start(cfg, Arc::new(MonotonicClock::new()))
        .await
        .unwrap_or_else(|e| panic!("could not publish to {url}: {e}"));
    tokio::spawn(async move {
        while let Some(e) = events.recv().await {
            if let EngineEvent::Announce { broadcast_id } = e {
                println!("GAWK_DEVPUB_ID={broadcast_id}");
            }
        }
    });
    eprintln!(
        "looping the {codec} fixture ({codec_string}, {} frames) at {FPS} fps",
        units.len()
    );
    let sender = session.sender();
    sender.set_codec(&codec_string);
    let mut tick = tokio::time::interval(Duration::from_micros(1_000_000 / FPS));
    let start = tokio::time::Instant::now();
    for i in (0..units.len()).cycle() {
        tick.tick().await;
        sender
            .send_video(AccessUnit {
                data: units[i].clone(),
                timestamp_us: start.elapsed().as_micros() as u64,
                keyframe: keyframes[i],
            })
            .await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splits_every_fixture_into_30_frames_with_keyframes_at_0_and_15() {
        let h264 = h264_access_units(&std::fs::read(fixtures().join("h264-320x240.h264")).unwrap());
        assert_eq!(h264.len(), 30);
        let keys: Vec<usize> = (0..30)
            .filter(|&i| gawk_encode::h264::has_idr(&h264[i]))
            .collect();
        assert_eq!(keys, [0, 15]);
        for codec in ["vp8", "vp9"] {
            let frames = ivf_frames(
                &std::fs::read(fixtures().join(format!("{codec}-320x240.ivf"))).unwrap(),
            );
            assert_eq!(frames.len(), 30, "{codec}");
            let keys: Vec<usize> = (0..30)
                .filter(|&i| vpx_keyframe(codec, &frames[i]))
                .collect();
            assert_eq!(keys, [0, 15], "{codec}");
        }
    }
}
