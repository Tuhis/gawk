use super::*;
use gawk_wire::{
    AudioFrameHeader, ChunkBudget, StreamFrameHeader, VideoChunkHeader, append_audio_frame,
    append_clock_mapping, append_decoder_config, append_session_closing,
    append_stream_frame_header, append_time_sync, append_video_chunk, append_viewer_count,
    split_frame,
};

fn keyframe_stream(frame_id: u32, ts_us: u64, codec: &str, payload: &[u8]) -> Vec<u8> {
    let mut config = Vec::new();
    append_decoder_config(&mut config, codec, &[1, 2, 3]).unwrap();
    let mut msg = Vec::new();
    let h = StreamFrameHeader {
        keyframe: true,
        frame_id,
        timestamp_us: ts_us,
        config_len: config.len() as u32,
        payload_len: payload.len() as u32,
    };
    append_stream_frame_header(&mut msg, &h).unwrap();
    msg.extend_from_slice(&config);
    msg.extend_from_slice(payload);
    msg
}

fn delta_datagrams(frame_id: u32, ts_us: u64, data: &[u8]) -> Vec<Vec<u8>> {
    let chunks = split_frame(data, &ChunkBudget::new()).unwrap();
    let count = chunks.len() as u16;
    chunks
        .iter()
        .enumerate()
        .map(|(i, c)| {
            let mut d = Vec::new();
            let h = VideoChunkHeader {
                keyframe: false,
                frame_id,
                chunk_index: i as u16,
                chunk_count: count,
                timestamp_us: ts_us,
            };
            append_video_chunk(&mut d, &h, c).unwrap();
            d
        })
        .collect()
}

fn audio(seq: u32, ts_us: u64) -> Vec<u8> {
    let mut d = Vec::new();
    let h = AudioFrameHeader {
        seq,
        timestamp_us: ts_us,
    };
    append_audio_frame(&mut d, &h, &[0xfc, 0xff, 0xfe]).unwrap();
    d
}

fn mapping(offset: i64) -> Vec<u8> {
    let mut d = Vec::new();
    append_clock_mapping(&mut d, offset);
    d
}

fn frames(out: &[ViewerEvent]) -> Vec<(u32, bool, f64)> {
    out.iter()
        .filter_map(|e| match e {
            ViewerEvent::VideoFrame {
                frame_id,
                keyframe,
                present_at_ms,
                ..
            } => Some((*frame_id, *keyframe, *present_at_ms)),
            _ => None,
        })
        .collect()
}

fn ids(out: &[ViewerEvent]) -> Vec<u32> {
    frames(out).iter().map(|f| f.0).collect()
}

fn count(out: &[ViewerEvent], f: impl Fn(&ViewerEvent) -> bool) -> usize {
    out.iter().filter(|e| f(e)).count()
}

/// A Balanced pipeline that has played keyframe 0 (ts 0, arriving at 1000)
/// once its offset came due.
fn playing() -> (Pipeline, Vec<ViewerEvent>) {
    let mut p = Pipeline::new(PlayoutPreset::Balanced);
    let mut out = Vec::new();
    p.on_stream(
        &keyframe_stream(0, 0, "avc1.42E01F", &[9; 100]),
        1000.0,
        &mut out,
    );
    p.tick(2000.0, &mut out);
    (p, out)
}

#[test]
fn a_keyframe_stream_then_deltas_play_in_order_with_one_config() {
    let (mut p, mut out) = playing();
    for id in 1..=3u32 {
        for d in delta_datagrams(id, id as u64 * 16_000, &[id as u8; 3000]) {
            p.on_datagram(&d, 2000.0, &mut out);
        }
    }
    p.tick(2000.0, &mut out);
    assert_eq!(ids(&out), [0, 1, 2, 3]);
    assert!(frames(&out)[0].1, "the first frame is the keyframe");
    let configs = count(&out, |e| matches!(e, ViewerEvent::VideoConfig(_)));
    assert_eq!(configs, 1);
    assert!(matches!(&out[0], ViewerEvent::VideoConfig(c) if c.codec == "avc1.42E01F"));
}

#[test]
fn presentation_times_are_timestamp_plus_baseline_plus_offset() {
    let (mut p, mut out) = playing();
    // Keyframe ts 0 arrived at 1000: baseline 1000, seed offset 150.
    assert_eq!(frames(&out), [(0, true, 1150.0)]);
    for d in delta_datagrams(1, 16_000, &[1; 10]) {
        p.on_datagram(&d, 1016.0, &mut out);
    }
    p.tick(1200.0, &mut out);
    assert_eq!(frames(&out)[1], (1, false, 16.0 + 1000.0 + 150.0));
}

