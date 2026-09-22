//! Raw QPC sampling — the Windows end of the [`gawk_engine::clock::QpcMapper`]
//! pairing (docs/38 D7). WGC's `SystemRelativeTime` and WASAPI's
//! `u64QPCPosition` are both QPC expressed as 100 ns ticks; this reads the
//! same clock in the same unit so one affine offset maps either onto the
//! session clock.

use gawk_engine::clock::{Clock, QpcMapper};
use windows::Win32::System::Performance::{QueryPerformanceCounter, QueryPerformanceFrequency};

/// The current QPC value in 100 ns ticks.
pub fn now_qpc_100ns() -> i64 {
    let mut counter = 0i64;
    let mut freq = 0i64;
    // These cannot fail on XP+; the windows crate still surfaces Results.
    unsafe {
        let _ = QueryPerformanceCounter(&mut counter);
        let _ = QueryPerformanceFrequency(&mut freq);
    }
    if freq <= 0 {
        return 0;
    }
    // i128 keeps the scale exact: counter * 1e7 overflows i64 within hours
    // of uptime on a 10 MHz QPC.
    ((i128::from(counter) * 10_000_000) / i128::from(freq)) as i64
}

/// Builds the session's QPC mapper from a back-to-back sample pair. Both
/// media pumps must map through mappers built against the SAME `clock` —
/// that is the single-clock invariant in practice.
pub fn mapper(clock: &dyn Clock) -> QpcMapper {
    QpcMapper::new(now_qpc_100ns(), clock.now_us())
}
