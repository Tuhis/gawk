//! The send policy, ported row for row from gawk-broadcast/internal/engine/
//! send.go (docs/38 D5). Friends broadcast over home uplinks, where
//! saturation is a normal condition, so every failure has a stated answer:
//!
//! - Keyframes go whole, on one reliable uni stream — too big and too
//!   important to lose to a single dropped packet.
//! - Deltas are sliced into datagrams: fast and lossy; losing one costs a
//!   frame rather than a GOP.
//! - A mid-frame datagram failure drops the frame's remaining chunks.
//! - A too-large error lowers the chunk budget and re-chunks ONCE; the new
//!   size sticks. Never assume 1200 is reachable.
//! - At most one keyframe stream in flight; a newer keyframe cancels the
//!   older (the browser's unbounded fire-and-forget is a known-weak spot we
//!   do not reproduce in fresh code).

use crate::clock::Clock;
use crate::media::{AUDIO_CONFIG_RESEND_MS, AccessUnit, AudioFormat, AudioPacket};
use crate::relay::{KeyframeOutcome, RelaySession, SendDatagramError};
use crate::stats::Stats;
use gawk_wire as wire;
use std::sync::{Arc, Mutex};
use tokio::sync::oneshot;
use tokio::task::JoinHandle;

/// A batch of encoded datagrams ready to send.
type Datagrams = Vec<Vec<u8>>;

/// Stream error code for a keyframe abandoned because a newer one is ready.
/// Application-defined; the relay treats any reset stream the same way.
pub const KEYFRAME_SUPERSEDED_CODE: u32 = 1;

struct KfSlot {
    /// Cancels the in-flight keyframe write, if any.
    cancel: Option<oneshot::Sender<u32>>,
    /// Generation of the current in-flight write; a finishing writer only
    /// clears the slot if it is still the current one.
    generation: u64,
    /// Set by `wait()` to stop new writes being spawned behind it.
    closed: bool,
    handles: Vec<JoinHandle<()>>,
}

struct State {
    /// Shared frameId space for datagrams and streams; starts at 0, wraps at
    /// u32 — the relay's ingress-loss window depends on that wrap being
    /// serial, and continuity across resume is what tells the relay a
    /// reclaim is a resume (there is no server-side epoch).
    next_frame_id: u32,
    budget: wire::ChunkBudget,
    /// Set only by `apply_capabilities` — the relay decides, because the
    /// relay is what filters symbols per subscriber. 0 against a pre-R29
    /// relay.
    parity_level: u8,
    /// The DecoderConfig, embedded in every keyframe stream and NEVER sent
    /// as a standalone datagram (mirrors the browser since R8; it is what
    /// the relay's priming caches).
    config_datagram: Option<Vec<u8>>,

    audio_format: Option<AudioFormat>,
    audio_config_datagram: Option<Vec<u8>>,
    next_audio_seq: u32,
    audio_config_sent_at_ms: u64,
    audio_config_sent: bool,
    audio_checked: bool,

    st: Stats,
    last_keyframe_us: u64,
    kf_interval_ms: f64,
    kf_interval_ok: bool,
}

/// Verifies the first audio packet's bitstream against the advertised config
/// (R25 Decision 10). Pluggable: the real TOC parser lands with the audio
/// crate (WB5); `None` skips the check.
pub type AudioBitstreamCheck = Box<dyn Fn(&[u8], &AudioFormat) -> Result<(), String> + Send + Sync>;

pub struct Sender {
    relay: Mutex<Arc<dyn RelaySession>>,
    clock: Arc<dyn Clock>,
    // Arc'd so the spawned keyframe writer can report its outcome after the
    // caller has moved on.
    state: Arc<Mutex<State>>,
    kf: Arc<Mutex<KfSlot>>,
    audio_check: Option<AudioBitstreamCheck>,
}

impl Sender {
    pub fn new(relay: Arc<dyn RelaySession>, clock: Arc<dyn Clock>) -> Self {
        Self::with_audio_check(relay, clock, None)
    }

