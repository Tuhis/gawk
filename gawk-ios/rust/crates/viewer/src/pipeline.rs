//! One subscribe session's receive path, bytes in and timed media out
//! (docs/67 D13, D15): the composition `viewer.ts` does in the SPA, without
//! a decoder or a network.
//!
//! Datagrams go through TimeSync, then the [`Reassembler`]; keyframe
//! streams skip it (and move its late-delta watermark). Both meet in the
//! [`ReorderBuffer`], which releases in decode order on the playout
//! schedule. Every released frame and every audio packet leaves with a
//! presentation time on the caller's clock, `ts + baseline + offset`, where
//! `baseline` is the windowed-min arrival delta and `offset` the adaptive
//! playout offset (D15). Video is the master clock and audio is scheduled
//! on the same anchor, so the two renderers under one
//! `AVSampleBufferRenderSynchronizer` stay in sync by construction. A slewed
//! offset moves those times gradually, which is the "rate a fraction off
//! 1.0" of D15 done in the timestamps instead of the synchronizer.
//!
//! Pure and clock-free: the session passes `now_ms` (a monotonic clock, the
//! same one presentation times are on) into every call.

use crate::jitter::ArrivalJitter;
use crate::playout::{
    DECODE_LEAD_MS, MIN_PLAYOUT_OFFSET_MS, PLAYOUT_UPDATE_INTERVAL_MS, PlayoutController,
    PlayoutPreset,
};
use crate::reassembly::{AudioPacket, AudioSettings, Demuxed, Reassembler, VideoConfig};
use crate::reorder::{GRACE_ENVELOPE, Pacing, ReorderBuffer, Reordered, StreamKeyframe};
use crate::timesync::TimeSync;
use gawk_wire::{
    MAX_KEYFRAME_BYTES, STREAM_FRAME_HEADER_SIZE, TYPE_DELIVERY_ACK, TYPE_RELAY_CAPABILITIES,
    TYPE_SESSION_CLOSING, TYPE_STREAM_FRAME, TYPE_TELEMETRY_ENDPOINT, TYPE_TELEMETRY_HELLO,
};

/// The SPA's `KEYFRAME_STALL_MS`: keyframes stopped while deltas flow.
pub const KEYFRAME_STALL_MS: f64 = 8000.0;
/// The SPA's `FRAMES_FLOWING_WINDOW_MS`: what tells a wedged stream path
/// from a broadcaster who stepped away.
pub const FRAMES_FLOWING_WINDOW_MS: f64 = 1000.0;
/// The SPA's `SESSION_STALL_MS`: three missed ViewerCount keepalives.
pub const SESSION_STALL_MS: f64 = 15_000.0;
/// The SPA's `MEDIA_STALL_MS`.
pub const MEDIA_STALL_MS: f64 = 6000.0;
/// The SPA's `MAPPINGS_PROVING_LIVE`: ClockMappings since the last media
/// that prove the broadcaster is still capturing.
pub const MAPPINGS_PROVING_LIVE: u32 = 2;
/// D15's drop-to-live: the newest frame received may run at most this many
/// offsets ahead of what's on screen before the renderers are flushed and
/// playback jumps to the next keyframe.
pub const DROP_TO_LIVE_OFFSETS: f64 = 2.0;

/// What the session hands the renderers.
#[derive(Debug, Clone, PartialEq)]
pub enum ViewerEvent {
    /// The decoder configuration changed; the next frame is a keyframe.
    VideoConfig(VideoConfig),
    /// A frame in decode order, to present at `present_at_ms`.
    VideoFrame {
        frame_id: u32,
        keyframe: bool,
        timestamp_us: u64,
        data: Vec<u8>,
        present_at_ms: f64,
    },
    AudioConfig(AudioSettings),
    Audio {
        packet: AudioPacket,
        present_at_ms: f64,
    },
    /// Drop everything queued in both renderers: a broadcaster restart (a
    /// new timeline) or a drop to live (D15).
    Flush,
    ViewerCount(u32),
    /// The relay's telemetry identity for this session (TelemetryHello,
    /// 0x0D). The token is never logged or put in stats.
    TelemetryHello {
        enabled: bool,
        report_interval_ms: u16,
        token: Vec<u8>,
        broadcast_key: Vec<u8>,
    },
    TelemetryEndpoint(String),
    /// The session's diagnostics, every 500 ms.
    Stats(PipelineStats),
}

