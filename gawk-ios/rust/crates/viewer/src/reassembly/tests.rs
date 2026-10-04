//! Restated from the SPA's `packetize-reassemble.test.ts` and
//! `reassembler-parity.test.ts` (same frames, same losses, same counts), so
//! the two viewers' drop policies can't drift apart unnoticed. Cases that
//! test the TS packetizer or JS buffer types, and the stripe-only delta
//! evidence and accounting, have no counterpart here.

use super::*;
use gawk_wire::{
    AudioConfig, AudioFrameHeader, ChunkBudget, ParityChunkHeader, VideoChunkHeader,
    append_audio_config, append_audio_frame, append_clock_mapping, append_decoder_config,
    append_parity_chunk, append_video_chunk, append_viewer_count, compute_parity, split_frame,
};

fn pattern(len: usize, seed: usize) -> Vec<u8> {
    (0..len).map(|i| ((i * 31 + seed) & 0xff) as u8).collect()
}

/// The datagrams of one frame, and its parity datagrams at `k` symbols.
fn packetize(
    frame_id: u32,
    keyframe: bool,
    ts: u64,
    data: &[u8],
    k: usize,
) -> (Vec<Vec<u8>>, Vec<Vec<u8>>) {
    let chunks = split_frame(data, &ChunkBudget::new()).unwrap();
    let count = chunks.len() as u16;
    let dgrams = chunks
        .iter()
        .enumerate()
        .map(|(i, c)| {
            let mut d = Vec::new();
            let h = VideoChunkHeader {
                keyframe,
                frame_id,
                chunk_index: i as u16,
                chunk_count: count,
                timestamp_us: ts,
            };
            append_video_chunk(&mut d, &h, c).unwrap();
            d
        })
        .collect();
    let parity = compute_parity(&chunks, k)
        .unwrap()
        .iter()
        .enumerate()
        .map(|(i, p)| {
            let mut d = Vec::new();
            let h = ParityChunkHeader {
                frame_id,
                parity_index: i as u8,
                chunk_count: count,
                frame_bytes: data.len() as u32,
            };
            append_parity_chunk(&mut d, &h, p).unwrap();
            d
        })
        .collect();
    (dgrams, parity)
}

fn frames(out: &[Demuxed]) -> Vec<&AssembledFrame> {
    out.iter()
        .filter_map(|d| match d {
            Demuxed::Frame(f) => Some(f),
            _ => None,
        })
        .collect()
}

fn ids(out: &[Demuxed]) -> Vec<u32> {
    frames(out).iter().map(|f| f.frame_id).collect()
}

fn push_all(r: &mut Reassembler, dgrams: &[Vec<u8>], out: &mut Vec<Demuxed>) {
    for d in dgrams {
        r.push(d, out);
    }
}

fn push_frame(r: &mut Reassembler, frame_id: u32, keyframe: bool, out: &mut Vec<Demuxed>) {
    let (d, _) = packetize(
        frame_id,
        keyframe,
        frame_id as u64,
        &pattern(100, frame_id as usize),
        0,
    );
    push_all(r, &d, out);
}

/// The parity tests' `feed`: every datagram except the data chunks in
/// `drop`, then the parity. Timestamps are `frame_id * 1000`, as there.
fn feed(
    r: &mut Reassembler,
    frame_id: u32,
    data: &[u8],
    k: usize,
    drop: &[usize],
    out: &mut Vec<Demuxed>,
) -> usize {
    let (dgrams, parity) = packetize(frame_id, false, frame_id as u64 * 1000, data, k);
    for (i, d) in dgrams.iter().enumerate() {
        if !drop.contains(&i) {
            r.push(d, out);
        }
    }
    push_all(r, &parity, out);
    dgrams.len()
}

// --- packetize → reassemble round trip -------------------------------------

#[test]
fn reassembles_a_multi_chunk_frame_byte_for_byte() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let data = pattern(5000, 42);
    let (d, _) = packetize(3, true, 999, &data, 0);
    push_all(&mut r, &d, &mut out);
    let f = frames(&out);
    assert_eq!(f.len(), 1);
    assert_eq!(
        (f[0].frame_id, f[0].keyframe, f[0].timestamp_us),
        (3, true, 999)
    );
    assert_eq!(f[0].data, data);
}