#[test]
fn the_same_config_on_every_keyframe_reconfigures_only_on_a_change() {
    let mut p = Pipeline::new(PlayoutPreset::LowestLatency);
    let mut out = Vec::new();
    p.on_stream(&keyframe_stream(0, 0, "vp8", &[1]), 0.0, &mut out);
    p.on_stream(&keyframe_stream(15, 500_000, "vp8", &[1]), 500.0, &mut out);
    p.on_stream(
        &keyframe_stream(30, 1_000_000, "vp09.00.10.08", &[1]),
        1000.0,
        &mut out,
    );
    let codecs: Vec<String> = out
        .iter()
        .filter_map(|e| match e {
            ViewerEvent::VideoConfig(c) => Some(c.codec.clone()),
            _ => None,
        })
        .collect();
    assert_eq!(codecs, ["vp8", "vp09.00.10.08"]);
    // Each config precedes its keyframe.
    let first_vp9 = out
        .iter()
        .position(|e| matches!(e, ViewerEvent::VideoConfig(c) if c.codec.starts_with("vp09")))
        .unwrap();
    assert!(matches!(
        &out[first_vp9 + 1],
        ViewerEvent::VideoFrame { frame_id: 30, .. }
    ));
}

#[test]
fn lowest_latency_presents_video_on_release_and_audio_at_the_minimum_offset() {
    let mut p = Pipeline::new(PlayoutPreset::LowestLatency);
    let mut out = Vec::new();
    p.on_stream(&keyframe_stream(0, 0, "vp8", &[1]), 1000.0, &mut out);
    assert_eq!(frames(&out), [(0, true, 1000.0)]);
    p.on_datagram(&audio(0, 20_000), 1010.0, &mut out);
    let at = out.iter().find_map(|e| match e {
        ViewerEvent::Audio { present_at_ms, .. } => Some(*present_at_ms),
        _ => None,
    });
    assert_eq!(at, Some(20.0 + 1000.0 + MIN_PLAYOUT_OFFSET_MS));
}

#[test]
fn audio_is_scheduled_on_the_video_anchor() {
    let (mut p, mut out) = playing();
    p.on_datagram(&audio(7, 40_000), 1045.0, &mut out);
    let at = out.iter().find_map(|e| match e {
        ViewerEvent::Audio {
            packet,
            present_at_ms,
        } if packet.seq == 7 => Some(*present_at_ms),
        _ => None,
    });
    assert_eq!(at, Some(40.0 + 1000.0 + 150.0));
}

#[test]
fn a_broadcaster_restart_flushes_and_re_anchors() {
    let (mut p, mut out) = playing();
    // The new session's timeline: ts 0 again, much later.
    p.on_stream(
        &keyframe_stream(0xffff_0000, 5_000_000, "avc1.42E01F", &[2]),
        3000.0,
        &mut out,
    );
    p.on_stream(
        &keyframe_stream(3, 0, "avc1.42E01F", &[3]),
        9000.0,
        &mut out,
    );
    p.tick(9500.0, &mut out);
    assert_eq!(count(&out, |e| *e == ViewerEvent::Flush), 1);
    assert_eq!(p.stats(9500.0).restarts, 1);
    // Re-anchored on the new timeline: ts 0 arriving at 9000.
    assert_eq!(*frames(&out).last().unwrap(), (3, true, 9000.0 + 150.0));
}

#[test]
fn drops_to_live_when_the_screen_falls_two_offsets_behind() {
    let (mut p, mut out) = playing();
    p.note_presented(0);
    // Frames keep arriving, but the renderer reports nothing newer: at a
    // 150 ms offset, 300 ms of backlog is the limit.
    for id in 1..=10u32 {
        for d in delta_datagrams(id, id as u64 * 33_000, &[1; 10]) {
            p.on_datagram(&d, 2000.0 + id as f64, &mut out);
        }
    }
    let before = count(&out, |e| *e == ViewerEvent::Flush);
    p.tick(2100.0, &mut out);
    assert_eq!(count(&out, |e| *e == ViewerEvent::Flush), before + 1);
    assert_eq!(p.stats(2100.0).drops_to_live, 1);
    // Resynced: deltas wait for the next keyframe.
    for d in delta_datagrams(11, 363_000, &[1; 10]) {
        p.on_datagram(&d, 2200.0, &mut out);
    }
    p.tick(3000.0, &mut out);
    assert!(!ids(&out).contains(&11));
    // A drop to live never repeats while frozen for the keyframe.
    assert_eq!(p.stats(3000.0).drops_to_live, 1);
}

#[test]
fn no_drop_to_live_while_the_renderer_keeps_up() {
    let (mut p, mut out) = playing();
    for id in 1..=10u32 {
        for d in delta_datagrams(id, id as u64 * 16_000, &[1; 10]) {
            p.on_datagram(&d, 2000.0, &mut out);
        }
        p.note_presented(id as u64 * 16_000);
        p.tick(2000.0, &mut out);
    }
    assert_eq!(p.stats(2000.0).drops_to_live, 0);
    assert_eq!(count(&out, |e| *e == ViewerEvent::Flush), 0);
}