/// Why a watchdog gave up on a session that never said it ended. Every one
/// reconnects.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Stall {
    /// Nothing at all for [`SESSION_STALL_MS`].
    Session,
    /// Deltas flowing, no keyframe for [`KEYFRAME_STALL_MS`].
    Keyframe,
    /// No video or audio for [`MEDIA_STALL_MS`] while ClockMappings prove
    /// the broadcaster is capturing (armed only once audio was seen).
    Media,
}

/// Diagnostics for the stats sheet (G3, G8).
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct PipelineStats {
    pub reassembly: crate::reassembly::ReassemblerStats,
    pub reorder: crate::reorder::ReorderStats,
    pub offset_ms: f64,
    pub grace_ms: f64,
    pub jitter_ms: Option<f64>,
    pub time_sync_rtt_ms: Option<f64>,
    pub viewer_count: Option<u32>,
    pub drops_to_live: u64,
    pub restarts: u64,
}

/// See the module docs.
pub struct Pipeline {
    preset: PlayoutPreset,
    reassembler: Reassembler,
    reorder: ReorderBuffer,
    jitter: ArrivalJitter,
    playout: PlayoutController,
    grace: PlayoutController,
    time_sync: TimeSync,
    last_update_ms: Option<f64>,
    current_config: Option<VideoConfig>,
    /// `relay_us = frame_ts_us + clock_mapping_us` (ClockMapping, 0x06).
    clock_mapping_us: Option<i64>,
    /// The close code a SessionClosing (0x17) named, for a close that
    /// arrives without one.
    session_closing: Option<u32>,
    newest_frame_ts_us: Option<u64>,
    presented_ts_us: Option<u64>,
    last_inbound_ms: Option<f64>,
    last_frame_ms: Option<f64>,
    last_keyframe_ms: Option<f64>,
    last_media_ms: Option<f64>,
    mappings_since_media: u32,
    audio_seen: bool,
    viewer_count: Option<u32>,
    drops_to_live: u64,
    restarts: u64,
    demuxed: Vec<Demuxed>,
    reordered: Vec<Reordered>,
}

impl Pipeline {
    pub fn new(preset: PlayoutPreset) -> Self {
        Self {
            preset,
            reassembler: Reassembler::new(),
            reorder: ReorderBuffer::new(),
            jitter: ArrivalJitter::new(),
            playout: PlayoutController::new(),
            grace: PlayoutController::with_envelope(GRACE_ENVELOPE),
            time_sync: TimeSync::new(),
            last_update_ms: None,
            current_config: None,
            clock_mapping_us: None,
            session_closing: None,
            newest_frame_ts_us: None,
            presented_ts_us: None,
            last_inbound_ms: None,
            last_frame_ms: None,
            last_keyframe_ms: None,
            last_media_ms: None,
            mappings_since_media: 0,
            audio_seen: false,
            viewer_count: None,
            drops_to_live: 0,
            restarts: 0,
            demuxed: Vec::new(),
            reordered: Vec::new(),
        }
    }

    pub fn set_preset(&mut self, preset: PlayoutPreset) {
        self.preset = preset;
    }

    /// The playout offset in force (D15 presets).
    pub fn offset_ms(&self) -> f64 {
        self.preset.offset_ms(&self.playout)
    }

    fn pacing(&mut self, now_ms: f64) -> Pacing {
        let (offset_ms, decode_lead_ms) = match self.preset {
            PlayoutPreset::Balanced => (self.playout.offset_ms(), DECODE_LEAD_MS),
            // Lowest latency: video on arrival (DisplayImmediately), audio at
            // the minimum offset.
            PlayoutPreset::LowestLatency => (0.0, 0.0),
        };
        Pacing {
            offset_ms,
            decode_lead_ms,
            grace_ms: self.grace.offset_ms(),
            baseline_ms: self.jitter.baseline_ms(now_ms),
        }
    }

    /// A TimeSync ping when one is due; the session sends it as a datagram.
    pub fn time_sync_ping(&mut self, now_ms: f64) -> Option<Vec<u8>> {
        self.time_sync.ping_due(now_ms)
    }

    /// The close code a SessionClosing named, if one arrived.
    pub fn session_closing(&self) -> Option<u32> {
        self.session_closing
    }