#[test]
fn reassembles_despite_chunk_reordering_and_duplicates() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let data = pattern(4000, 7);
    let (d, _) = packetize(1, false, 5, &data, 0);
    for i in [2, 0, 0, 3, 1] {
        r.push(&d[i], &mut out);
    }
    assert_eq!(frames(&out).len(), 1);
    assert_eq!(frames(&out)[0].data, data);
    assert_eq!(r.stats().duplicate_chunks, 1);
}

#[test]
fn never_emits_a_frame_with_a_missing_chunk() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (d, _) = packetize(1, false, 0, &pattern(3000, 3), 0);
    push_all(&mut r, &d[1..], &mut out); // chunk 0 lost
    assert!(frames(&out).is_empty());
}

#[test]
fn round_trips_an_empty_frame() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (d, _) = packetize(9, false, 1, &[], 0);
    push_all(&mut r, &d, &mut out);
    assert_eq!(frames(&out).len(), 1);
    assert!(frames(&out)[0].data.is_empty());
}

// --- config handling -------------------------------------------------------

fn config(codec: &str, extradata: &[u8]) -> Vec<u8> {
    let mut d = Vec::new();
    append_decoder_config(&mut d, codec, extradata).unwrap();
    d
}

fn configs(out: &[Demuxed]) -> Vec<&VideoConfig> {
    out.iter()
        .filter_map(|d| match d {
            Demuxed::Config(c) => Some(c),
            _ => None,
        })
        .collect()
}

#[test]
fn emits_a_config_once_and_deduplicates_re_emissions() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let cfg = config("avc1.42E02A", &[1, 2, 3]);
    r.push(&cfg, &mut out);
    r.push(&cfg.clone(), &mut out);
    assert_eq!(configs(&out).len(), 1);
    assert_eq!(configs(&out)[0].codec, "avc1.42E02A");
    assert_eq!(r.stats().duplicate_configs, 1);
}

#[test]
fn emits_again_when_the_config_actually_changes() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    r.push(&config("avc1.42E02A", &[1]), &mut out);
    r.push(&config("vp8", &[]), &mut out);
    let codecs: Vec<&str> = configs(&out).iter().map(|c| c.codec.as_str()).collect();
    assert_eq!(codecs, ["avc1.42E02A", "vp8"]);
}

// --- ordering policy -------------------------------------------------------

#[test]
fn rejects_a_wholly_late_delta_frame_chunk_by_chunk_without_assembling_it() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    push_frame(&mut r, 2, false, &mut out);
    push_frame(&mut r, 1, false, &mut out); // entirely after a newer frame
    assert_eq!(ids(&out), [2]);
    let s = r.stats();
    assert_eq!(s.stale_chunks, 1);
    assert_eq!(s.frames_dropped_late, 0);
    assert_eq!(s.frames_dropped_incomplete, 0);
}

#[test]
fn still_counts_a_partially_assembled_frame_that_completes_late_as_dropped_late() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (late, _) = packetize(1, false, 1, &pattern(2000, 1), 0); // 2 chunks
    r.push(&late[0], &mut out); // frame 1's assembly exists...
    push_frame(&mut r, 2, false, &mut out); // ...when frame 2 emits past it
    r.push(&late[1], &mut out); // the tail completes it — late
    assert_eq!(ids(&out), [2]);
    assert_eq!(r.stats().frames_dropped_late, 1);
    assert_eq!(r.stats().stale_chunks, 0);
}

#[test]
fn always_emits_keyframes_allowing_broadcaster_restart_recovery() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    push_frame(&mut r, 100, true, &mut out);
    push_frame(&mut r, 101, false, &mut out);
    push_frame(&mut r, 0, true, &mut out); // restarted, IDs reset
    push_frame(&mut r, 1, false, &mut out);
    assert_eq!(ids(&out), [100, 101, 0, 1]);
}

#[test]
fn recovers_new_session_deltas_after_restart_via_the_stream_keyframe_watermark_reset() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    push_frame(&mut r, 100_000, false, &mut out); // the old session's last delta
    r.note_stream_keyframe(3); // the new session's keyframe, via a stream
    push_frame(&mut r, 4, false, &mut out);
    push_frame(&mut r, 5, false, &mut out);
    assert_eq!(ids(&out), [100_000, 4, 5]);
    assert_eq!(r.stats().frames_dropped_late, 0);
}