#[test]
fn the_session_watchdog_fires_after_15s_of_silence_only() {
    let mut p = Pipeline::new(PlayoutPreset::Balanced);
    assert_eq!(
        p.stall(100_000.0),
        None,
        "never armed before the first byte"
    );
    let mut out = Vec::new();
    let mut count_dgram = Vec::new();
    append_viewer_count(&mut count_dgram, 1);
    p.on_datagram(&count_dgram, 1000.0, &mut out);
    assert_eq!(p.stall(1000.0 + SESSION_STALL_MS - 1.0), None);
    assert_eq!(p.stall(1000.0 + SESSION_STALL_MS), Some(Stall::Session));
}

#[test]
fn the_keyframe_watchdog_fires_only_while_deltas_still_flow() {
    let (mut p, mut out) = playing(); // keyframe at 1000
    let t = 1000.0 + KEYFRAME_STALL_MS;
    for d in delta_datagrams(1, 16_000, &[1; 10]) {
        p.on_datagram(&d, t - 10.0, &mut out);
    }
    assert_eq!(p.stall(t), Some(Stall::Keyframe));
    // A broadcaster who stepped away sends nothing: not a stall.
    assert_eq!(p.stall(t + FRAMES_FLOWING_WINDOW_MS + 1.0), None);
}

#[test]
fn the_media_watchdog_needs_audio_and_two_mappings_since_the_last_media() {
    let mut p = Pipeline::new(PlayoutPreset::Balanced);
    let mut out = Vec::new();
    p.on_datagram(&audio(0, 0), 1000.0, &mut out);
    p.on_datagram(&mapping(5), 3000.0, &mut out);
    assert_eq!(
        p.stall(1000.0 + MEDIA_STALL_MS),
        None,
        "one mapping proves nothing"
    );
    p.on_datagram(&mapping(5), 6500.0, &mut out);
    assert_eq!(p.stall(1000.0 + MEDIA_STALL_MS), Some(Stall::Media));
    // Without audio ever seen it can't tell a wedge from a static screen.
    let mut silent = Pipeline::new(PlayoutPreset::Balanced);
    silent.on_stream(&keyframe_stream(0, 0, "vp8", &[1]), 1000.0, &mut out);
    silent.on_datagram(&mapping(5), 3000.0, &mut out);
    silent.on_datagram(&mapping(5), 6500.0, &mut out);
    assert_eq!(silent.stall(7100.0), None);
}

#[test]
fn records_session_closing_and_drops_malformed_streams() {
    let mut p = Pipeline::new(PlayoutPreset::Balanced);
    let mut out = Vec::new();
    p.on_stream(&[0x01], 0.0, &mut out);
    p.on_stream(&[0x01, 0x04, 0, 0, 1], 0.0, &mut out); // truncated keyframe
    let mut kf = keyframe_stream(0, 0, "vp8", &[1, 2, 3]);
    kf.pop(); // shorter than its header says
    p.on_stream(&kf, 0.0, &mut out);
    assert!(out.is_empty());
    let mut closing = Vec::new();
    append_session_closing(&mut closing, gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR).unwrap();
    p.on_stream(&closing, 0.0, &mut out);
    assert_eq!(
        p.session_closing(),
        Some(gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR)
    );
}

#[test]
fn capture_latency_combines_time_sync_and_the_clock_mapping() {
    let mut p = Pipeline::new(PlayoutPreset::Balanced);
    let mut out = Vec::new();
    assert_eq!(p.capture_latency_ms(0, 0.0), None);
    // Local 1000 ms ↔ relay 501_000 µs: offset +500 ms, rtt 0.
    let mut echo = Vec::new();
    append_time_sync(&mut echo, 1_000_000, 1_500_000);
    p.on_datagram(&echo, 1000.0, &mut out);
    // The broadcaster's frame clock is 2 s behind the relay's.
    p.on_datagram(&mapping(2_000_000), 1000.0, &mut out);
    // A frame stamped 0 (relay 2_000_000 µs) seen at local 1600 ms
    // (relay 2_100_000 µs): 100 ms capture-to-now.
    assert_eq!(p.capture_latency_ms(0, 1600.0), Some(100.0));
    assert!(out.is_empty(), "TimeSync and ClockMapping are not media");
}

#[test]
fn pings_time_sync_at_once_then_every_two_seconds() {
    let mut p = Pipeline::new(PlayoutPreset::Balanced);
    assert!(p.time_sync_ping(0.0).is_some());
    assert!(p.time_sync_ping(1999.0).is_none());
    assert!(p.time_sync_ping(2000.0).is_some());
}
