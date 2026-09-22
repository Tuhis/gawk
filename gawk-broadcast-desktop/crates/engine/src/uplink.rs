//! Uplink-health detection (docs/38 D12 amendment, 2026-07-31): decides
//! when the shell should warn that the user's upload bandwidth is not
//! keeping up with the stream. Pure counter arithmetic over 1 Hz stats
//! snapshots — no clocks, no transport — so the whole policy is unit
//! tests.
//!
//! The signals are the ones F-12 made meaningful:
//! - keyframe streams superseded/failed climbing means keyframe writes are
//!   outliving whole GOPs (delivery taking too long — the direct symptom);
//! - frames dropped at send means delta datagram sends are failing;
//! - sent fps far below encode fps means most frames never leave the box.
//!
//! Hysteresis on both edges: several consecutive bad seconds raise the
//! warning (one slow keyframe is weather, not climate), and it takes a
//! longer run of clean seconds to clear it (a flapping banner is worse
//! than a sticky one).

use crate::stats::Stats;

/// Consecutive bad seconds before the warning raises.
pub const WARN_AFTER_BAD_SECONDS: u32 = 5;
/// Consecutive clean seconds before it clears.
pub const CLEAR_AFTER_GOOD_SECONDS: u32 = 15;

#[derive(Debug, Clone, Copy, Default)]
struct Counters {
    superseded: u64,
    failed: u64,
    dropped: u64,
}

/// Feed [`UplinkMonitor::observe`] one stats snapshot per second; it
/// returns whether the warning should currently show.
#[derive(Debug, Default)]
pub struct UplinkMonitor {
    last: Option<Counters>,
    bad_streak: u32,
    good_streak: u32,
    warned: bool,
}

impl UplinkMonitor {
    pub fn new() -> Self {
        Self::default()
    }

    /// One 1 Hz sample. Snapshot deltas decide whether this second was
    /// "bad"; the hysteresis decides whether the warning shows.
    pub fn observe(&mut self, st: &Stats) -> bool {
        let now = Counters {
            superseded: st.keyframe_streams_superseded,
            failed: st.keyframe_streams_failed,
            dropped: st.frames_dropped_at_send,
        };
        let Some(prev) = self.last.replace(now) else {
            return self.warned; // first sample: no deltas yet
        };

        let kf_pressure = (now.superseded.saturating_sub(prev.superseded))
            + (now.failed.saturating_sub(prev.failed));
        let dropped = now.dropped.saturating_sub(prev.dropped);
        // Keyframe cadence is ~2/s: ≥2 displaced writes in one second means
        // essentially every keyframe is running late.
        let bad = kf_pressure >= 2
            || dropped > 0
            || (st.encoder_fps > 20.0 && st.sent_fps < st.encoder_fps * 0.75);

        if bad {
            self.bad_streak += 1;
            self.good_streak = 0;
            if self.bad_streak >= WARN_AFTER_BAD_SECONDS {
                self.warned = true;
            }
        } else {
            self.good_streak += 1;
            self.bad_streak = 0;
            if self.good_streak >= CLEAR_AFTER_GOOD_SECONDS {
                self.warned = false;
            }
        }
        self.warned
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn healthy(superseded: u64) -> Stats {
        Stats {
            keyframe_streams_superseded: superseded,
            encoder_fps: 60.0,
            sent_fps: 58.0,
            ..Default::default()
        }
    }

    #[test]
    fn a_healthy_stream_never_warns() {
        let mut m = UplinkMonitor::new();
        for i in 0..60u64 {
            // One superseded per minute-ish is weather, not climate.
            assert!(!m.observe(&healthy(i / 30)));
        }
    }

    #[test]
    fn sustained_keyframe_pressure_warns_after_the_streak() {
        let mut m = UplinkMonitor::new();
        assert!(!m.observe(&healthy(0)));
        // Superseded climbing 2/s: bad seconds accumulate…
        let mut superseded = 0;
        for i in 1..=WARN_AFTER_BAD_SECONDS {
            superseded += 2;
            let warned = m.observe(&healthy(superseded));
            assert_eq!(warned, i >= WARN_AFTER_BAD_SECONDS, "second {i}");
        }
    }

    #[test]
    fn a_single_bad_second_is_not_a_warning() {
        let mut m = UplinkMonitor::new();
        m.observe(&healthy(0));
        assert!(!m.observe(&healthy(5)), "one bad second: no warning");
        for _ in 0..10 {
            assert!(!m.observe(&healthy(5)));
        }
    }

    #[test]
    fn dropped_deltas_and_starved_sent_fps_also_count_as_bad() {
        let mut m = UplinkMonitor::new();
        let mut st = healthy(0);
        m.observe(&st);
        let mut warned = false;
        for _ in 0..WARN_AFTER_BAD_SECONDS {
            st.frames_dropped_at_send += 30;
            warned = m.observe(&st);
        }
        assert!(warned);

        let mut m = UplinkMonitor::new();
        let starved = Stats {
            encoder_fps: 60.0,
            sent_fps: 20.0,
            ..Default::default()
        };
        m.observe(&starved);
        let mut warned = false;
        for _ in 0..WARN_AFTER_BAD_SECONDS {
            warned = m.observe(&starved);
        }
        assert!(warned);
    }

    #[test]
    fn the_warning_clears_only_after_a_long_clean_run() {
        let mut m = UplinkMonitor::new();
        let mut superseded = 0;
        m.observe(&healthy(superseded));
        for _ in 0..WARN_AFTER_BAD_SECONDS {
            superseded += 2;
            m.observe(&healthy(superseded));
        }
        // Clean seconds: stays warned until the clear streak completes
        // (this first clean observe is clean second 1).
        assert!(m.observe(&healthy(superseded)), "clean second 1");
        for i in 2..CLEAR_AFTER_GOOD_SECONDS {
            assert!(m.observe(&healthy(superseded)), "clean second {i}");
        }
        assert!(!m.observe(&healthy(superseded)), "cleared after the run");
    }

    #[test]
    fn a_bad_second_resets_the_clear_streak() {
        let mut m = UplinkMonitor::new();
        let mut superseded = 0;
        m.observe(&healthy(superseded));
        for _ in 0..WARN_AFTER_BAD_SECONDS {
            superseded += 2;
            m.observe(&healthy(superseded));
        }
        for _ in 0..CLEAR_AFTER_GOOD_SECONDS - 2 {
            assert!(m.observe(&healthy(superseded)));
        }
        superseded += 4; // bad again: the clean run restarts
        assert!(m.observe(&healthy(superseded)));
        for i in 1..CLEAR_AFTER_GOOD_SECONDS {
            assert!(m.observe(&healthy(superseded)), "clean second {i}");
        }
        assert!(!m.observe(&healthy(superseded)));
    }
}