#[test]
fn accepts_deltas_across_frame_id_rollover() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    for id in [0xffff_fffe, 0xffff_ffff, 0, 1] {
        push_frame(&mut r, id, false, &mut out);
    }
    assert_eq!(ids(&out), [0xffff_fffe, 0xffff_ffff, 0, 1]);
    assert_eq!(r.stats().frames_dropped_late, 0);
}

#[test]
fn evicts_the_oldest_incomplete_assembly_under_pressure() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (stuck, _) = packetize(0, false, 0, &pattern(3000, 0), 0);
    r.push(&stuck[0], &mut out);
    for id in 1..=8u32 {
        let (d, _) = packetize(id, false, 0, &pattern(3000, id as usize), 0);
        r.push(&d[0], &mut out);
    }
    assert_eq!(r.stats().frames_dropped_incomplete, 1);
    // Frame 0 was evicted; its leftovers start a fresh, incomplete assembly.
    push_all(&mut r, &stuck[1..], &mut out);
    assert!(frames(&out).is_empty());
}

// --- side messages ---------------------------------------------------------

#[test]
fn routes_clock_mappings_last_one_wins_and_drops_malformed_ones() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    for offset in [1_500_000i64, -42] {
        let mut d = Vec::new();
        append_clock_mapping(&mut d, offset);
        r.push(&d, &mut out);
    }
    assert_eq!(
        out,
        [Demuxed::ClockMapping(1_500_000), Demuxed::ClockMapping(-42)]
    );
    assert_eq!(r.stats().bad_datagrams, 0);

    let mut d = Vec::new();
    append_clock_mapping(&mut d, 7);
    r.push(&d[..5], &mut out); // truncated
    assert_eq!(out.len(), 2);
    assert_eq!(r.stats().bad_datagrams, 1);
}

#[test]
fn routes_viewer_counts_last_one_wins_and_drops_malformed_ones() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    for count in [1, 4] {
        let mut d = Vec::new();
        append_viewer_count(&mut d, count);
        r.push(&d, &mut out);
    }
    assert_eq!(out, [Demuxed::ViewerCount(1), Demuxed::ViewerCount(4)]);

    let mut d = Vec::new();
    append_viewer_count(&mut d, 7);
    r.push(&d[..4], &mut out); // truncated
    assert_eq!(out.len(), 2);
    assert_eq!(r.stats().bad_datagrams, 1);
}

// --- malformed input -------------------------------------------------------

#[test]
fn counts_bad_datagrams_without_emitting() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    r.push(&[], &mut out);
    r.push(&[0x02, 0x01], &mut out); // wrong version
    r.push(&[0x01, 0x7f], &mut out); // unknown type
    r.push(&[0x01, 0x01, 0x00], &mut out); // truncated video chunk
    r.push(&[0x01, 0x02, 0x00, 0x05, 0x76], &mut out); // codecLen overrun
    assert_eq!(r.stats().bad_datagrams, 5);
    assert!(out.is_empty());
}

#[test]
fn rejects_a_chunk_whose_count_disagrees_with_its_frame() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (a, _) = packetize(5, false, 0, &pattern(2000, 1), 0);
    assert_eq!(a.len(), 2);
    r.push(&a[0], &mut out);
    let (forged, _) = packetize(5, false, 0, &pattern(3000, 2), 0); // claims 3
    r.push(&forged[1], &mut out);
    assert_eq!(r.stats().bad_datagrams, 1);
    r.push(&a[1], &mut out);
    assert_eq!(frames(&out).len(), 1); // the original still completes
}

// --- audio demux -----------------------------------------------------------

fn audio_frame(seq: u32, ts: u64, payload: &[u8]) -> Vec<u8> {
    let mut d = Vec::new();
    let h = AudioFrameHeader {
        seq,
        timestamp_us: ts,
    };
    append_audio_frame(&mut d, &h, payload).unwrap();
    d
}

fn audio_config(sample_rate: u32) -> Vec<u8> {
    let mut d = Vec::new();
    let c = AudioConfig {
        codec: "opus",
        sample_rate,
        channels: 2,
        description: &[],
    };
    append_audio_config(&mut d, &c).unwrap();
    d
}