    pub fn with_audio_check(
        relay: Arc<dyn RelaySession>,
        clock: Arc<dyn Clock>,
        audio_check: Option<AudioBitstreamCheck>,
    ) -> Self {
        Self {
            relay: Mutex::new(relay),
            clock,
            state: Arc::new(Mutex::new(State {
                next_frame_id: 0,
                budget: wire::ChunkBudget::new(),
                parity_level: 0,
                config_datagram: None,
                audio_format: None,
                audio_config_datagram: None,
                next_audio_seq: 0,
                audio_config_sent_at_ms: 0,
                audio_config_sent: false,
                audio_checked: false,
                st: Stats::default(),
                last_keyframe_us: 0,
                kf_interval_ms: 0.0,
                kf_interval_ok: false,
            })),
            kf: Arc::new(Mutex::new(KfSlot {
                cancel: None,
                generation: 0,
                closed: false,
                handles: Vec::new(),
            })),
            audio_check,
        }
    }

    fn current_relay(&self) -> Arc<dyn RelaySession> {
        self.relay.lock().unwrap().clone()
    }

    /// Points the sender at a reclaimed session (resume).
    ///
    /// The frameId space, the DecoderConfig and the shrunken chunk budget
    /// all carry over deliberately (continuity IS the resume signal; the
    /// learned MTU is a property of the uplink, not the session). The
    /// in-flight keyframe does not — it belongs to the session that died —
    /// and neither does the audio config cadence: the relay dropped its
    /// caches, video re-primes through its next keyframe, and audio has no
    /// keyframe, so the NEXT packet must carry the config.
    pub fn set_relay(&self, relay: Arc<dyn RelaySession>) {
        *self.relay.lock().unwrap() = relay;
        {
            let mut st = self.state.lock().unwrap();
            st.audio_config_sent = false;
        }
        let cancel = {
            let mut kf = self.kf.lock().unwrap();
            kf.generation += 1;
            kf.cancel.take()
        };
        if let Some(c) = cancel {
            let _ = c.send(KEYFRAME_SUPERSEDED_CODE);
        }
    }

    /// Sets the codec string once known. Derived from the BITSTREAM (the
    /// first SPS), never assumed — the encode crate owns that parsing
    /// (docs/38 D10); on the Annex-B path this string is the only thing
    /// telling the viewer's decoder what it is about to get. Extradata stays
    /// empty on purpose.
    pub fn set_codec(&self, codec: &str) {
        let mut st = self.state.lock().unwrap();
        if st.config_datagram.is_some() {
            return;
        }
        let mut dgram = Vec::new();
        if wire::append_decoder_config(&mut dgram, codec, b"").is_ok() {
            st.config_datagram = Some(dgram);
            st.st.codec = Some(codec.to_owned());
        }
    }

    /// Records what the relay says this fleet supports. The CapParityChunks
    /// FLAG gates the level: the flag is what says the relay filters parity
    /// per subscriber — without it, emitting would spray parity at viewers
    /// that cannot parse it.
    pub fn apply_capabilities(&self, c: wire::RelayCapabilities) {
        let level = if c.flags & wire::CAP_PARITY_CHUNKS != 0 {
            c.parity_level
        } else {
            0
        };
        let mut st = self.state.lock().unwrap();
        st.parity_level = level;
        st.st.parity_level = level;
    }

    /// Routes one access unit.
    pub async fn send_video(&self, au: AccessUnit) {
        let (frame_id, keyframe) = {
            let mut s = self.state.lock().unwrap();
            let id = s.next_frame_id;
            s.next_frame_id = s.next_frame_id.wrapping_add(1);
            s.st.encoded_frames += 1;
            if au.keyframe {
                s.st.keyframes += 1;
                if s.last_keyframe_us != 0 && au.timestamp_us > s.last_keyframe_us {
                    let gap = (au.timestamp_us - s.last_keyframe_us) as f64 / 1000.0;
                    if s.kf_interval_ok {
                        // EMA, α = 0.3: settles within a few GOPs, smooths
                        // the jitter damage-driven capture puts on gaps.
                        s.kf_interval_ms = 0.7 * s.kf_interval_ms + 0.3 * gap;
                    } else {
                        s.kf_interval_ms = gap;
                        s.kf_interval_ok = true;
                    }
                }
                s.last_keyframe_us = au.timestamp_us;
            }
            (id, au.keyframe)
        };
        if keyframe {
            self.send_keyframe(frame_id, au).await;
        } else {
            self.send_delta(frame_id, au);
        }
    }

