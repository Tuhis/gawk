//! Datagrams → complete encoded frames and demuxed side messages (docs/67
//! D13, D14). A port of the SPA's `transport/reassembler.ts`, policy for
//! policy, so a native viewer drops exactly what a web viewer drops:
//!
//! - A frame is emitted only when every chunk is in (or parity rebuilt the
//!   missing ones); nothing waits for a retransmit.
//! - At most [`MAX_ASSEMBLIES`] frames assemble at once; starting one more
//!   evicts the oldest (it lost a datagram).
//! - A completed delta at or behind the last emitted frame is dropped as
//!   late, in serial arithmetic ([`gawk_wire::frame_id_ahead`]): frame IDs
//!   wrap at 2^32.
//! - Keyframes reset that watermark. Real keyframes ride reliable streams
//!   and never come through here, so the session reports them with
//!   [`Reassembler::note_stream_keyframe`]; that reset is what lets a
//!   broadcaster restart (frame IDs back to 0) recover.
//! - Repeated DecoderConfig and AudioConfig datagrams are deduplicated by
//!   byte equality.
//!
//! Not ported: the per-frame arrival accounting and the recovered-frame
//! ledger, whose only consumer is R30's stripe detector (no striping in v1,
//! D13), and the delta evidence behind the SPA's one-loss-per-GOP allowance,
//! which D13's stricter rule does not use.

use gawk_wire::{
    TYPE_AUDIO_CONFIG, TYPE_AUDIO_FRAME, TYPE_CLOCK_MAPPING, TYPE_DECODER_CONFIG,
    TYPE_PARITY_CHUNK, TYPE_VIDEO_CHUNK, TYPE_VIEWER_COUNT, VERSION, frame_id_ahead,
};

/// The SPA's `MAX_ASSEMBLIES`.
pub const MAX_ASSEMBLIES: usize = 8;

/// One complete encoded video frame.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AssembledFrame {
    pub frame_id: u32,
    pub keyframe: bool,
    /// The broadcaster's capture clock, µs.
    pub timestamp_us: u64,
    pub data: Vec<u8>,
}

/// A decoder configuration (DecoderConfig, 0x02), owned.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VideoConfig {
    pub codec: String,
    pub extradata: Vec<u8>,
}

/// One Opus packet (AudioFrame, 0x07): a datagram is a packet, no chunking.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AudioPacket {
    pub seq: u32,
    pub timestamp_us: u64,
    pub payload: Vec<u8>,
}

/// The audio lane's configuration (AudioConfig, 0x08), owned.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AudioSettings {
    pub codec: String,
    pub sample_rate: u32,
    pub channels: u8,
    pub description: Vec<u8>,
}

/// What one datagram produced.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Demuxed {
    /// Only when the bytes differ from the previous config.
    Config(VideoConfig),
    Frame(AssembledFrame),
    /// `relay_clock_us = timestamp_us + offset_us`; last one wins.
    ClockMapping(i64),
    /// The relay's "N watching"; last one wins.
    ViewerCount(u32),
    AudioFrame(AudioPacket),
    /// Only when the bytes differ from the previous audio config.
    AudioConfig(AudioSettings),
}

/// The SPA's `ReassemblerStats`, less the stripe-only counters.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct ReassemblerStats {
    pub datagrams_received: u64,
    pub bad_datagrams: u64,
    pub duplicate_chunks: u64,
    pub duplicate_configs: u64,
    pub frames_completed: u64,
    pub frames_dropped_incomplete: u64,
    pub frames_dropped_late: u64,
    pub audio_packets_received: u64,
    pub audio_bytes_received: u64,
    pub parity_chunks_received: u64,
    /// A frame that would have been dropped incomplete and decoded instead.
    pub frames_recovered_by_parity: u64,
    /// The GF solve failed (a header that can't describe the block).
    pub parity_recovery_failures: u64,
    /// Data chunks for a frame at or behind the watermark, dropped without
    /// building an assembly.
    pub stale_chunks: u64,
    /// Frames given up on that held parity which couldn't cover the loss.
    pub parity_insufficient: u64,
}

