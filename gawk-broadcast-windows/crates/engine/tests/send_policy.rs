//! The send-policy parity tests (docs/38 D5, WB2): every row of the parity
//! table asserted against a scripted `RelaySession` fake — the send policy
//! is defined by what happens when sends FAIL, and a real relay will not
//! produce those failures on demand.

use gawk_engine::clock::testing::FakeClock;
use gawk_engine::media::{AccessUnit, AudioFormat, AudioPacket};
use gawk_engine::relay::{
    BoxFuture, CancelSignal, KeyframeOutcome, KeyframeWriter, RelaySession, SendDatagramError,
    ServerStream,
};
use gawk_engine::sender::{KEYFRAME_SUPERSEDED_CODE, Sender};
use gawk_wire as wire;
use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

// --- The scripted fake relay -------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq)]
enum KfEvent {
    Written(Vec<u8>),
    Cancelled(u32),
    Aborted(u32),
}

#[derive(Clone, Copy)]
enum KfMode {
    /// Write completes immediately.
    Instant,
    /// Write parks until the cancel signal fires.
    HoldUntilCancelled,
}

struct FakeWriter {
    mode: KfMode,
    log: Arc<Mutex<Vec<KfEvent>>>,
}

impl KeyframeWriter for FakeWriter {
    fn write(
        self: Box<Self>,
        msg: Vec<u8>,
        cancel: CancelSignal,
    ) -> BoxFuture<'static, KeyframeOutcome> {
        Box::pin(async move {
            match self.mode {
                KfMode::Instant => {
                    self.log.lock().unwrap().push(KfEvent::Written(msg));
                    KeyframeOutcome::Sent
                }
                KfMode::HoldUntilCancelled => match cancel.await {
                    Ok(code) => {
                        self.log.lock().unwrap().push(KfEvent::Cancelled(code));
                        KeyframeOutcome::Cancelled
                    }
                    Err(_) => KeyframeOutcome::Cancelled,
                },
            }
        })
    }

    fn abort(self: Box<Self>, code: u32) {
        self.log.lock().unwrap().push(KfEvent::Aborted(code));
    }
}

#[derive(Default)]
struct FakeRelay {
    /// Every datagram accepted, in order.
    datagrams: Mutex<Vec<Vec<u8>>>,
    /// Per-send scripted outcomes; when exhausted, sends succeed.
    script: Mutex<VecDeque<Result<(), SendDatagramError>>>,
    /// Keyframe writer modes, popped per open; when exhausted, Instant.
    kf_modes: Mutex<VecDeque<KfMode>>,
    kf_log: Arc<Mutex<Vec<KfEvent>>>,
}

impl FakeRelay {
    fn script_sends(&self, outcomes: Vec<Result<(), SendDatagramError>>) {
        *self.script.lock().unwrap() = outcomes.into();
    }
    fn script_keyframes(&self, modes: Vec<KfMode>) {
        *self.kf_modes.lock().unwrap() = modes.into();
    }
    fn sent_of_type(&self, msg_type: u8) -> Vec<Vec<u8>> {
        self.datagrams
            .lock()
            .unwrap()
            .iter()
            .filter(|d| d[1] == msg_type)
            .cloned()
            .collect()
    }
    fn kf_events(&self) -> Vec<KfEvent> {
        self.kf_log.lock().unwrap().clone()
    }
}

impl RelaySession for FakeRelay {
    fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError> {
        if let Some(outcome) = self.script.lock().unwrap().pop_front() {
            outcome?;
        }
        self.datagrams.lock().unwrap().push(dgram.to_vec());
        Ok(())
    }

    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        let mode = self
            .kf_modes
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(KfMode::Instant);
        let log = self.kf_log.clone();
        Box::pin(async move { Ok(Box::new(FakeWriter { mode, log }) as Box<dyn KeyframeWriter>) })
    }

    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        Box::pin(std::future::pending())
    }

    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(std::future::pending())
    }

    fn closed(&self) -> BoxFuture<'_, gawk_engine::relay::SessionClose> {
        Box::pin(std::future::pending())
    }
}

