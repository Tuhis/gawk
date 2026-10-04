//! Merges stream keyframes and datagram deltas into decode order (docs/67
//! D13). A port of the SPA's `transport/reorder-buffer.ts`.
//!
//! Keyframes arrive reliably on uni streams and deltas fast-but-lossy as
//! datagrams, so the two race: delta N+1 can land before keyframe N has
//! finished its stream. This buffer joins them by frame ID and releases in
//! decode order, as soon as a frame is decodable, or, under the adaptive
//! playout offset (D15), once `now >= ts + baseline + offset − decode lead`.
//! Pacing adds delay, never patience: every drop and resync below fires the
//! same way.
//!
//! Two bounded waits exist only to settle the race:
//! - while waiting for a keyframe, undecodable deltas are held at most
//!   [`keyframe_wait_ms`];
//! - when the next delta is missing but later frames are here, a straggler
//!   gets the delta-gap grace, after which the buffer freezes and resyncs at
//!   the next keyframe.
//!
//! **The delta-loss rule (D13) is stricter than the SPA's default.** The web
//! steps over one lost delta per GOP and shows artifacts until the next
//! keyframe; this viewer never decodes past a hole (the SPA's loss allowance
//! at 0), so a loss is a short freeze, not corruption. The allowance and its
//! delta evidence are therefore not ported.
//!
//! Pure and clock-free: the caller passes `now_ms` and the [`Pacing`] inputs
//! on every call, owns the arrival trackers that the baseline and the jitter
//! come from (reset them on [`Reordered::Restart`]), and ticks it so the
//! waits elapse without new arrivals.

use crate::playout::{Envelope, OFFSET_SLEW_DOWN_MS_PER_S, OFFSET_SLEW_UP_MS_PER_S};
use crate::reassembly::VideoConfig;
use gawk_wire::frame_id_ahead;

/// The SPA's `KEYFRAME_WAIT_MS`: covers a keyframe stream landing hundreds of
/// ms behind its trailing deltas on a congested link (R10's field finding).
pub const KEYFRAME_WAIT_MS: f64 = 1000.0;
/// The SPA's `DELTA_GAP_GRACE_MS`, the adaptive grace's seed and floor.
pub const DELTA_GAP_GRACE_MS: f64 = 60.0;
/// The SPA's `MAX_DELTA_GAP_GRACE_MS`, the adaptive grace's ceiling.
pub const MAX_DELTA_GAP_GRACE_MS: f64 = 250.0;
/// The SPA's `MAX_BUFFERED_FRAMES`; the oldest-received entry goes past it.
pub const MAX_BUFFERED_FRAMES: usize = 64;
/// The SPA's `KEYFRAME_WAIT_PLAYOUT_HEADROOM_MS`: one GOP.
pub const KEYFRAME_WAIT_PLAYOUT_HEADROOM_MS: f64 = 500.0;

/// The adaptive delta-gap grace (`GRACE_ENVELOPE` in `reorder-buffer.ts`):
/// patience, not delay, so it sits on the arrival jitter the playout offset
/// reads. A late frame arrives and buys itself patience; a lost one never
/// arrives and leaves the grace at its floor to freeze fast. A large rise is
/// stepped (under-patience costs a visible freeze now); a descent never is.
pub const GRACE_ENVELOPE: Envelope = Envelope {
    seed_ms: DELTA_GAP_GRACE_MS,
    min_ms: DELTA_GAP_GRACE_MS,
    max_ms: MAX_DELTA_GAP_GRACE_MS,
    slew_up_ms_per_s: OFFSET_SLEW_UP_MS_PER_S,
    slew_down_ms_per_s: OFFSET_SLEW_DOWN_MS_PER_S,
    step_up_above_ms: 50.0,
};

/// The keyframe wait must outlast the playout offset, or every held delta
/// ages out before its keyframe comes due: keyframe-only playback.
pub fn keyframe_wait_ms(offset_ms: f64) -> f64 {
    KEYFRAME_WAIT_MS.max(offset_ms + KEYFRAME_WAIT_PLAYOUT_HEADROOM_MS)
}

/// What the release schedule reads, live, on every call.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Pacing {
    /// The playout offset; 0 releases on arrival (live edge).
    pub offset_ms: f64,
    /// How early a frame releases to the decoder before its display target
    /// (non-zero only under the adaptive offset).
    pub decode_lead_ms: f64,
    /// The delta-gap grace, from the adaptive grace controller.
    pub grace_ms: f64,
    /// The windowed min of `arrival − timestamp`, ms: the pacing anchor.
    /// `None` before any frame.
    pub baseline_ms: Option<f64>,
}

impl Default for Pacing {
    /// Live edge with the shipped grace: release on arrival.
    fn default() -> Self {
        Self {
            offset_ms: 0.0,
            decode_lead_ms: 0.0,
            grace_ms: DELTA_GAP_GRACE_MS,
            baseline_ms: None,
        }
    }
}

