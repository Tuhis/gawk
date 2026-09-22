//! The producer-side frame gate, mirroring the Go source's `offer` policy
//! (gawk-broadcast/internal/gst/source.go:645-685, docs/38 D5):
//!
//! - The gate never blocks capture — blocking would stall the encoder.
//! - Once a delta is dropped, EVERYTHING until the next keyframe is dropped
//!   too: the drop happens before frame IDs are assigned, so the wire stays
//!   contiguous and the viewer's freeze-on-gap cannot see it — a delta whose
//!   reference was dropped would decode into reference soup.
//! - A keyframe arriving at a full queue FLUSHES the stale backlog and takes
//!   its place: under sustained backpressure the stream degrades to a clean
//!   keyframe cadence rather than artifacts.

use crate::media::AccessUnit;
use std::collections::VecDeque;

/// Queue capacity, matching the Go frame channel (capacity 8).
pub const FRAME_GATE_CAPACITY: usize = 8;

/// What `offer` did with the frame, for counters.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Offered {
    Queued,
    /// A delta was dropped (queue full, or inside a dropped GOP).
    DroppedDelta,
    /// A keyframe flushed `n` stale frames and was queued.
    FlushedBacklog {
        flushed: usize,
    },
}

#[derive(Debug)]
pub struct FrameGate {
    queue: VecDeque<AccessUnit>,
    capacity: usize,
    dropping_gop: bool,
}

impl FrameGate {
    pub fn new() -> Self {
        Self::with_capacity(FRAME_GATE_CAPACITY)
    }

    pub fn with_capacity(capacity: usize) -> Self {
        Self {
            queue: VecDeque::with_capacity(capacity),
            capacity,
            dropping_gop: false,
        }
    }

    /// Offers one AU; never blocks.
    pub fn offer(&mut self, au: AccessUnit) -> Offered {
        if au.keyframe {
            let flushed = self.queue.len();
            if self.queue.len() == self.capacity {
                self.queue.clear();
                self.dropping_gop = false;
                self.queue.push_back(au);
                return Offered::FlushedBacklog { flushed };
            }
            self.dropping_gop = false;
            self.queue.push_back(au);
            return Offered::Queued;
        }
        if self.dropping_gop || self.queue.len() == self.capacity {
            self.dropping_gop = true;
            return Offered::DroppedDelta;
        }
        self.queue.push_back(au);
        Offered::Queued
    }

    /// Takes the next AU for the send pump.
    pub fn pop(&mut self) -> Option<AccessUnit> {
        self.queue.pop_front()
    }

    pub fn len(&self) -> usize {
        self.queue.len()
    }

    pub fn is_empty(&self) -> bool {
        self.queue.is_empty()
    }
}

impl Default for FrameGate {
    fn default() -> Self {
        Self::new()
    }
}

/// The audio queue policy is the OPPOSITE of video's on overflow: bounded at
/// ~32 packets (≈640 ms) and evicting the OLDEST (docs/28 Decision 9 /
/// docs/24 finding 14) — for audio, the freshest packets are the ones worth
/// having.
pub const AUDIO_QUEUE_DEPTH: usize = 32;

#[derive(Debug, Default)]
pub struct AudioGate {
    queue: VecDeque<crate::media::AudioPacket>,
}

impl AudioGate {
    /// Offers one packet; returns how many stale packets were evicted (0/1).
    pub fn offer(&mut self, p: crate::media::AudioPacket) -> usize {
        let mut evicted = 0;
        while self.queue.len() >= AUDIO_QUEUE_DEPTH {
            self.queue.pop_front();
            evicted += 1;
        }
        self.queue.push_back(p);
        evicted
    }

    pub fn pop(&mut self) -> Option<crate::media::AudioPacket> {
        self.queue.pop_front()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn au(keyframe: bool, id: u64) -> AccessUnit {
        AccessUnit {
            data: vec![id as u8],
            timestamp_us: id,
            keyframe,
        }
    }

    #[test]
    fn a_dropped_delta_drops_the_rest_of_its_gop() {
        let mut g = FrameGate::with_capacity(2);
        assert_eq!(g.offer(au(true, 0)), Offered::Queued);
        assert_eq!(g.offer(au(false, 1)), Offered::Queued);
        // Full: this delta drops…
        assert_eq!(g.offer(au(false, 2)), Offered::DroppedDelta);
        // …and even after the pump drains, later deltas of the same GOP keep
        // dropping (their reference is gone — queueing them would be soup).
        g.pop();
        g.pop();
        assert!(g.is_empty());
        assert_eq!(g.offer(au(false, 3)), Offered::DroppedDelta);
        // The next keyframe ends the dropped GOP.
        assert_eq!(g.offer(au(true, 4)), Offered::Queued);
        assert_eq!(g.offer(au(false, 5)), Offered::Queued);
    }

    #[test]
    fn a_keyframe_into_a_full_queue_flushes_the_stale_backlog() {
        let mut g = FrameGate::with_capacity(3);
        g.offer(au(true, 0));
        g.offer(au(false, 1));
        g.offer(au(false, 2));
        assert_eq!(g.offer(au(true, 3)), Offered::FlushedBacklog { flushed: 3 });
        // Only the fresh keyframe remains.
        assert_eq!(g.len(), 1);
        let head = g.pop().unwrap();
        assert!(head.keyframe);
        assert_eq!(head.timestamp_us, 3);
    }

    #[test]
    fn audio_evicts_oldest_never_newest() {
        let mut g = AudioGate::default();
        for i in 0..AUDIO_QUEUE_DEPTH {
            assert_eq!(
                g.offer(crate::media::AudioPacket {
                    data: vec![],
                    timestamp_us: i as u64
                }),
                0
            );
        }
        assert_eq!(
            g.offer(crate::media::AudioPacket {
                data: vec![],
                timestamp_us: 999
            }),
            1
        );
        // The oldest (ts 0) is gone; the head is now ts 1.
        assert_eq!(g.pop().unwrap().timestamp_us, 1);
    }
}