fn setup() -> (Arc<FakeRelay>, Arc<FakeClock>, Sender) {
    let relay = Arc::new(FakeRelay::default());
    let clock = Arc::new(FakeClock::default());
    let sender = Sender::new(relay.clone(), clock.clone());
    (relay, clock, sender)
}

fn delta(len: usize, ts: u64) -> AccessUnit {
    AccessUnit {
        data: vec![0xAA; len],
        timestamp_us: ts,
        keyframe: false,
    }
}

fn keyframe(len: usize, ts: u64) -> AccessUnit {
    AccessUnit {
        data: vec![0xBB; len],
        timestamp_us: ts,
        keyframe: true,
    }
}

// --- Delta path ----------------------------------------------------------------

#[tokio::test]
async fn frame_ids_are_shared_monotonic_and_survive_relay_swaps() {
    let (relay_a, _clock, sender) = setup();
    sender.send_video(delta(10, 1)).await;

    // Resume onto a fresh session: the frameId space carries over —
    // continuity IS the resume signal (there is no server-side epoch).
    let relay_b = Arc::new(FakeRelay::default());
    sender.set_relay(relay_b.clone());
    sender.send_video(delta(10, 2)).await;

    let (h_a, _) =
        wire::parse_video_chunk(&relay_a.sent_of_type(wire::TYPE_VIDEO_CHUNK)[0]).unwrap();
    let (h_b, _) =
        wire::parse_video_chunk(&relay_b.sent_of_type(wire::TYPE_VIDEO_CHUNK)[0]).unwrap();
    assert_eq!(h_a.frame_id, 0);
    assert_eq!(h_b.frame_id, 1);
}

#[tokio::test]
async fn a_mid_frame_failure_drops_the_remainder_and_counts_it() {
    let (relay, _clock, sender) = setup();
    // Three chunks; the second send fails with an ordinary error.
    relay.script_sends(vec![
        Ok(()),
        Err(SendDatagramError::Failed("uplink".into())),
    ]);
    sender
        .send_video(delta(wire::MAX_CHUNK_PAYLOAD * 2 + 100, 1))
        .await;

    let sent = relay.sent_of_type(wire::TYPE_VIDEO_CHUNK);
    assert_eq!(
        sent.len(),
        1,
        "the remainder must not be pushed — it is dead weight"
    );
    let st = sender.stats();
    assert_eq!(st.frames_dropped_at_send, 1);
    assert_eq!(st.sent_frames, 0);

    // The NEXT frame is unaffected and takes the next frameId.
    sender.send_video(delta(10, 2)).await;
    let sent = relay.sent_of_type(wire::TYPE_VIDEO_CHUNK);
    let (h, _) = wire::parse_video_chunk(sent.last().unwrap()).unwrap();
    assert_eq!(h.frame_id, 1);
    assert_eq!(sender.stats().sent_frames, 1);
}

#[tokio::test]
async fn too_large_shrinks_the_budget_once_and_it_sticks() {
    let (relay, _clock, sender) = setup();
    relay.script_sends(vec![Err(SendDatagramError::TooLarge {
        max_datagram_size: Some(620),
    })]);

    // 1000 bytes fits one 1180-budget chunk; after the too-large error the
    // budget shrinks to 620-20=600 and the frame re-chunks into two.
    sender.send_video(delta(1000, 1)).await;
    let sent = relay.sent_of_type(wire::TYPE_VIDEO_CHUNK);
    assert_eq!(sent.len(), 2);
    let (h, p) = wire::parse_video_chunk(&sent[0]).unwrap();
    assert_eq!((h.chunk_index, h.chunk_count, p.len()), (0, 2, 600));
    let st = sender.stats();
    assert_eq!(st.sent_frames, 1);
    assert_eq!(st.frames_dropped_at_send, 0);

    // The learned budget sticks for every later frame.
    sender.send_video(delta(1000, 2)).await;
    let sent = relay.sent_of_type(wire::TYPE_VIDEO_CHUNK);
    assert_eq!(sent.len(), 4);
    let (h, _) = wire::parse_video_chunk(&sent[2]).unwrap();
    assert_eq!(h.chunk_count, 2);
}