#[test]
fn routes_audio_frames_straight_through() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let payload = pattern(320, 7);
    r.push(&audio_frame(42, 1_234_000, &payload), &mut out);
    assert_eq!(
        out,
        [Demuxed::AudioFrame(AudioPacket {
            seq: 42,
            timestamp_us: 1_234_000,
            payload,
        })]
    );
}

#[test]
fn deduplicates_the_1_hz_audio_config_re_sends() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    for _ in 0..3 {
        r.push(&audio_config(48_000), &mut out);
    }
    assert_eq!(out.len(), 1);
    r.push(&audio_config(44_100), &mut out);
    assert_eq!(out.len(), 2);
    assert!(matches!(&out[1], Demuxed::AudioConfig(c) if c.sample_rate == 44_100));
    assert_eq!(r.stats().duplicate_configs, 2);
}

#[test]
fn counts_audio_without_disturbing_the_video_counters() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    push_frame(&mut r, 1, false, &mut out);
    let audio = audio_frame(0, 0, &pattern(320, 2));
    r.push(&audio, &mut out);
    let s = r.stats();
    assert_eq!(frames(&out).len(), 1);
    assert_eq!(s.frames_completed, 1);
    assert_eq!(s.audio_packets_received, 1);
    assert_eq!(s.audio_bytes_received, audio.len() as u64);
    assert_eq!(s.bad_datagrams, 0);
}

#[test]
fn malformed_audio_datagrams_count_bad_and_emit_nothing() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let frame = audio_frame(1, 0, &pattern(10, 1));
    let cfg = audio_config(48_000);
    r.push(&frame[..10], &mut out); // truncated header
    r.push(&frame[..16], &mut out); // header only, empty payload
    r.push(&cfg[..6], &mut out); // truncated config
    assert!(out.is_empty());
    assert_eq!(r.stats().bad_datagrams, 3);
}

// --- parity recovery (reassembler-parity.test.ts) --------------------------

#[test]
fn recovers_a_frame_missing_one_chunk() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let data = pattern(5000, 1);
    feed(&mut r, 10, &data, 2, &[2], &mut out);
    assert_eq!(frames(&out).len(), 1);
    assert_eq!(frames(&out)[0].data, data);
    assert_eq!(r.stats().frames_recovered_by_parity, 1);
    assert_eq!(r.stats().frames_dropped_incomplete, 0);
}

#[test]
fn recovers_a_frame_missing_two_chunks_at_k2() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let data = pattern(5000, 2);
    let n = feed(&mut r, 11, &data, 2, &[0, 3], &mut out);
    assert!(n > 3);
    assert_eq!(frames(&out)[0].data, data);
    assert_eq!(r.stats().frames_recovered_by_parity, 1);
}

#[test]
fn recovers_when_the_last_short_chunk_is_the_missing_one() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let data = pattern(5000, 3);
    let (dgrams, parity) = packetize(12, false, 12_000, &data, 2);
    push_all(&mut r, &dgrams[..dgrams.len() - 1], &mut out);
    push_all(&mut r, &parity, &mut out);
    assert_eq!(frames(&out).len(), 1);
    assert_eq!(frames(&out)[0].data, data);
}

#[test]
fn cannot_recover_two_losses_at_k1() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    feed(&mut r, 13, &pattern(5000, 4), 1, &[1, 2], &mut out);
    assert!(frames(&out).is_empty());
    assert_eq!(r.stats().frames_recovered_by_parity, 0);
    assert_eq!(r.stats().bad_datagrams, 0); // still held, never an error
}

#[test]
fn emits_the_frame_without_parity_when_nothing_was_lost() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    feed(&mut r, 14, &pattern(5000, 5), 2, &[], &mut out);
    assert_eq!(frames(&out).len(), 1);
    assert_eq!(r.stats().frames_recovered_by_parity, 0);
    assert_eq!(r.stats().parity_chunks_received, 2);
}

#[test]
fn ignores_parity_that_arrives_after_the_frame_completed() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (dgrams, parity) = packetize(15, false, 15_000, &pattern(3000, 6), 2);
    push_all(&mut r, &dgrams, &mut out);
    push_all(&mut r, &parity, &mut out);
    assert_eq!(frames(&out).len(), 1);
    assert_eq!(r.stats().bad_datagrams, 0);
}