    // Writes one StreamFrame on its own reliable uni stream. (The
    // forced-IDR-on-resume improvement lives at the session/encoder seam —
    // the sender has no frame to force, it only ships what arrives.)
    async fn send_keyframe(&self, frame_id: u32, au: AccessUnit) {
        let (mut msg, has_config) = {
            let s = self.state.lock().unwrap();
            let config = s.config_datagram.clone().unwrap_or_default();
            let mut msg =
                Vec::with_capacity(wire::STREAM_FRAME_HEADER_SIZE + config.len() + au.data.len());
            let header = wire::StreamFrameHeader {
                keyframe: true,
                frame_id,
                timestamp_us: au.timestamp_us,
                config_len: config.len() as u32,
                payload_len: au.data.len() as u32,
            };
            if wire::append_stream_frame_header(&mut msg, &header).is_err() {
                // Over MaxKeyframeBytes: the relay would reject it anyway.
                drop(s);
                self.count_keyframe_failed();
                return;
            }
            msg.extend_from_slice(&config);
            (msg, !config.is_empty())
        };
        msg.extend_from_slice(&au.data);
        if has_config {
            self.state.lock().unwrap().st.configs_sent += 1;
        }

        let relay = self.current_relay();
        let stream = match relay.open_keyframe_stream().await {
            Ok(s) => s,
            Err(_) => {
                self.count_keyframe_failed();
                return;
            }
        };

        // Supersede any keyframe still writing: newest wins, ≤1 in flight.
        let (cancel_rx, my_generation) = {
            let mut kf = self.kf.lock().unwrap();
            if kf.closed {
                // Teardown is waiting for in-flight writes before closing the
                // session; spawning another would write to a session about to
                // close.
                drop(kf);
                stream.abort(KEYFRAME_SUPERSEDED_CODE);
                self.count_keyframe_failed();
                return;
            }
            if let Some(old) = kf.cancel.take() {
                let _ = old.send(KEYFRAME_SUPERSEDED_CODE);
                self.state.lock().unwrap().st.keyframe_streams_superseded += 1;
            }
            let (tx, rx) = oneshot::channel();
            kf.cancel = Some(tx);
            kf.generation += 1;
            (rx, kf.generation)
        };

        // Write off the frame path: a stalled uplink must not block the next
        // frame's arrival from the encoder.
        let msg_len = msg.len() as u64;
        let outcome_fut = stream.write(msg, cancel_rx);
        let (state, kf_slot) = (self.state.clone(), self.kf.clone());
        let handle = tokio::spawn(async move {
            let outcome = outcome_fut.await;
            {
                let mut kf = kf_slot.lock().unwrap();
                if kf.generation == my_generation {
                    kf.cancel = None;
                }
            }
            let mut s = state.lock().unwrap();
            match outcome {
                KeyframeOutcome::Sent => {
                    s.st.keyframe_streams_sent += 1;
                    s.st.keyframe_bytes_sent += msg_len;
                    s.st.bytes_sent += msg_len;
                    s.st.sent_frames += 1;
                }
                // A superseded stream lands here too; mirroring the Go code,
                // any unfinished write counts as failed (the supersede was
                // already counted separately at supersede time).
                KeyframeOutcome::Cancelled | KeyframeOutcome::Failed(_) => {
                    s.st.keyframe_streams_failed += 1;
                }
            }
        });
        self.kf.lock().unwrap().handles.push(handle);
    }

