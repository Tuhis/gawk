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
