//! Drop-only fps gating (docs/38 D6, WB3 acceptance row 2).
//!
//! WGC is damage-driven, like PipeWire: a static desktop delivers almost
//! nothing, a game delivers at refresh rate. The rung's fps is enforced by
//! DROPPING surplus frames only — never by synthesizing repeats into CFR
//! (R14 Decision 13's invariant, inherited): the wire model treats absent
//! frames as truth, and a synthesized cadence would just spend bitrate
//! re-encoding stillness.

/// Admits at most `target_fps` frames per second, by timestamp.
///
/// Keeps a virtual schedule advanced one interval per admitted frame, the
/// browser broadcaster's gate (`gawk-app/src/media/preprocess.ts`). A frame
/// up to a quarter interval early still takes its slot, so the gate follows
/// the source's *average* cadence, not each gap. Spacing against the last
/// admitted frame instead drops every frame after a short gap and never makes
/// it up: 60 fps content on a 144 Hz display arrives 13.9/20.8 ms apart and
/// came out at 36 fps, and a 144 Hz source at 48.
#[derive(Debug)]
pub struct FpsGate {
    interval_us: u64,
    /// The slot the next frame is due in; `None` until the first frame.
    next_due_us: Option<u64>,
}

impl FpsGate {
    pub fn new(target_fps: u32) -> Self {
        Self {
            interval_us: 1_000_000 / u64::from(target_fps.max(1)),
            next_due_us: None,
        }
    }

    /// Whether the frame stamped `ts_us` (QPC-derived, µs) passes the gate.
    pub fn admit(&mut self, ts_us: u64) -> bool {
        let next_due = match self.next_due_us {
            // The first frame, or one more than an interval late (a
            // damage-driven gap, a stall): re-anchor on it. Never burst to
            // catch up — there are no "owed" frames.
            Some(due) if ts_us <= due + self.interval_us => due,
            _ => {
                self.next_due_us = Some(ts_us + self.interval_us);
                return true;
            }
        };
        if ts_us + self.interval_us / 4 < next_due {
            return false;
        }
        // Advancing a full interval per admitted frame is what caps the
        // rate: the early allowance shifts a slot, it never adds one.
        self.next_due_us = Some(next_due + self.interval_us);
        true
    }
}

/// Measured capture fps for the stats card: an EMA over admitted-frame
/// spacing (α = 0.3, the same smoothing the keyframe-interval display uses).
/// "Available" only after two frames — an absent number and a zero must stay
/// distinguishable (the Stats convention).
#[derive(Debug, Default)]
pub struct FpsMeter {
    last_us: Option<u64>,
    ema_interval_us: f64,
    available: bool,
}

impl FpsMeter {
    pub fn observe(&mut self, ts_us: u64) {
        if let Some(last) = self.last_us {
            let dt = ts_us.saturating_sub(last) as f64;
            if dt > 0.0 {
                self.ema_interval_us = if self.available {
                    0.3 * dt + 0.7 * self.ema_interval_us
                } else {
                    dt
                };
                self.available = true;
            }
        }
        self.last_us = Some(ts_us);
    }

    pub fn fps(&self) -> Option<f64> {
        self.available.then(|| 1_000_000.0 / self.ema_interval_us)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn caps_an_oversupplying_source_to_the_target() {
        let mut g = FpsGate::new(30);
        // A 60 fps source: every other frame passes.
        let admitted = (0..60u64).filter(|i| g.admit(i * 16_667)).count();
        assert!((28..=32).contains(&admitted), "admitted {admitted}");
    }

    #[test]
    fn nominal_cadence_passes_untouched() {
        // A source already at the target must not be halved by jitter
        // against an exact-interval gate.
        let mut g = FpsGate::new(60);
        let admitted = (0..120u64).filter(|i| g.admit(i * 16_667)).count();
        assert_eq!(admitted, 120);
    }

    /// Frame `i` of a `content_fps` source, presented on the first vsync of
    /// a `refresh_hz` display at or after its due time — how a browser
    /// playing a 60 fps video on a high-refresh monitor reaches WGC/SCK.
    fn vsync_presented(i: u64, content_fps: u64, refresh_hz: u64) -> u64 {
        let due_ns = i * 1_000_000_000 / content_fps;
        let tick_ns = 1_000_000_000 / refresh_hz;
        due_ns.div_ceil(tick_ns) * tick_ns / 1_000
    }

    #[test]
    fn vsync_quantized_content_at_the_target_passes_untouched() {
        // 60 fps content on a 144 Hz or 165 Hz display arrives 2 or 3
        // refreshes apart (13.9/20.8 ms, 12.1/18.2 ms): the short gaps are
        // under any fixed minimum spacing, but the average is exactly the
        // target and nothing is surplus.
        for refresh_hz in [144, 165] {
            let mut g = FpsGate::new(60);
            let admitted = (0..600u64)
                .filter(|&i| g.admit(vsync_presented(i, 60, refresh_hz)))
                .count();
            assert!(admitted >= 594, "{refresh_hz} Hz: admitted {admitted}/600");
        }
    }

    #[test]
    fn a_high_refresh_source_is_still_capped_to_the_target() {
        // Every refresh of a 144 Hz display carries new content: only the
        // target's worth passes, bursts or not.
        let mut g = FpsGate::new(60);
        let admitted = (0..1440u64)
            .filter(|&i| g.admit(vsync_presented(i, 144, 144)))
            .count();
        assert!((595..=605).contains(&admitted), "admitted {admitted}");
    }

    #[test]
    fn damage_driven_gaps_pass_through_never_synthesized() {
        let mut g = FpsGate::new(60);
        assert!(g.admit(0));
        // Nothing for a second (static screen) — the next real frame is
        // simply admitted; there is no notion of "owed" frames.
        assert!(g.admit(1_000_000));
        assert!(!g.admit(1_001_000)); // burst after the gap still gated
    }

    #[test]
    fn meter_needs_two_frames_then_tracks() {
        let mut m = FpsMeter::default();
        assert_eq!(m.fps(), None);
        m.observe(0);
        assert_eq!(m.fps(), None);
        for i in 1..60u64 {
            m.observe(i * 16_667);
        }
        let fps = m.fps().unwrap();
        assert!((59.0..61.0).contains(&fps), "fps {fps}");
    }
}