struct Assembly {
    frame_id: u32,
    keyframe: bool,
    timestamp_us: u64,
    chunk_count: u16,
    payloads: Vec<Option<Vec<u8>>>,
    /// The completeness cursor; a recovery bumps it to `chunk_count`.
    received: usize,
    /// Real chunk arrivals only.
    arrived: usize,
    /// Parity symbols by index (P, Q), allocated only when one arrives.
    parity: Option<[Option<Vec<u8>>; gawk_wire::MAX_PARITY_SYMBOLS]>,
    parity_held: usize,
    frame_bytes: usize,
}

impl Assembly {
    fn new(frame_id: u32, keyframe: bool, timestamp_us: u64, chunk_count: u16) -> Self {
        Self {
            frame_id,
            keyframe,
            timestamp_us,
            chunk_count,
            payloads: vec![None; chunk_count as usize],
            received: 0,
            arrived: 0,
            parity: None,
            parity_held: 0,
            frame_bytes: 0,
        }
    }
}

/// See the module docs.
#[derive(Default)]
pub struct Reassembler {
    /// Insertion order is arrival order; at most [`MAX_ASSEMBLIES`] long, so
    /// a linear scan beats a map.
    assemblies: Vec<Assembly>,
    last_config: Option<Vec<u8>>,
    last_audio_config: Option<Vec<u8>>,
    last_emitted: Option<u32>,
    stats: ReassemblerStats,
}

impl Reassembler {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn stats(&self) -> ReassemblerStats {
        self.stats
    }

    /// A keyframe arrived on a stream: move the late-delta watermark to it,
    /// unconditionally. A backwards jump is exactly the broadcaster-restart
    /// signal; mid-session it's a no-op, because keyframe IDs follow the
    /// delta sequence.
    pub fn note_stream_keyframe(&mut self, frame_id: u32) {
        self.last_emitted = Some(frame_id);
    }

    /// Feeds one datagram and appends whatever it produced to `out`.
    pub fn push(&mut self, dgram: &[u8], out: &mut Vec<Demuxed>) {
        self.stats.datagrams_received += 1;
        let Ok((version, kind)) = gawk_wire::peek_type(dgram) else {
            self.stats.bad_datagrams += 1;
            return;
        };
        if version != VERSION {
            self.stats.bad_datagrams += 1;
            return;
        }
        match kind {
            TYPE_DECODER_CONFIG => self.push_config(dgram, out),
            TYPE_VIDEO_CHUNK => self.push_chunk(dgram, out),
            TYPE_PARITY_CHUNK => self.push_parity(dgram, out),
            TYPE_CLOCK_MAPPING => match gawk_wire::parse_clock_mapping(dgram) {
                Ok(offset) => out.push(Demuxed::ClockMapping(offset)),
                Err(_) => self.stats.bad_datagrams += 1,
            },
            TYPE_VIEWER_COUNT => match gawk_wire::parse_viewer_count(dgram) {
                Ok(count) => out.push(Demuxed::ViewerCount(count)),
                Err(_) => self.stats.bad_datagrams += 1,
            },
            TYPE_AUDIO_FRAME => self.push_audio_frame(dgram, out),
            TYPE_AUDIO_CONFIG => self.push_audio_config(dgram, out),
            _ => self.stats.bad_datagrams += 1,
        }
    }

    fn push_audio_frame(&mut self, dgram: &[u8], out: &mut Vec<Demuxed>) {
        let Ok((h, payload)) = gawk_wire::parse_audio_frame(dgram) else {
            self.stats.bad_datagrams += 1;
            return;
        };
        self.stats.audio_packets_received += 1;
        self.stats.audio_bytes_received += dgram.len() as u64;
        out.push(Demuxed::AudioFrame(AudioPacket {
            seq: h.seq,
            timestamp_us: h.timestamp_us,
            payload: payload.to_vec(),
        }));
    }