#[tokio::test]
async fn a_zero_length_frame_still_exists_on_the_wire() {
    let (relay, _clock, sender) = setup();
    sender.send_video(delta(0, 1)).await;
    let sent = relay.sent_of_type(wire::TYPE_VIDEO_CHUNK);
    assert_eq!(sent.len(), 1);
    let (h, p) = wire::parse_video_chunk(&sent[0]).unwrap();
    assert_eq!((h.chunk_index, h.chunk_count), (0, 1));
    assert!(p.is_empty());
}

// --- Keyframe path ---------------------------------------------------------------

/// Yields until `cond` holds — spawned keyframe writers complete on the next
/// scheduler turn, and `Sender::wait()` cannot be used mid-test because it
/// is teardown (it closes the sender to new writes permanently).
async fn settle(mut cond: impl FnMut() -> bool) {
    for _ in 0..1000 {
        if cond() {
            return;
        }
        tokio::task::yield_now().await;
    }
    panic!("condition never settled");
}

#[tokio::test]
async fn keyframes_embed_the_config_and_never_send_it_standalone() {
    let (relay, _clock, sender) = setup();

    // Before the codec is known: config_len 0 ("not yet", not "never").
    sender.send_video(keyframe(100, 1)).await;
    settle(|| relay.kf_events().len() == 1).await;
    let events = relay.kf_events();
    let KfEvent::Written(msg) = &events[0] else {
        panic!("expected a write")
    };
    let h = wire::parse_stream_frame_header(msg).unwrap();
    assert_eq!(h.config_len, 0);

    // Once known, every keyframe embeds it — and no standalone 0x02 datagram
    // is ever sent (the relay's priming caches the whole StreamFrame).
    sender.set_codec("avc1.42E02A");
    sender.send_video(keyframe(100, 2)).await;
    settle(|| relay.kf_events().len() == 2).await;
    let events = relay.kf_events();
    let KfEvent::Written(msg) = &events[1] else {
        panic!("expected a write")
    };
    let h = wire::parse_stream_frame_header(msg).unwrap();
    assert!(h.config_len > 0);
    let config_bytes = &msg
        [wire::STREAM_FRAME_HEADER_SIZE..wire::STREAM_FRAME_HEADER_SIZE + h.config_len as usize];
    let cfg = wire::parse_decoder_config(config_bytes).unwrap();
    assert_eq!(cfg.codec, "avc1.42E02A");
    assert!(
        cfg.extradata.is_empty(),
        "Annex-B path: extradata stays empty, permanently"
    );
    assert!(relay.sent_of_type(wire::TYPE_DECODER_CONFIG).is_empty());
    assert_eq!(sender.stats().configs_sent, 1);
}

#[tokio::test]
async fn at_most_one_keyframe_stream_in_flight_newest_wins() {
    let (relay, _clock, sender) = setup();
    relay.script_keyframes(vec![KfMode::HoldUntilCancelled, KfMode::Instant]);

    sender.send_video(keyframe(100, 1)).await; // parks
    sender.send_video(keyframe(100, 2)).await; // supersedes it
    sender.wait().await;

    let events = relay.kf_events();
    assert!(
        events.contains(&KfEvent::Cancelled(KEYFRAME_SUPERSEDED_CODE)),
        "the older stream must be reset with the supersede code: {events:?}"
    );
    let st = sender.stats();
    assert_eq!(st.keyframe_streams_superseded, 1);
    assert_eq!(st.keyframe_streams_sent, 1);
    // Mirroring the Go engine: the cancelled write also lands in the failed
    // counter (the supersede was counted separately).
    assert_eq!(st.keyframe_streams_failed, 1);
}

#[tokio::test]
async fn a_closed_sender_aborts_new_keyframes_instead_of_writing() {
    let (relay, _clock, sender) = setup();
    sender.wait().await; // teardown: closed to new writes
    sender.send_video(keyframe(100, 1)).await;
    assert_eq!(
        relay.kf_events(),
        vec![KfEvent::Aborted(KEYFRAME_SUPERSEDED_CODE)]
    );
    assert_eq!(sender.stats().keyframe_streams_failed, 1);
}