#[test]
fn does_not_inflate_frames_dropped_incomplete_on_a_clean_link() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    for i in 1..=40u32 {
        feed(&mut r, i, &pattern(3000, i as usize), 2, &[], &mut out);
    }
    assert_eq!(frames(&out).len(), 40);
    assert_eq!(r.stats().frames_dropped_incomplete, 0);
    assert_eq!(r.stats().frames_recovered_by_parity, 0);
}

#[test]
fn still_attaches_parity_for_a_frame_not_yet_emitted() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let data = pattern(5000, 21);
    let (dgrams, parity) = packetize(30, false, 30_000, &data, 2);
    push_all(&mut r, &parity, &mut out);
    push_all(&mut r, &dgrams[1..], &mut out);
    assert_eq!(frames(&out)[0].data, data);
    assert_eq!(frames(&out)[0].timestamp_us, 30_000);
}

#[test]
fn recovers_when_parity_arrives_before_the_surviving_chunks() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let data = pattern(5000, 7);
    let (dgrams, parity) = packetize(16, false, 16_000, &data, 2);
    push_all(&mut r, &parity, &mut out);
    for (i, d) in dgrams.iter().enumerate() {
        if i != 1 {
            r.push(d, &mut out);
        }
    }
    assert_eq!(frames(&out)[0].data, data);
    assert_eq!(frames(&out)[0].timestamp_us, 16_000);
}

#[test]
fn keeps_the_timestamp_of_a_frame_whose_parity_arrived_first() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (dgrams, parity) = packetize(40, false, 40_000, &pattern(800, 3), 2);
    r.push(&parity[0], &mut out);
    push_all(&mut r, &dgrams, &mut out);
    assert_eq!(frames(&out).len(), 1);
    assert_eq!(frames(&out)[0].timestamp_us, 40_000);
}

#[test]
fn is_unaffected_when_no_parity_is_sent() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    feed(&mut r, 18, &pattern(5000, 9), 0, &[2], &mut out);
    assert!(frames(&out).is_empty());
    assert_eq!(r.stats().parity_chunks_received, 0);
    assert_eq!(r.stats().frames_recovered_by_parity, 0);
}

#[test]
fn survives_a_parity_chunk_whose_frame_it_has_never_seen() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let (_, parity) = packetize(19, false, 19_000, &pattern(3000, 10), 2);
    push_all(&mut r, &parity, &mut out);
    assert_eq!(r.stats().bad_datagrams, 0);
    assert_eq!(r.stats().parity_chunks_received, 2);
}

// --- parity shortfall accounting -------------------------------------------

/// Eight parity-free frames each missing a chunk: the eighth evicts the
/// oldest assembly, the frame under test.
fn evict_after(r: &mut Reassembler, out: &mut Vec<Demuxed>) {
    for id in 100..108u32 {
        feed(r, id, &pattern(3000, id as usize), 0, &[0], out);
    }
}

#[test]
fn counts_a_frame_given_up_on_with_too_few_symbols() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    feed(&mut r, 1, &pattern(9000, 7), 2, &[0, 1, 2], &mut out);
    assert_eq!(r.stats().frames_recovered_by_parity, 0);
    evict_after(&mut r, &mut out);
    let s = r.stats();
    assert!(s.frames_dropped_incomplete >= 1);
    assert_eq!(s.parity_insufficient, 1);
    assert_eq!(s.parity_recovery_failures, 0); // the solve was never reached
}

#[test]
fn does_not_count_a_frame_that_parity_repaired() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    feed(&mut r, 1, &pattern(9000, 8), 2, &[1], &mut out);
    evict_after(&mut r, &mut out);
    assert_eq!(r.stats().frames_recovered_by_parity, 1);
    assert_eq!(r.stats().parity_insufficient, 0);
}

#[test]
fn counts_a_single_chunk_delta_whose_only_chunk_was_lost() {
    let (mut r, mut out) = (Reassembler::new(), Vec::new());
    let n = feed(&mut r, 1, &pattern(200, 9), 2, &[0], &mut out);
    assert_eq!(n, 1);
    evict_after(&mut r, &mut out);
    assert!(r.stats().frames_dropped_incomplete >= 1);
    assert_eq!(r.stats().parity_insufficient, 1);
}