    /// One received datagram.
    pub fn on_datagram(&mut self, dgram: &[u8], now_ms: f64, out: &mut Vec<ViewerEvent>) {
        // Liveness first, for every kind: the count keepalive is all an away
        // broadcaster's viewer receives.
        self.last_inbound_ms = Some(now_ms);
        if self.time_sync.handle(dgram, (now_ms * 1000.0) as u64) {
            return;
        }
        // The relay's join-time statement of the served mode: v1 always asks
        // for datagrams, so it carries nothing to act on.
        if dgram.len() >= 2 && dgram[1] == TYPE_DELIVERY_ACK {
            return;
        }
        let mut demuxed = std::mem::take(&mut self.demuxed);
        self.reassembler.push(dgram, &mut demuxed);
        for d in demuxed.drain(..) {
            self.on_demuxed(d, now_ms, out);
        }
        self.demuxed = demuxed;
    }

    fn on_demuxed(&mut self, d: Demuxed, now_ms: f64, out: &mut Vec<ViewerEvent>) {
        match d {
            // The legacy datagram path: keyframes carry their config now.
            Demuxed::Config(c) => self.apply_config(c, out),
            Demuxed::Frame(f) => {
                self.note_frame(f.timestamp_us, f.keyframe, now_ms);
                let pacing = self.pacing(now_ms);
                let mut reordered = std::mem::take(&mut self.reordered);
                if f.keyframe {
                    let kf = StreamKeyframe {
                        frame_id: f.frame_id,
                        timestamp_us: f.timestamp_us,
                        config: None,
                        data: f.data,
                    };
                    self.reorder
                        .push_keyframe(kf, now_ms, &pacing, &mut reordered);
                } else {
                    self.reorder.push_delta(
                        f.frame_id,
                        f.timestamp_us,
                        f.data,
                        now_ms,
                        &pacing,
                        &mut reordered,
                    );
                }
                self.drain_reordered(&mut reordered, now_ms, out);
                self.reordered = reordered;
            }
            Demuxed::ClockMapping(offset) => {
                self.clock_mapping_us = Some(offset);
                self.mappings_since_media += 1;
            }
            Demuxed::ViewerCount(n) => {
                self.viewer_count = Some(n);
                out.push(ViewerEvent::ViewerCount(n));
            }
            Demuxed::AudioConfig(c) => out.push(ViewerEvent::AudioConfig(c)),
            Demuxed::AudioFrame(packet) => {
                self.audio_seen = true;
                self.note_media(now_ms);
                let present_at_ms = self.present_at(packet.timestamp_us, now_ms);
                out.push(ViewerEvent::Audio {
                    packet,
                    present_at_ms,
                });
            }
        }
    }

    /// The whole payload of one server-opened uni stream, dispatched on its
    /// `version‖type` prologue. A malformed or unknown stream is dropped.
    pub fn on_stream(&mut self, msg: &[u8], now_ms: f64, out: &mut Vec<ViewerEvent>) {
        self.last_inbound_ms = Some(now_ms);
        let Ok((_, kind)) = gawk_wire::peek_type(msg) else {
            return;
        };
        match kind {
            TYPE_STREAM_FRAME => self.on_keyframe_stream(msg, now_ms, out),
            TYPE_TELEMETRY_HELLO => {
                if let Ok(h) = gawk_wire::parse_telemetry_hello(msg) {
                    out.push(ViewerEvent::TelemetryHello {
                        enabled: h.enabled,
                        report_interval_ms: h.report_interval_ms,
                        token: h.token.to_vec(),
                        broadcast_key: h.broadcast_key.to_vec(),
                    });
                }
            }
            TYPE_TELEMETRY_ENDPOINT => {
                if let Ok(url) = gawk_wire::parse_telemetry_endpoint(msg) {
                    out.push(ViewerEvent::TelemetryEndpoint(url.to_owned()));
                }
            }
            TYPE_SESSION_CLOSING => {
                if let Ok(code) = gawk_wire::parse_session_closing(msg) {
                    self.session_closing = Some(code);
                }
            }
            // Parity needs no negotiation on the viewer (the fleet default
            // applies), and striping is not in v1 (D13).
            TYPE_RELAY_CAPABILITIES => {}
            _ => {}
        }
    }

