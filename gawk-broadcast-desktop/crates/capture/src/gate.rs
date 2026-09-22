//! Drop-only fps gating (docs/38 D6, WB3 acceptance row 2).
//!
//! WGC is damage-driven, like PipeWire: a static desktop delivers almost
//! nothing, a game delivers at refresh rate. The rung's fps is enforced by
//! DROPPING surplus frames only — never by synthesizing repeats into CFR
//! (R14 Decision 13's invariant, inherited): the wire model treats absent
//! frames as truth, and a synthesized cadence would just spend bitrate
//! re-encoding stillness.

/// Admits at most `target_fps` frames per second, by timestamp.
#[derive(Debug)]
pub struct FpsGate {
    /// Minimum spacing between admitted frames, µs.
    min_interval_us: u64,
    /// Timestamp of the last admitted frame; `None` until the first.
    last_admitted_us: Option<u64>,
}

impl FpsGate {
    pub fn new(target_fps: u32) -> Self {
        Self {
            // A hair under the nominal interval: capture clocks jitter, and
            // a full-interval gate would halve an exactly-on-cadence source
            // (frame at 16.66 ms vs gate at 16.67 ms — every other frame
            // rejected). 95 % admits nominal cadence, still caps the rate.
            min_interval_us: 950_000 / u64::from(target_fps.max(1)),
            last_admitted_us: None,
        }
    }

    /// Whether the frame stamped `ts_us` (QPC-derived, µs) passes the gate.
    pub fn admit(&mut self, ts_us: u64) -> bool {
        match self.last_admitted_us {
            Some(last) if ts_us < last + self.min_interval_us => false,
            _ => {
                self.last_admitted_us = Some(ts_us);
                true
            }
        }
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
