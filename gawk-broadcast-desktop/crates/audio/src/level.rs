//! The live level meter and the silence latch behind D8's mode-1 hint:
//! "No audio from <App> yet…" appears when the captured app has stayed
//! silent for ~10 s while broadcasting, with the one-click whole-system
//! switch. Pure sample math, unit-tested here; the GUI just renders it.

/// Threshold below which a frame counts as silent (≈ -60 dBFS peak).
const SILENCE_PEAK: f32 = 0.001;
/// How long silence must persist before the hint latches.
pub const SILENCE_HINT_AFTER_US: u64 = 10_000_000;

#[derive(Debug, Default)]
pub struct LevelMeter {
    peak: f32,
    silent_since_us: Option<u64>,
    hint: bool,
}

impl LevelMeter {
    /// Feeds one frame of interleaved samples stamped `now_us`.
    pub fn observe(&mut self, interleaved: &[f32], now_us: u64) {
        let peak = interleaved.iter().fold(0f32, |m, s| m.max(s.abs()));
        // Fast attack, slow decay: the meter reads as live without
        // flickering to zero between quiet frames.
        self.peak = if peak > self.peak {
            peak
        } else {
            self.peak * 0.9
        };

        if peak < SILENCE_PEAK {
            let since = *self.silent_since_us.get_or_insert(now_us);
            if now_us.saturating_sub(since) >= SILENCE_HINT_AFTER_US {
                self.hint = true;
            }
        } else {
            self.silent_since_us = None;
            self.hint = false;
        }
    }

    /// Meter position, 0..=1 (already peak-held).
    pub fn level(&self) -> f32 {
        self.peak.min(1.0)
    }

    /// Whether the D8 "switch to whole-system audio?" hint should show.
    pub fn silence_hint(&self) -> bool {
        self.hint
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hint_latches_after_ten_silent_seconds_and_clears_on_sound() {
        let mut m = LevelMeter::default();
        let silent = vec![0.0f32; 1920];
        let loud = vec![0.5f32; 1920];

        m.observe(&silent, 0);
        assert!(!m.silence_hint());
        m.observe(&silent, 9_900_000);
        assert!(!m.silence_hint());
        m.observe(&silent, 10_000_000);
        assert!(m.silence_hint());

        // Sound clears the latch and restarts the clock.
        m.observe(&loud, 10_020_000);
        assert!(!m.silence_hint());
        assert!(m.level() > 0.4);
        m.observe(&silent, 10_040_000);
        m.observe(&silent, 19_000_000);
        assert!(!m.silence_hint());
    }
}