/// A keyframe off its uni stream (StreamFrame, 0x04), config split out.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StreamKeyframe {
    pub frame_id: u32,
    pub timestamp_us: u64,
    /// The DecoderConfig the stream embeds ahead of the payload.
    pub config: Option<VideoConfig>,
    pub data: Vec<u8>,
}

/// A frame released to the decoder, in decode order.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ReleasedFrame {
    pub frame_id: u32,
    pub keyframe: bool,
    pub timestamp_us: u64,
    pub data: Vec<u8>,
    /// The keyframe's embedded config; never on a delta.
    pub config: Option<VideoConfig>,
}

/// What a call produced.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Reordered {
    Frame(ReleasedFrame),
    /// A keyframe arrived serially behind the decode position: the
    /// broadcaster restarted (frame IDs reset). Timestamps are on a new
    /// timeline, so the caller resets its arrival trackers.
    Restart,
}

/// The SPA's `ReorderStats`, less the loss-allowance counter.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct ReorderStats {
    pub released: u64,
    pub keyframes_released: u64,
    /// Stale (at or behind the decode position) or evicted by the cap.
    pub deltas_dropped: u64,
    /// A missing delta declared a gap: frozen until a keyframe.
    pub gap_resyncs: u64,
    /// Undecodable frames aged out while waiting for a keyframe.
    pub keyframe_wait_drops: u64,
    /// Held right now.
    pub buffered: u64,
}

struct Entry {
    frame_id: u32,
    keyframe: bool,
    timestamp_us: u64,
    data: Vec<u8>,
    config: Option<VideoConfig>,
    received_at_ms: f64,
}

/// See the module docs.
pub struct ReorderBuffer {
    /// Insertion order is arrival order; at most [`MAX_BUFFERED_FRAMES`].
    buffer: Vec<Entry>,
    /// The last frame released; `None` before the first.
    decode_position: Option<u32>,
    /// Before the first frame, after a gap, or on a requested resync.
    waiting_for_keyframe: bool,
    stats: ReorderStats,
}

impl Default for ReorderBuffer {
    fn default() -> Self {
        Self {
            buffer: Vec::new(),
            decode_position: None,
            waiting_for_keyframe: true,
            stats: ReorderStats::default(),
        }
    }
}

impl ReorderBuffer {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn stats(&self) -> ReorderStats {
        ReorderStats {
            buffered: self.buffer.len() as u64,
            ..self.stats
        }
    }

    pub fn push_keyframe(
        &mut self,
        kf: StreamKeyframe,
        now_ms: f64,
        pacing: &Pacing,
        out: &mut Vec<Reordered>,
    ) {
        let StreamKeyframe {
            frame_id,
            timestamp_us,
            config,
            data,
        } = kf;
        // Only an exact duplicate of the current position is dropped: a frame
        // ID reset also looks "older", and that one must get through.
        if self.decode_position == Some(frame_id) {
            return;
        }
        // Serially behind the decode position is the restart signal. Resync
        // to it at once: waiting out the grace would stale-drop the new
        // session's first deltas against the old position.
        let backwards = self
            .decode_position
            .is_some_and(|p| !frame_id_ahead(frame_id, p));
        if backwards {
            // The old session's frames could pass for the new session's
            // oldest waiting ones.
            let deltas = self.buffer.iter().filter(|e| !e.keyframe).count();
            self.stats.deltas_dropped += deltas as u64;
            self.buffer.clear();
            out.push(Reordered::Restart);
            if !self.waiting_for_keyframe {
                self.stats.gap_resyncs += 1;
                self.waiting_for_keyframe = true;
            }
        }
        self.insert(Entry {
            frame_id,
            keyframe: true,
            timestamp_us,
            data,
            config,
            received_at_ms: now_ms,
        });
        self.advance(now_ms, pacing, out);
    }

    pub fn push_delta(
        &mut self,
        frame_id: u32,
        timestamp_us: u64,
        data: Vec<u8>,
        now_ms: f64,
        pacing: &Pacing,
        out: &mut Vec<Reordered>,
    ) {
        if self
            .decode_position
            .is_some_and(|p| !frame_id_ahead(frame_id, p))
        {
            self.stats.deltas_dropped += 1;
            return;
        }
        self.insert(Entry {
            frame_id,
            keyframe: false,
            timestamp_us,
            data,
            config: None,
            received_at_ms: now_ms,
        });
        self.advance(now_ms, pacing, out);
    }

    /// Lets the time-bounded waits elapse without new arrivals.
    pub fn tick(&mut self, now_ms: f64, pacing: &Pacing, out: &mut Vec<Reordered>) {
        self.advance(now_ms, pacing, out);
    }

    /// Stop feeding the decoder until the next keyframe: the drop-to-live
    /// lever (D15).
    pub fn request_resync(&mut self, now_ms: f64, pacing: &Pacing, out: &mut Vec<Reordered>) {
        if !self.waiting_for_keyframe {
            self.stats.gap_resyncs += 1;
        }
        self.waiting_for_keyframe = true;
        self.advance(now_ms, pacing, out);
    }