    fn on_keyframe_stream(&mut self, msg: &[u8], now_ms: f64, out: &mut Vec<ViewerEvent>) {
        if msg.len() > MAX_KEYFRAME_BYTES {
            return;
        }
        let Ok(h) = gawk_wire::parse_stream_frame_header(msg) else {
            return;
        };
        let config_end = STREAM_FRAME_HEADER_SIZE + h.config_len as usize;
        let end = config_end + h.payload_len as usize;
        if msg.len() != end {
            return;
        }
        let config = if h.config_len == 0 {
            None
        } else {
            match gawk_wire::parse_decoder_config(&msg[STREAM_FRAME_HEADER_SIZE..config_end]) {
                Ok(c) => Some(VideoConfig {
                    codec: c.codec.to_owned(),
                    extradata: c.extradata.to_vec(),
                }),
                Err(_) => return,
            }
        };
        // Keyframes bypass the reassembler, so move its watermark here: what
        // makes a broadcaster restart (IDs back to 0) recover.
        self.reassembler.note_stream_keyframe(h.frame_id);
        self.note_frame(h.timestamp_us, true, now_ms);
        let pacing = self.pacing(now_ms);
        let mut reordered = std::mem::take(&mut self.reordered);
        let kf = StreamKeyframe {
            frame_id: h.frame_id,
            timestamp_us: h.timestamp_us,
            config,
            data: msg[config_end..end].to_vec(),
        };
        self.reorder
            .push_keyframe(kf, now_ms, &pacing, &mut reordered);
        self.drain_reordered(&mut reordered, now_ms, out);
        self.reordered = reordered;
    }

    fn note_frame(&mut self, ts_us: u64, keyframe: bool, now_ms: f64) {
        self.jitter.record_arrival(ts_us, now_ms);
        self.last_frame_ms = Some(now_ms);
        if keyframe {
            self.last_keyframe_ms = Some(now_ms);
        }
        if self.newest_frame_ts_us.is_none_or(|n| ts_us > n) {
            self.newest_frame_ts_us = Some(ts_us);
        }
        self.note_media(now_ms);
    }

    fn note_media(&mut self, now_ms: f64) {
        self.last_media_ms = Some(now_ms);
        self.mappings_since_media = 0;
    }

    /// The renderer reports what it last put on screen; D15's drop-to-live
    /// rule reads it.
    pub fn note_presented(&mut self, timestamp_us: u64) {
        self.presented_ts_us = Some(timestamp_us);
    }

    /// The renderer's queue is too deep: stop feeding it and resync at the
    /// next keyframe (the SPA's decoder-backpressure resync).
    pub fn request_resync(&mut self, now_ms: f64, out: &mut Vec<ViewerEvent>) {
        let pacing = self.pacing(now_ms);
        let mut reordered = std::mem::take(&mut self.reordered);
        self.reorder.request_resync(now_ms, &pacing, &mut reordered);
        out.push(ViewerEvent::Flush);
        self.drain_reordered(&mut reordered, now_ms, out);
        self.reordered = reordered;
    }

    /// The periodic drive (the SPA's 16 ms reorder tick): the estimators on
    /// their 500 ms cadence, D15's drop-to-live check, then the reorder
    /// buffer's waits.
    pub fn tick(&mut self, now_ms: f64, out: &mut Vec<ViewerEvent>) {
        if self
            .last_update_ms
            .is_none_or(|t| now_ms - t >= PLAYOUT_UPDATE_INTERVAL_MS)
        {
            self.last_update_ms = Some(now_ms);
            let jitter = self.jitter.jitter_ms(now_ms);
            self.playout.update(jitter, now_ms);
            self.grace.update(jitter, now_ms);
        }
        if self.behind_live() {
            self.drops_to_live += 1;
            self.presented_ts_us = None;
            self.request_resync(now_ms, out);
            return;
        }
        let pacing = self.pacing(now_ms);
        let mut reordered = std::mem::take(&mut self.reordered);
        self.reorder.tick(now_ms, &pacing, &mut reordered);
        self.drain_reordered(&mut reordered, now_ms, out);
        self.reordered = reordered;
    }

    /// D15: the newest frame received runs more than
    /// [`DROP_TO_LIVE_OFFSETS`] × offset ahead of what's presented. Never
    /// while already frozen for a keyframe, when the gap is expected.
    fn behind_live(&self) -> bool {
        let (Some(newest), Some(shown)) = (self.newest_frame_ts_us, self.presented_ts_us) else {
            return false;
        };
        if self.reorder.waiting_for_keyframe() || newest <= shown {
            return false;
        }
        let ahead_ms = (newest - shown) as f64 / 1000.0;
        ahead_ms > DROP_TO_LIVE_OFFSETS * self.offset_ms().max(MIN_PLAYOUT_OFFSET_MS)
    }

