//! `CLOCK_MONOTONIC` — the Linux end of the single-clock rule (docs/58 D4),
//! as `qpc` is the Windows end and `host` the macOS one. Both GStreamer
//! pipelines run on the system clock in its monotonic mode, so a buffer's
//! `base_time + running time` is a `CLOCK_MONOTONIC` reading; this reads the
//! same clock, so one affine offset maps video and audio onto the session
//! clock and A/V skew is zero by construction. The Go app's `ptsAnchor` and
//! its bias gate existed because of the MPEG-TS pipe, and are gone with it.

use gawk_engine::clock::{Clock, QpcMapper};

/// `CLOCK_MONOTONIC` now, in ns.
pub fn now_ns() -> u64 {
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    // SAFETY: clock_gettime writes one timespec through a valid pointer;
    // CLOCK_MONOTONIC always exists on Linux.
    unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut ts) };
    ts.tv_sec as u64 * 1_000_000_000 + ts.tv_nsec as u64
}

/// Maps `CLOCK_MONOTONIC` ns onto the session clock — the shared affine
/// mapper, in ns instead of the 100 ns units Windows and macOS feed it.
#[derive(Debug, Clone, Copy)]
pub struct Mapper(QpcMapper);

impl Mapper {
    /// `monotonic_ns` and `clock_us` must be sampled back to back — the
    /// pairing error is the mapping error.
    pub fn new(monotonic_ns: u64, clock_us: u64) -> Self {
        Self(QpcMapper::new((monotonic_ns / 100) as i64, clock_us))
    }

    /// A `CLOCK_MONOTONIC` reading on the session timeline, µs.
    pub fn to_session_us(&self, monotonic_ns: u64) -> u64 {
        self.0.to_session_us((monotonic_ns / 100) as i64)
    }
}

/// The session's mapper from a back-to-back sample pair. Video and audio
/// must map through mappers built against the SAME `clock`.
pub fn mapper(clock: &dyn Clock) -> Mapper {
    Mapper::new(now_ns(), clock.now_us())
}

#[cfg(test)]
mod tests {
    use super::*;
    use gawk_engine::clock::MonotonicClock;

    #[test]
    fn maps_ns_onto_the_session_timeline() {
        let m = Mapper::new(5_000_000_000, 1_000_000);
        assert_eq!(m.to_session_us(5_000_000_000), 1_000_000);
        assert_eq!(m.to_session_us(5_001_000_000), 1_001_000);
        assert_eq!(m.to_session_us(4_999_000_000), 999_000);
    }

    /// The engine's clock is `Instant`, which is `CLOCK_MONOTONIC` on Linux:
    /// a reading taken later through the mapper lands where the engine's
    /// clock says it is, to within scheduling noise.
    #[test]
    fn the_engine_clock_and_this_one_are_the_same_clock() {
        let clock = MonotonicClock::new();
        std::thread::sleep(std::time::Duration::from_millis(5));
        let m = mapper(&clock);
        std::thread::sleep(std::time::Duration::from_millis(20));
        let mapped = m.to_session_us(now_ns());
        let engine = clock.now_us();
        assert!(
            mapped.abs_diff(engine) < 2_000,
            "mapped {mapped} vs engine {engine}"
        );
    }
}