    fn push_audio_config(&mut self, dgram: &[u8], out: &mut Vec<Demuxed>) {
        let Ok(c) = gawk_wire::parse_audio_config(dgram) else {
            self.stats.bad_datagrams += 1;
            return;
        };
        // The broadcaster re-sends this at 1 Hz.
        if self.last_audio_config.as_deref() == Some(dgram) {
            self.stats.duplicate_configs += 1;
            return;
        }
        self.last_audio_config = Some(dgram.to_vec());
        out.push(Demuxed::AudioConfig(AudioSettings {
            codec: c.codec.to_owned(),
            sample_rate: c.sample_rate,
            channels: c.channels,
            description: c.description.to_vec(),
        }));
    }

    fn push_config(&mut self, dgram: &[u8], out: &mut Vec<Demuxed>) {
        let Ok(c) = gawk_wire::parse_decoder_config(dgram) else {
            self.stats.bad_datagrams += 1;
            return;
        };
        if self.last_config.as_deref() == Some(dgram) {
            self.stats.duplicate_configs += 1;
            return;
        }
        self.last_config = Some(dgram.to_vec());
        out.push(Demuxed::Config(VideoConfig {
            codec: c.codec.to_owned(),
            extradata: c.extradata.to_vec(),
        }));
    }

    fn behind_watermark(&self, frame_id: u32) -> bool {
        self.last_emitted
            .is_some_and(|w| !frame_id_ahead(frame_id, w))
    }

    fn find(&self, frame_id: u32) -> Option<usize> {
        self.assemblies.iter().position(|a| a.frame_id == frame_id)
    }

    fn push_chunk(&mut self, dgram: &[u8], out: &mut Vec<Demuxed>) {
        let Ok((h, payload)) = gawk_wire::parse_video_chunk(dgram) else {
            self.stats.bad_datagrams += 1;
            return;
        };
        let idx = match self.find(h.frame_id) {
            Some(i) => i,
            None => {
                // A chunk of an already-emitted frame must not build a
                // phantom assembly. Keyframes bypass: a datagram keyframe
                // resets the watermark by design.
                if !h.keyframe && self.behind_watermark(h.frame_id) {
                    self.stats.stale_chunks += 1;
                    return;
                }
                self.evict_if_full();
                self.assemblies.push(Assembly::new(
                    h.frame_id,
                    h.keyframe,
                    h.timestamp_us,
                    h.chunk_count,
                ));
                self.assemblies.len() - 1
            }
        };
        let a = &mut self.assemblies[idx];
        if h.chunk_count != a.chunk_count {
            // Chunks of one frame disagree on the count: corrupt.
            self.stats.bad_datagrams += 1;
            return;
        }
        if a.arrived == 0 {
            // Opened by a parity symbol, which carries no timestamp.
            a.timestamp_us = h.timestamp_us;
            a.keyframe = h.keyframe;
        }
        let slot = &mut a.payloads[h.chunk_index as usize];
        if slot.is_some() {
            self.stats.duplicate_chunks += 1;
            return;
        }
        *slot = Some(payload.to_vec());
        a.received += 1;
        a.arrived += 1;
        if a.received == a.chunk_count as usize {
            let a = self.assemblies.remove(idx);
            self.complete(a, out);
            return;
        }
        self.try_recover(idx, out);
    }