#[tokio::test]
async fn set_relay_cancels_the_inflight_keyframe() {
    let (relay, _clock, sender) = setup();
    relay.script_keyframes(vec![KfMode::HoldUntilCancelled]);
    sender.send_video(keyframe(100, 1)).await;

    sender.set_relay(Arc::new(FakeRelay::default()));
    sender.wait().await;
    assert!(
        relay
            .kf_events()
            .contains(&KfEvent::Cancelled(KEYFRAME_SUPERSEDED_CODE))
    );
}

// --- Parity (R29) ------------------------------------------------------------------

#[tokio::test]
async fn parity_is_gated_on_the_capability_flag_not_just_the_level() {
    let (relay, _clock, sender) = setup();

    // No capabilities seen: byte-identical to pre-R29 — no parity.
    sender
        .send_video(delta(wire::MAX_CHUNK_PAYLOAD * 2 + 100, 1))
        .await;
    assert!(relay.sent_of_type(wire::TYPE_PARITY_CHUNK).is_empty());

    // A level without the flag must ALSO emit nothing: the flag is what says
    // the relay filters symbols per subscriber.
    sender.apply_capabilities(wire::RelayCapabilities {
        flags: 0,
        parity_level: 2,
    });
    sender
        .send_video(delta(wire::MAX_CHUNK_PAYLOAD * 2 + 100, 2))
        .await;
    assert!(relay.sent_of_type(wire::TYPE_PARITY_CHUNK).is_empty());

    // Flag + level: parity trails the data chunks.
    sender.apply_capabilities(wire::RelayCapabilities {
        flags: wire::CAP_PARITY_CHUNKS,
        parity_level: 2,
    });
    let frame_len = wire::MAX_CHUNK_PAYLOAD * 2 + 100;
    sender.send_video(delta(frame_len, 3)).await;
    let parity = relay.sent_of_type(wire::TYPE_PARITY_CHUNK);
    assert_eq!(parity.len(), 2);
    let (h, _) = wire::parse_parity_chunk(&parity[0]).unwrap();
    assert_eq!(
        (h.parity_index, h.chunk_count, h.frame_bytes as usize),
        (0, 3, frame_len)
    );
    // Ordering: all data before any parity (useless before the loss it
    // repairs is known).
    let all = relay.datagrams.lock().unwrap().clone();
    let first_parity = all
        .iter()
        .position(|d| d[1] == wire::TYPE_PARITY_CHUNK)
        .unwrap();
    assert!(
        all[..first_parity]
            .iter()
            .filter(|d| d[1] == wire::TYPE_VIDEO_CHUNK)
            .count()
            >= 3
    );
}

#[tokio::test]
async fn a_parity_send_failure_is_not_a_frame_failure() {
    let (relay, _clock, sender) = setup();
    sender.apply_capabilities(wire::RelayCapabilities {
        flags: wire::CAP_PARITY_CHUNKS,
        parity_level: 1,
    });
    // Two data sends succeed; the parity send fails.
    relay.script_sends(vec![
        Ok(()),
        Ok(()),
        Err(SendDatagramError::Failed("x".into())),
    ]);
    sender
        .send_video(delta(wire::MAX_CHUNK_PAYLOAD + 100, 1))
        .await;

    let st = sender.stats();
    assert_eq!(
        st.sent_frames, 1,
        "the data is already out — the frame shipped"
    );
    assert_eq!(st.frames_dropped_at_send, 0);
    assert_eq!(st.parity_chunks_sent, 0);
}

// --- Audio lane (R25) -----------------------------------------------------------------

fn opus_format() -> AudioFormat {
    AudioFormat {
        codec: "opus".into(),
        sample_rate: 48_000,
        channels: 2,
        bitrate_bps: 128_000,
        source: "test".into(),
    }
}

fn packet(ts: u64) -> AudioPacket {
    AudioPacket {
        data: vec![0xFC, 0xFF, 0xFE],
        timestamp_us: ts,
    }
}