    fn insert(&mut self, e: Entry) {
        if let Some(i) = self.buffer.iter().position(|x| x.frame_id == e.frame_id) {
            // A keyframe supersedes a same-ID delta; any other duplicate is
            // ignored.
            if e.keyframe && !self.buffer[i].keyframe {
                self.buffer[i] = e;
            }
            return;
        }
        self.buffer.push(e);
        while self.buffer.len() > MAX_BUFFERED_FRAMES {
            self.buffer.remove(0);
            self.stats.deltas_dropped += 1;
        }
    }

    fn advance(&mut self, now_ms: f64, pacing: &Pacing, out: &mut Vec<Reordered>) {
        loop {
            let Some(pos) = self.decode_position.filter(|_| !self.waiting_for_keyframe) else {
                if self.jump_to_keyframe(now_ms, pacing, out) {
                    continue;
                }
                self.drop_stale_while_waiting(now_ms, pacing);
                return;
            };
            let next = pos.wrapping_add(1);
            if let Some(i) = self.buffer.iter().position(|e| e.frame_id == next) {
                if now_ms < releasable_at(&self.buffer[i], pacing) {
                    return; // paced: a tick re-drives it
                }
                let e = self.buffer.remove(i);
                self.release(e, out);
                continue;
            }
            // The contiguous next frame is missing. A keyframe already
            // buffered ahead is a definitive resync point: jump now.
            if self
                .buffer
                .iter()
                .any(|e| e.keyframe && frame_id_ahead(e.frame_id, pos))
            {
                self.stats.gap_resyncs += 1;
                self.waiting_for_keyframe = true;
                continue;
            }
            // Otherwise give a straggler the grace, then freeze until the
            // next (reliable) keyframe. D13: never step over the hole.
            let oldest = self
                .buffer
                .iter()
                .map(|e| e.received_at_ms)
                .fold(f64::INFINITY, f64::min);
            if oldest.is_finite() && now_ms - oldest >= pacing.grace_ms {
                self.stats.gap_resyncs += 1;
                self.waiting_for_keyframe = true;
                continue;
            }
            return;
        }
    }

    /// Releases the freshest keyframe that is actually due, dropping what's
    /// behind it. "Due" is part of the selection: picking the freshest
    /// overall and then rejecting it as not due livelocks whenever the
    /// offset exceeds the GOP, because a newer keyframe always arrives first.
    fn jump_to_keyframe(&mut self, now_ms: f64, pacing: &Pacing, out: &mut Vec<Reordered>) -> bool {
        let mut best: Option<&Entry> = None;
        for e in &self.buffer {
            if !e.keyframe || now_ms < releasable_at(e, pacing) {
                continue;
            }
            if best.is_none_or(|b| e.received_at_ms > b.received_at_ms) {
                best = Some(e);
            }
        }
        let Some(best_id) = best.map(|b| b.frame_id) else {
            return false;
        };
        let mut kept = Vec::with_capacity(self.buffer.len());
        let mut best = None;
        for e in self.buffer.drain(..) {
            if e.frame_id == best_id && e.keyframe {
                best = Some(e);
            } else if !frame_id_ahead(e.frame_id, best_id) {
                if !e.keyframe {
                    self.stats.deltas_dropped += 1;
                }
            } else {
                kept.push(e);
            }
        }
        self.buffer = kept;
        self.waiting_for_keyframe = false;
        if let Some(e) = best {
            self.release(e, out);
        }
        true
    }

    fn drop_stale_while_waiting(&mut self, now_ms: f64, pacing: &Pacing) {
        let cutoff = now_ms - keyframe_wait_ms(pacing.offset_ms);
        let before = self.buffer.len();
        // Keyframes are always worth resyncing on.
        self.buffer
            .retain(|e| e.keyframe || e.received_at_ms >= cutoff);
        self.stats.keyframe_wait_drops += (before - self.buffer.len()) as u64;
    }

    fn release(&mut self, e: Entry, out: &mut Vec<Reordered>) {
        self.decode_position = Some(e.frame_id);
        self.stats.released += 1;
        if e.keyframe {
            self.stats.keyframes_released += 1;
        }
        out.push(Reordered::Frame(ReleasedFrame {
            frame_id: e.frame_id,
            keyframe: e.keyframe,
            timestamp_us: e.timestamp_us,
            data: e.data,
            config: e.config,
        }));
    }
}

/// When a frame may go to the decoder: at once on the live edge, else
/// `ts + baseline + offset − decode lead`.
fn releasable_at(e: &Entry, p: &Pacing) -> f64 {
    if p.offset_ms <= 0.0 {
        return 0.0;
    }
    let Some(base) = p.baseline_ms else {
        return 0.0;
    };
    e.timestamp_us as f64 / 1000.0 + base + p.offset_ms - p.decode_lead_ms
}

#[cfg(test)]
mod tests;
