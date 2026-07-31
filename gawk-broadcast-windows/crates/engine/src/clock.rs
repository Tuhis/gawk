//! The single session clock (docs/38 D7). Everything that stamps a timestamp
//! — video frames, audio packets, TimeSync's t0 — must read the SAME
//! monotonic clock, or the A/V mapping silently relates two unrelated
//! timelines (R25's one-anchor lesson, structural here). On Windows,
//! `std::time::Instant` is QPC-backed, which is the same clock WGC frame
//! times and WASAPI packet positions are expressed in.

use std::time::Instant;

/// Microsecond monotonic clock, injectable so cadence and timestamp logic is
/// a unit test rather than a sleep.
pub trait Clock: Send + Sync + 'static {
    fn now_us(&self) -> u64;
}

/// The real clock: microseconds since an arbitrary per-session origin.
#[derive(Debug, Clone)]
pub struct MonotonicClock {
    origin: Instant,
}

impl MonotonicClock {
    pub fn new() -> Self {
        Self {
            origin: Instant::now(),
        }
    }
}

impl Default for MonotonicClock {
    fn default() -> Self {
        Self::new()
    }
}

impl Clock for MonotonicClock {
    fn now_us(&self) -> u64 {
        self.origin.elapsed().as_micros() as u64
    }
}

/// Maps device timestamps (QPC-derived 100 ns ticks: WGC's
/// `SystemRelativeTime`, WASAPI's `u64QPCPosition`) onto the session clock.
///
/// This is D7's "exactly one clock function" made concrete: both media
/// crates construct their mapper from the SAME pair of simultaneous samples
/// (one QPC read, one `Clock::now_us` read), so the mapping is one shared
/// affine offset and relative A/V skew stays zero by construction. Pure so
/// the R25 mutation test (video 5 ms late, audio 1 ms late — spacing must
/// survive) is a unit test.
#[derive(Debug, Clone, Copy)]
pub struct QpcMapper {
    qpc_origin_100ns: i64,
    clock_origin_us: u64,
}

impl QpcMapper {
    /// `qpc_100ns` and `clock_us` must be sampled back to back — the pairing
    /// error is the mapping error.
    pub fn new(qpc_100ns: i64, clock_us: u64) -> Self {
        Self {
            qpc_origin_100ns: qpc_100ns,
            clock_origin_us: clock_us,
        }
    }

    /// A device timestamp on the session timeline, µs. Saturates at the
    /// session origin: a pre-origin stamp (a buffered frame from before
    /// start) must not wrap into the far future.
    pub fn to_session_us(&self, device_qpc_100ns: i64) -> u64 {
        let delta_us = (device_qpc_100ns - self.qpc_origin_100ns) / 10;
        if delta_us >= 0 {
            self.clock_origin_us.saturating_add(delta_us as u64)
        } else {
            self.clock_origin_us.saturating_sub((-delta_us) as u64)
        }
    }
}

/// Test support: a scripted clock so cadence logic is a unit test rather
/// than a sleep. Lives in the public API (not `cfg(test)`) so integration
/// tests and other crates' tests can drive it too.
pub mod testing {
    use super::Clock;
    use std::sync::atomic::{AtomicU64, Ordering};

    #[derive(Default)]
    pub struct FakeClock(AtomicU64);

    impl FakeClock {
        pub fn set_us(&self, us: u64) {
            self.0.store(us, Ordering::SeqCst);
        }
        pub fn advance_ms(&self, ms: u64) {
            self.0.fetch_add(ms * 1000, Ordering::SeqCst);
        }
    }

    impl Clock for FakeClock {
        fn now_us(&self) -> u64 {
            self.0.load(Ordering::SeqCst)
        }
    }
}

#[cfg(test)]
mod qpc_tests {
    use super::*;

    #[test]
    fn maps_device_ticks_onto_the_session_timeline() {
        // Session started at clock=1_000_000 µs when QPC read 5_000_000
        // 100 ns ticks (= 500 ms of QPC time).
        let m = QpcMapper::new(5_000_000, 1_000_000);
        assert_eq!(m.to_session_us(5_000_000), 1_000_000);
        assert_eq!(m.to_session_us(5_000_000 + 10 * 1000), 1_001_000); // +1 ms
        // Pre-origin stamps map backwards, never wrap forwards.
        assert_eq!(m.to_session_us(5_000_000 - 10 * 1000), 999_000);
        let very_early = QpcMapper::new(5_000_000, 100);
        assert_eq!(very_early.to_session_us(0), 0); // saturates, no wrap
    }

    // R25's mutation test, restaged for D7: video arrives 5 ms late and
    // audio 1 ms late, but both carry DEVICE timestamps through the same
    // mapper — arrival jitter cannot appear in the mapping, so source
    // spacing survives exactly.
    #[test]
    fn one_mapper_preserves_source_spacing_for_both_media() {
        let m = QpcMapper::new(0, 0);
        let video_qpc = 1_000_000; // captured at 100 ms
        let audio_qpc = 1_200_000; // captured at 120 ms
        let video_us = m.to_session_us(video_qpc);
        let audio_us = m.to_session_us(audio_qpc);
        assert_eq!(audio_us - video_us, 20_000);
    }
}