    fn drain_reordered(
        &mut self,
        reordered: &mut Vec<Reordered>,
        now_ms: f64,
        out: &mut Vec<ViewerEvent>,
    ) {
        for r in reordered.drain(..) {
            match r {
                Reordered::Restart => self.on_restart(out),
                Reordered::Frame(f) => {
                    if let Some(c) = f.config {
                        self.apply_config(c, out);
                    }
                    let present_at_ms = match self.preset {
                        PlayoutPreset::LowestLatency => now_ms,
                        PlayoutPreset::Balanced => self.present_at(f.timestamp_us, now_ms),
                    };
                    out.push(ViewerEvent::VideoFrame {
                        frame_id: f.frame_id,
                        keyframe: f.keyframe,
                        timestamp_us: f.timestamp_us,
                        data: f.data,
                        present_at_ms,
                    });
                }
            }
        }
    }

    /// `ts + baseline + offset` on the caller's clock; before any video
    /// anchors the baseline, `now + offset`.
    fn present_at(&mut self, ts_us: u64, now_ms: f64) -> f64 {
        let offset = self.offset_ms();
        match self.jitter.baseline_ms(now_ms) {
            Some(base) => ts_us as f64 / 1000.0 + base + offset,
            None => now_ms + offset,
        }
    }

    /// Reconfigure only on a real change: every keyframe embeds its config.
    fn apply_config(&mut self, c: VideoConfig, out: &mut Vec<ViewerEvent>) {
        if self.current_config.as_ref() == Some(&c) {
            return;
        }
        self.current_config = Some(c.clone());
        out.push(ViewerEvent::VideoConfig(c));
    }

    /// A keyframe arrived serially behind the decode position: the
    /// broadcaster restarted, and everything derived from the old timeline
    /// (the baseline and jitter, both controllers, the clock mapping, the
    /// renderers' queues) goes.
    fn on_restart(&mut self, out: &mut Vec<ViewerEvent>) {
        self.restarts += 1;
        self.jitter.reset();
        self.playout.reset();
        self.grace.reset();
        self.clock_mapping_us = None;
        self.newest_frame_ts_us = None;
        self.presented_ts_us = None;
        out.push(ViewerEvent::Flush);
    }

    /// The watchdogs, checked on the tick (`viewer.ts`'s order).
    pub fn stall(&self, now_ms: f64) -> Option<Stall> {
        if self
            .last_inbound_ms
            .is_some_and(|t| now_ms - t >= SESSION_STALL_MS)
        {
            return Some(Stall::Session);
        }
        if let (Some(kf), Some(frame)) = (self.last_keyframe_ms, self.last_frame_ms)
            && now_ms - frame <= FRAMES_FLOWING_WINDOW_MS
            && now_ms - kf >= KEYFRAME_STALL_MS
        {
            return Some(Stall::Keyframe);
        }
        if self.audio_seen
            && self.mappings_since_media >= MAPPINGS_PROVING_LIVE
            && self
                .last_media_ms
                .is_some_and(|t| now_ms - t >= MEDIA_STALL_MS)
        {
            return Some(Stall::Media);
        }
        None
    }

    /// Capture-to-now latency of a frame, for the stats sheet (G3, G8):
    /// `now + time_sync_offset − (frame_ts + clock_mapping)`, all µs. `None`
    /// until both a TimeSync sample and a ClockMapping are in.
    pub fn capture_latency_ms(&self, frame_ts_us: u64, now_ms: f64) -> Option<f64> {
        let sync = self.time_sync.best()?;
        let mapping = self.clock_mapping_us?;
        let now_relay = (now_ms * 1000.0) as i64 + sync.offset_us;
        let captured_relay = frame_ts_us as i64 + mapping;
        Some(((now_relay - captured_relay) as f64 / 1000.0).max(0.0))
    }

    pub fn stats(&mut self, now_ms: f64) -> PipelineStats {
        PipelineStats {
            reassembly: self.reassembler.stats(),
            reorder: self.reorder.stats(),
            offset_ms: self.offset_ms(),
            grace_ms: self.grace.offset_ms(),
            jitter_ms: self.jitter.jitter_ms(now_ms),
            time_sync_rtt_ms: self.time_sync.best().map(|s| s.rtt_us as f64 / 1000.0),
            viewer_count: self.viewer_count,
            drops_to_live: self.drops_to_live,
            restarts: self.restarts,
        }
    }
}

#[cfg(test)]
mod tests;