    /// A parity symbol. It creates the assembly when its frame is unknown
    /// and not yet emitted: under reorder, parity can outrun the chunk that
    /// would have created it.
    fn push_parity(&mut self, dgram: &[u8], out: &mut Vec<Demuxed>) {
        let Ok((h, payload)) = gawk_wire::parse_parity_chunk(dgram) else {
            self.stats.bad_datagrams += 1;
            return;
        };
        self.stats.parity_chunks_received += 1;
        let idx = match self.find(h.frame_id) {
            Some(i) => i,
            None => {
                // On a clean link every frame completes before its parity
                // lands; an assembly here could never complete and would
                // later evict as a phantom incomplete.
                if self.behind_watermark(h.frame_id) {
                    return;
                }
                self.evict_if_full();
                // Parity is delta-only (keyframes ride streams); the first
                // real chunk fills in the timestamp.
                self.assemblies
                    .push(Assembly::new(h.frame_id, false, 0, h.chunk_count));
                self.assemblies.len() - 1
            }
        };
        let a = &mut self.assemblies[idx];
        if h.chunk_count != a.chunk_count {
            self.stats.bad_datagrams += 1;
            return;
        }
        let symbols = a.parity.get_or_insert_with(Default::default);
        let Some(slot) = symbols.get_mut(h.parity_index as usize) else {
            self.stats.bad_datagrams += 1;
            return;
        };
        if slot.is_some() {
            self.stats.duplicate_chunks += 1;
            return;
        }
        *slot = Some(payload.to_vec());
        a.parity_held += 1;
        a.frame_bytes = h.frame_bytes as usize;
        self.try_recover(idx, out);
    }

    /// Recovers as soon as the erasures are within the symbols held: eager,
    /// because waiting adds latency to exactly the frames parity saves.
    fn try_recover(&mut self, idx: usize, out: &mut Vec<Demuxed>) {
        let a = &mut self.assemblies[idx];
        let Some(symbols) = &a.parity else { return };
        let missing = a.chunk_count as usize - a.received;
        if a.parity_held == 0 || missing == 0 || missing > a.parity_held {
            return;
        }
        // Parity alone carries no timestamp; the decoder can't be fed.
        if a.received == 0 {
            return;
        }
        let refs: Vec<Option<&[u8]>> = symbols.iter().map(|s| s.as_deref()).collect();
        if gawk_wire::recover_chunks(&mut a.payloads, &refs, a.frame_bytes).is_err() {
            // Routine on a bad link; the frame stays held so a straggler can
            // still complete it.
            self.stats.parity_recovery_failures += 1;
            return;
        }
        if a.payloads.iter().any(Option::is_none) {
            return;
        }
        a.received = a.chunk_count as usize;
        let a = self.assemblies.remove(idx);
        self.stats.frames_recovered_by_parity += 1;
        self.complete(a, out);
    }

    fn complete(&mut self, a: Assembly, out: &mut Vec<Demuxed>) {
        // A late delta's reference was already superseded; keyframes are
        // self-contained and always emitted.
        if !a.keyframe && self.behind_watermark(a.frame_id) {
            self.stats.frames_dropped_late += 1;
            return;
        }
        let len = a.payloads.iter().flatten().map(Vec::len).sum();
        let mut data = Vec::with_capacity(len);
        for p in a.payloads.into_iter().flatten() {
            data.extend_from_slice(&p);
        }
        self.last_emitted = Some(a.frame_id);
        self.stats.frames_completed += 1;
        out.push(Demuxed::Frame(AssembledFrame {
            frame_id: a.frame_id,
            keyframe: a.keyframe,
            timestamp_us: a.timestamp_us,
            data,
        }));
    }

    fn evict_if_full(&mut self) {
        if self.assemblies.len() < MAX_ASSEMBLIES {
            return;
        }
        let a = self.assemblies.remove(0);
        // Where a frame is given up on is the one place a parity shortfall
        // is attributable exactly once: more erasures than symbols, or every
        // data chunk lost (no timestamp to decode a reconstruction with).
        if a.parity_held > 0 {
            let missing = a.chunk_count as usize - a.received;
            if missing > a.parity_held || a.received == 0 {
                self.stats.parity_insufficient += 1;
            }
        }
        self.stats.frames_dropped_incomplete += 1;
    }
}

#[cfg(test)]
mod tests;