    /// Chunks a non-keyframe AU into datagrams and applies the failure
    /// policy.
    fn send_delta(&self, frame_id: u32, au: AccessUnit) {
        let relay = self.current_relay();
        for _attempt in 0..2 {
            let (dgrams, parity) = match self.chunk_with_parity(frame_id, &au) {
                Ok(v) => v,
                Err(_) => {
                    self.count_frame_dropped();
                    return;
                }
            };
            let mut send_err = None;
            for d in &dgrams {
                match relay.send_datagram(d) {
                    Ok(()) => {
                        let mut s = self.state.lock().unwrap();
                        s.st.datagrams_sent += 1;
                        s.st.bytes_sent += d.len() as u64;
                    }
                    Err(e) => {
                        send_err = Some(e);
                        break;
                    }
                }
            }
            // R29: parity trails the data chunks. A parity send failure is
            // NOT a frame failure — the data is already out, and a viewer
            // without parity is exactly a pre-R29 viewer — so it must not
            // trigger the re-chunk or drop path.
            if send_err.is_none() {
                for d in &parity {
                    if relay.send_datagram(d).is_err() {
                        break;
                    }
                    let mut s = self.state.lock().unwrap();
                    s.st.parity_chunks_sent += 1;
                    s.st.parity_bytes_sent += d.len() as u64;
                }
                self.state.lock().unwrap().st.sent_frames += 1;
                return;
            }

            // A too-large datagram is not this frame's fault — the path MTU
            // is smaller than assumed. Shrink and re-chunk once; the new
            // size sticks for every later frame.
            if let Some(SendDatagramError::TooLarge {
                max_datagram_size: Some(max),
            }) = send_err
                && self
                    .state
                    .lock()
                    .unwrap()
                    .budget
                    .shrink_for_datagram_size(max)
            {
                continue;
            }

            // Any other failure mid-frame: the rest of this frame is dead
            // weight (a partial frame is discarded by the reassembler
            // anyway; pushing the rest spends uplink to deliver nothing).
            self.count_frame_dropped();
            return;
        }
        // Re-chunked and still failing: drop rather than loop.
        self.count_frame_dropped();
    }

    fn chunk_with_parity(
        &self,
        frame_id: u32,
        au: &AccessUnit,
    ) -> Result<(Datagrams, Datagrams), wire::WireError> {
        let (budget, level) = {
            let s = self.state.lock().unwrap();
            (s.budget, s.parity_level)
        };
        let payloads = wire::split_frame(&au.data, &budget)?;
        let count = payloads.len() as u16;
        let mut dgrams = Vec::with_capacity(payloads.len());
        for (i, p) in payloads.iter().enumerate() {
            let mut d = Vec::with_capacity(wire::VIDEO_CHUNK_HEADER_SIZE + p.len());
            wire::append_video_chunk(
                &mut d,
                &wire::VideoChunkHeader {
                    keyframe: false,
                    frame_id,
                    chunk_index: i as u16,
                    chunk_count: count,
                    timestamp_us: au.timestamp_us,
                },
                p,
            )?;
            dgrams.push(d);
        }
        // Parity covers chunk PAYLOADS (what the viewer reassembles), deltas
        // only. Past MaxParityDataChunks the frame degrades to plain
        // datagrams instead of erroring — a ~300 KB delta is not worth
        // failing a broadcast over.
        if level == 0 || payloads.len() > wire::MAX_PARITY_DATA_CHUNKS {
            return Ok((dgrams, Vec::new()));
        }
        let symbols = match wire::compute_parity(&payloads, level as usize) {
            Ok(s) => s,
            // Parity is an enhancement: a frame it cannot cover still ships.
            Err(_) => return Ok((dgrams, Vec::new())),
        };
        let mut parity = Vec::with_capacity(symbols.len());
        for (i, sym) in symbols.iter().enumerate() {
            let mut d = Vec::with_capacity(wire::PARITY_CHUNK_HEADER_SIZE + sym.len());
            wire::append_parity_chunk(
                &mut d,
                &wire::ParityChunkHeader {
                    frame_id,
                    parity_index: i as u8,
                    chunk_count: count,
                    frame_bytes: au.data.len() as u32,
                },
                sym,
            )?;
            parity.push(d);
        }
        Ok((dgrams, parity))
    }

    /// Prepares the AudioConfig this lane advertises. Called once, before
    /// the audio pump starts. Description stays empty, exactly as the
    /// browser lane sends it (an OpusHead is the only thing that could make
    /// a multistream layout decodable — the layout the capture format
    /// exists to prevent).
    pub fn set_audio_format(&self, f: AudioFormat) {
        let mut dgram = Vec::new();
        let ok = wire::append_audio_config(
            &mut dgram,
            &wire::AudioConfig {
                codec: &f.codec,
                sample_rate: f.sample_rate,
                channels: f.channels,
                description: b"",
            },
        )
        .is_ok();
        let mut s = self.state.lock().unwrap();
        if ok {
            s.audio_config_datagram = Some(dgram);
        }
        s.st.audio_codec = Some(f.codec.clone());
        s.st.audio_sample_rate = f.sample_rate;
        s.st.audio_channels = f.channels;
        s.st.audio_bitrate_bps = f.bitrate_bps;
        s.st.audio_source = Some(f.source.clone());
        s.audio_format = Some(f);
    }

