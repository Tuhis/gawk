//! Delta-frame chunking, mirroring the Go engine's send policy budget
//! (gawk-broadcast/internal/engine/send.go): start at the wire maximum and
//! only ever shrink, once, in response to a real too-large error — "never
//! assume 1200 is reachable".

use crate::error::WireError;
use crate::{MAX_CHUNK_COUNT, MAX_CHUNK_PAYLOAD, VIDEO_CHUNK_HEADER_SIZE};

/// The per-chunk payload budget for delta datagrams.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ChunkBudget {
    payload: usize,
}

impl ChunkBudget {
    /// Starts at [`MAX_CHUNK_PAYLOAD`] (1180).
    pub fn new() -> Self {
        Self {
            payload: MAX_CHUNK_PAYLOAD,
        }
    }

    /// The current per-chunk payload budget.
    pub fn payload(&self) -> usize {
        self.payload
    }

    /// Adjusts the budget to a reported maximum datagram size (the whole
    /// datagram, header included). Mirrors Go `shrinkChunk`: refuses a
    /// non-shrinking change so a retry loop cannot spin, floors at 1, and the
    /// new size sticks for every later frame. Returns whether it changed.
    pub fn shrink_for_datagram_size(&mut self, max_datagram_size: usize) -> bool {
        let new_payload = max_datagram_size
            .saturating_sub(VIDEO_CHUNK_HEADER_SIZE)
            .max(1);
        if new_payload >= self.payload {
            return false;
        }
        self.payload = new_payload;
        true
    }
}

impl Default for ChunkBudget {
    fn default() -> Self {
        Self::new()
    }
}

/// Splits an encoded frame into per-chunk payload slices under a budget. A
/// zero-length frame still produces exactly one (empty) chunk — the wire has
/// no zero-chunk frames. Errors if the frame would need more than
/// [`MAX_CHUNK_COUNT`] chunks (the relay's parse bound; such a frame could
/// never be delivered).
pub fn split_frame<'a>(frame: &'a [u8], budget: &ChunkBudget) -> Result<Vec<&'a [u8]>, WireError> {
    let payload = budget.payload();
    if frame.is_empty() {
        return Ok(vec![&frame[..0]]);
    }
    let count = frame.len().div_ceil(payload);
    if count > MAX_CHUNK_COUNT as usize {
        return Err(WireError::BadChunkCount {
            index: 0,
            count: count as u32,
        });
    }
    Ok(frame.chunks(payload).collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::MAX_DATAGRAM_SIZE;

    #[test]
    fn budget_starts_at_wire_max_and_only_shrinks() {
        let mut b = ChunkBudget::new();
        assert_eq!(b.payload(), MAX_CHUNK_PAYLOAD);

        // A reported max at or above the current total is refused.
        assert!(!b.shrink_for_datagram_size(MAX_DATAGRAM_SIZE));
        assert!(!b.shrink_for_datagram_size(MAX_DATAGRAM_SIZE + 400));
        assert_eq!(b.payload(), MAX_CHUNK_PAYLOAD);

        // A genuinely smaller path MTU shrinks, once, and sticks.
        assert!(b.shrink_for_datagram_size(1000));
        assert_eq!(b.payload(), 1000 - VIDEO_CHUNK_HEADER_SIZE);
        // Re-reporting the same size is a no-op (the retry can't loop).
        assert!(!b.shrink_for_datagram_size(1000));
        // Growth is refused — the learned budget is permanent for the session.
        assert!(!b.shrink_for_datagram_size(1200));
        assert_eq!(b.payload(), 1000 - VIDEO_CHUNK_HEADER_SIZE);

        // Pathological tiny values floor at 1, never 0.
        assert!(b.shrink_for_datagram_size(5));
        assert_eq!(b.payload(), 1);
    }

    #[test]
    fn zero_length_frame_is_one_empty_chunk() {
        let chunks = split_frame(b"", &ChunkBudget::new()).unwrap();
        assert_eq!(chunks.len(), 1);
        assert!(chunks[0].is_empty());
    }

    #[test]
    fn split_covers_frame_exactly_with_short_tail() {
        let frame = vec![0xAB; MAX_CHUNK_PAYLOAD * 2 + 100];
        let chunks = split_frame(&frame, &ChunkBudget::new()).unwrap();
        assert_eq!(chunks.len(), 3);
        assert_eq!(chunks[0].len(), MAX_CHUNK_PAYLOAD);
        assert_eq!(chunks[1].len(), MAX_CHUNK_PAYLOAD);
        assert_eq!(chunks[2].len(), 100);
        let total: usize = chunks.iter().map(|c| c.len()).sum();
        assert_eq!(total, frame.len());
    }

    #[test]
    fn frames_beyond_the_relay_parse_bound_are_refused() {
        let frame = vec![0u8; MAX_CHUNK_PAYLOAD * (MAX_CHUNK_COUNT as usize) + 1];
        assert!(matches!(
            split_frame(&frame, &ChunkBudget::new()),
            Err(WireError::BadChunkCount { .. })
        ));
    }
}
