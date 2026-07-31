//! Accumulates WASAPI capture packets into exact 20 ms frames and stamps
//! them on the session clock (docs/38 D7/D8).
//!
//! Stamping policy, pre-registered in D7's caveat (V-4): when capture
//! packets carry QPC positions, the frame timestamp derives from the FIRST
//! sample's QPC through the shared [`QpcMapper`] — the same affine map video
//! uses, so relative A/V skew is zero by construction. Process-loopback
//! streams may report zero QPC positions (Microsoft's own sample hedges);
//! the fallback is arrival stamping minus buffered duration, which
//! reproduces the Linux model and its measured-bias gate (R14 Decision 6).

use crate::opusenc::{FRAME_INTERLEAVED_LEN, FRAME_SAMPLES};
use gawk_engine::media::AUDIO_SAMPLE_RATE;

/// One complete 20 ms frame, ready for the encoder.
pub struct Frame {
    pub interleaved: Vec<f32>,
    /// Session-clock µs of the frame's first sample.
    pub timestamp_us: u64,
}

pub struct Framer {
    buf: Vec<f32>,
    /// Session-clock stamp of buf[0], set when the first samples arrive.
    head_us: u64,
}

impl Framer {
    pub fn new() -> Self {
        Self {
            buf: Vec::with_capacity(FRAME_INTERLEAVED_LEN * 2),
            head_us: 0,
        }
    }

    /// Feeds one capture packet. `packet_us` is the session-clock stamp of
    /// the packet's first sample — QPC-mapped when available, else arrival
    /// time minus the device-buffered duration (the caller resolves V-4;
    /// the framer only does the sample-count arithmetic).
    /// Returns every completed 20 ms frame.
    pub fn push(&mut self, interleaved: &[f32], packet_us: u64) -> Vec<Frame> {
        if self.buf.is_empty() {
            self.head_us = packet_us;
        }
        self.buf.extend_from_slice(interleaved);

        let mut out = Vec::new();
        while self.buf.len() >= FRAME_INTERLEAVED_LEN {
            let rest = self.buf.split_off(FRAME_INTERLEAVED_LEN);
            let frame = std::mem::replace(&mut self.buf, rest);
            out.push(Frame {
                interleaved: frame,
                timestamp_us: self.head_us,
            });
            // The next frame starts exactly one frame later on the sample
            // timeline — derived from sample count, not from arrival.
            self.head_us += FRAME_SAMPLES as u64 * 1_000_000 / u64::from(AUDIO_SAMPLE_RATE);
        }
        out
    }

    /// Drops buffered samples (capture re-open: mode 2's device switch) —
    /// a gap on the wire is truth, a spliced frame is not.
    pub fn reset(&mut self) {
        self.buf.clear();
    }
}

impl Default for Framer {
    fn default() -> Self {
        Self::new()
    }
}

/// The V-4 stamping decision for one packet, in one place so it is a unit
/// test: QPC position when the device reports one, else arrival minus what
/// the device says is buffered ahead of this packet.
pub fn packet_timestamp_us(
    qpc_mapped_us: Option<u64>,
    arrival_us: u64,
    buffered_samples: usize,
) -> u64 {
    match qpc_mapped_us {
        Some(us) => us,
        None => arrival_us
            .saturating_sub(buffered_samples as u64 * 1_000_000 / u64::from(AUDIO_SAMPLE_RATE)),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn packets_reassemble_into_exact_frames_with_derived_stamps() {
        let mut f = Framer::new();
        // 480-sample (10 ms) packets: every second push completes a frame.
        let pkt = vec![0.5f32; 480 * 2];
        assert!(f.push(&pkt, 1_000_000).is_empty());
        let frames = f.push(&pkt, 1_010_000);
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].interleaved.len(), FRAME_INTERLEAVED_LEN);
        assert_eq!(frames[0].timestamp_us, 1_000_000);

        // The buffer emptied with that frame, so the next packet's own
        // stamp is adopted as the new head — per-packet stamps are the
        // accurate source (QPC) and re-anchoring there is self-correcting;
        // sample-count extrapolation is used only WITHIN buffered data.
        assert!(f.push(&pkt, 1_023_456).is_empty());
        let frames = f.push(&pkt, 1_031_000);
        assert_eq!(frames[0].timestamp_us, 1_023_456);
    }

    #[test]
    fn one_big_packet_yields_multiple_frames() {
        let mut f = Framer::new();
        let pkt = vec![0.1f32; FRAME_INTERLEAVED_LEN * 2 + 100];
        let frames = f.push(&pkt, 500_000);
        assert_eq!(frames.len(), 2);
        assert_eq!(frames[0].timestamp_us, 500_000);
        assert_eq!(frames[1].timestamp_us, 520_000);
    }

    #[test]
    fn v4_stamping_prefers_qpc_and_falls_back_to_arrival_minus_buffered() {
        assert_eq!(packet_timestamp_us(Some(42), 999_999, 480), 42);
        // 480 buffered samples = 10 ms behind arrival.
        assert_eq!(packet_timestamp_us(None, 1_000_000, 480), 990_000);
        assert_eq!(packet_timestamp_us(None, 5_000, 480), 0); // saturates
    }

    #[test]
    fn reset_drops_partial_frames_instead_of_splicing_across_a_gap() {
        let mut f = Framer::new();
        f.push(&vec![0.5f32; 480 * 2], 0);
        f.reset();
        // After the reset the next packet starts a fresh frame at its own
        // timestamp.
        let frames = f.push(&vec![0.5f32; FRAME_INTERLEAVED_LEN], 2_000_000);
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].timestamp_us, 2_000_000);
    }
}