    /// Routes one Opus packet. One datagram per packet, never chunked; the
    /// config piggybacks at 1 Hz on the packet flow (50 packets/s is already
    /// a scheduler); a failure here touches NO video counter.
    pub fn send_audio(&self, p: AudioPacket) {
        // First packet: verify the bitstream against the advertised config
        // (R25 Decision 10) — never ship a config that lies about the stream.
        {
            let mut s = self.state.lock().unwrap();
            if s.audio_config_datagram.is_none() {
                return; // no format: nothing on the wire could describe this
            }
            if !s.audio_checked {
                s.audio_checked = true;
                if let (Some(check), Some(format)) = (&self.audio_check, &s.audio_format)
                    && check(&p.data, format).is_err()
                {
                    s.st.audio_errored = true;
                }
            }
            if s.st.audio_errored {
                return;
            }
            if p.data.is_empty() || p.data.len() > wire::MAX_AUDIO_PAYLOAD {
                s.st.audio_packets_dropped += 1;
                return;
            }
        }

        let relay = self.current_relay();

        // The config rides the flow: on the first packet, then at most once
        // per AUDIO_CONFIG_RESEND_MS.
        let now_ms = self.clock.now_us() / 1000;
        let config_due = {
            let s = self.state.lock().unwrap();
            !s.audio_config_sent || now_ms - s.audio_config_sent_at_ms >= AUDIO_CONFIG_RESEND_MS
        };
        if config_due {
            let dgram = self.state.lock().unwrap().audio_config_datagram.clone();
            if let Some(dgram) = dgram
                && relay.send_datagram(&dgram).is_ok()
            {
                let mut s = self.state.lock().unwrap();
                s.audio_config_sent_at_ms = now_ms;
                s.audio_config_sent = true;
                s.st.audio_configs_sent += 1;
            }
        }

        // The sequence advances whether or not the send succeeds: a viewer
        // seeing a gap is seeing the truth; reusing the number would hide a
        // lost packet behind a duplicate.
        let seq = {
            let mut s = self.state.lock().unwrap();
            let seq = s.next_audio_seq;
            s.next_audio_seq = s.next_audio_seq.wrapping_add(1);
            seq
        };
        let mut dgram = Vec::with_capacity(wire::AUDIO_FRAME_HEADER_SIZE + p.data.len());
        if wire::append_audio_frame(
            &mut dgram,
            &wire::AudioFrameHeader {
                seq,
                timestamp_us: p.timestamp_us,
            },
            &p.data,
        )
        .is_err()
        {
            self.state.lock().unwrap().st.audio_packets_dropped += 1;
            return;
        }
        if relay.send_datagram(&dgram).is_err() {
            self.state.lock().unwrap().st.audio_packets_dropped += 1;
            return;
        }
        let mut s = self.state.lock().unwrap();
        s.st.audio_packets_sent += 1;
        s.st.audio_bytes_sent += dgram.len() as u64;
        s.st.bytes_sent += dgram.len() as u64;
    }

    /// A snapshot of the counters.
    pub fn stats(&self) -> Stats {
        let s = self.state.lock().unwrap();
        let mut st = s.st.clone();
        st.keyframe_interval_ms = s.kf_interval_ok.then_some(s.kf_interval_ms);
        st
    }

    /// Closes the sender to new keyframe writes, then waits for the
    /// in-flight ones — so teardown does not race a write onto a session it
    /// is about to close.
    pub async fn wait(&self) {
        let handles = {
            let mut kf = self.kf.lock().unwrap();
            kf.closed = true;
            std::mem::take(&mut kf.handles)
        };
        for h in handles {
            let _ = h.await;
        }
    }

    fn count_frame_dropped(&self) {
        self.state.lock().unwrap().st.frames_dropped_at_send += 1;
    }

    fn count_keyframe_failed(&self) {
        self.state.lock().unwrap().st.keyframe_streams_failed += 1;
    }
}