#[tokio::test]
async fn audio_config_piggybacks_at_one_hz_and_seq_always_advances() {
    let (relay, clock, sender) = setup();
    sender.set_audio_format(opus_format());

    // First packet carries the config.
    sender.send_audio(packet(0));
    assert_eq!(relay.sent_of_type(wire::TYPE_AUDIO_CONFIG).len(), 1);
    // Within the second: no re-send.
    clock.advance_ms(500);
    sender.send_audio(packet(20_000));
    assert_eq!(relay.sent_of_type(wire::TYPE_AUDIO_CONFIG).len(), 1);
    // Past 1 s: the config rides again.
    clock.advance_ms(600);
    sender.send_audio(packet(40_000));
    assert_eq!(relay.sent_of_type(wire::TYPE_AUDIO_CONFIG).len(), 2);

    // A failed frame send drops the packet but the sequence still advances:
    // a viewer seeing a gap is seeing the truth.
    relay.script_sends(vec![Err(SendDatagramError::Failed("x".into()))]);
    sender.send_audio(packet(60_000));
    sender.send_audio(packet(80_000));
    let frames = relay.sent_of_type(wire::TYPE_AUDIO_FRAME);
    let seqs: Vec<u32> = frames
        .iter()
        .map(|d| wire::parse_audio_frame(d).unwrap().0.seq)
        .collect();
    assert_eq!(seqs, vec![0, 1, 2, 4], "seq 3 was lost, not reused");
    assert_eq!(sender.stats().audio_packets_dropped, 1);
}

#[tokio::test]
async fn resume_reprimes_the_audio_config_on_the_next_packet() {
    let (_relay_a, clock, sender) = setup();
    sender.set_audio_format(opus_format());
    sender.send_audio(packet(0));

    // Resume: the relay dropped its caches; audio has no keyframe to
    // re-prime through, so the NEXT packet must carry the config even though
    // the 1 Hz cadence has not elapsed.
    let relay_b = Arc::new(FakeRelay::default());
    sender.set_relay(relay_b.clone());
    clock.advance_ms(10);
    sender.send_audio(packet(20_000));
    assert_eq!(relay_b.sent_of_type(wire::TYPE_AUDIO_CONFIG).len(), 1);
}

#[tokio::test]
async fn an_oversize_audio_packet_is_dropped_never_chunked() {
    let (relay, _clock, sender) = setup();
    sender.set_audio_format(opus_format());
    sender.send_audio(AudioPacket {
        data: vec![0; wire::MAX_AUDIO_PAYLOAD + 1],
        timestamp_us: 0,
    });
    assert!(relay.sent_of_type(wire::TYPE_AUDIO_FRAME).is_empty());
    assert_eq!(sender.stats().audio_packets_dropped, 1);
}

#[tokio::test]
async fn a_failed_bitstream_check_latches_audio_off_and_touches_no_video_counter() {
    let relay = Arc::new(FakeRelay::default());
    let clock = Arc::new(FakeClock::default());
    let sender = Sender::with_audio_check(
        relay.clone(),
        clock,
        Some(Box::new(|_pkt, _fmt| Err("TOC disagrees".into()))),
    );
    sender.set_audio_format(opus_format());
    sender.send_audio(packet(0));
    sender.send_audio(packet(20_000));

    assert!(relay.sent_of_type(wire::TYPE_AUDIO_FRAME).is_empty());
    let st = sender.stats();
    assert!(st.audio_errored);
    assert_eq!(
        st.frames_dropped_at_send, 0,
        "audio never touches video counters"
    );
}

// --- Keyframe interval measurement -----------------------------------------------------

#[tokio::test]
async fn keyframe_cadence_is_measured_not_assumed() {
    let (_relay, _clock, sender) = setup();
    sender.send_video(keyframe(10, 1_000_000)).await;
    assert!(
        sender.stats().keyframe_interval_ms.is_none(),
        "one keyframe is no interval"
    );
    sender.send_video(keyframe(10, 1_650_000)).await; // 650 ms gap
    sender.wait().await;
    let ms = sender.stats().keyframe_interval_ms.unwrap();
    assert!((ms - 650.0).abs() < 0.001, "first gap seeds the EMA: {ms}");
}
