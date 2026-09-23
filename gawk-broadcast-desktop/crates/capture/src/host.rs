//! The host clock — the macOS end of the single-clock rule (docs/54 D5),
//! as `qpc` is the Windows end. ScreenCaptureKit stamps video and audio
//! sample buffers on `CMClockGetHostTimeClock()` (`mach_absolute_time`);
//! this reads the same clock so one affine offset maps either onto the
//! session clock, and A/V skew is zero by construction.

use crate::sck_policy::cm_time_to_100ns;
use gawk_engine::clock::{Clock, QpcMapper};
use objc2_core_media::{CMClock, CMTime, CMTimeFlags};

/// A `CMTime` on the host clock as 100 ns ticks; `None` if it is not a
/// valid numeric time.
pub fn to_100ns(t: CMTime) -> Option<i64> {
    cm_time_to_100ns(t.value, t.timescale, t.flags.contains(CMTimeFlags::Valid))
}

/// The host clock now, in 100 ns ticks.
pub fn now_100ns() -> i64 {
    // SAFETY: CMClockGetHostTimeClock returns the process-wide host clock
    // and CMClockGetTime only reads it.
    let now = unsafe { CMClock::host_time_clock().time() };
    to_100ns(now).unwrap_or(0)
}

/// The session's host-clock mapper from a back-to-back sample pair. Video
/// and audio (MB4) must map through mappers built against the SAME `clock`.
pub fn mapper(clock: &dyn Clock) -> QpcMapper {
    QpcMapper::new(now_100ns(), clock.now_us())
}
