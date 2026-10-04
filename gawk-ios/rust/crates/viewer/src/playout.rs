//! The adaptive playout offset (docs/67 D15, docs/17 Decision 6): how far
//! behind the live edge the player runs. A port of the SPA's
//! `PlayoutController` (`gawk-app/src/transport/playout.ts`) on its default
//! ("balanced") profile. The resilient and DVR profiles wait for R19
//! delivery in the viewer (docs/67 §5).
//!
//! Target = `clamp(arrival jitter + HEADROOM_MS, MIN, MAX)`, where jitter is
//! [`crate::jitter::ArrivalJitter`]'s `p95 − min`. The offset slews toward
//! the target asymmetrically: up fast, because under-buffering drops frames
//! now; down slowly, and only after the target has sat well below the
//! current value for a dwell period. Slew, never step: on iOS the slew is a
//! synchronizer rate a fraction of a percent off 1.0, which is invisible,
//! where a step would be a skip (D15). A constant offset is a rejected design
//! (docs/12 Decision 7, docs/17 Decision 10).
//!
//! Pure and clock-free like the TS: time arrives as the caller's monotonic
//! milliseconds. The caller's loop is the SPA's (`viewer.ts`
//! `publishStats`): record every frame's arrival, and every
//! [`PLAYOUT_UPDATE_INTERVAL_MS`] feed `jitter_ms(now)` to
//! [`PlayoutController::update`]. A frame then plays at
//! [`display_target_ms`].

/// Seed offset until the controller has [`OFFSET_WARMUP_MS`] of jitter to go
/// on: ~9 frames at 60 fps. `PLAYOUT_OFFSET_MS` in `playout.ts`.
pub const PLAYOUT_OFFSET_MS: f64 = 150.0;

/// How long before its display target a frame is released to the decoder,
/// so the decoded frame reaches the renderer just in time: ~1 frame interval
/// at 30 fps. `DECODE_LEAD_MS` in `playout.ts`; used by
/// [`release_at_ms`].
pub const DECODE_LEAD_MS: f64 = 35.0;

/// Added over the jitter: one 30 fps interval over the p95. `HEADROOM_MS`
/// in `playout.ts`.
pub const HEADROOM_MS: f64 = 34.0;

/// The offset's floor, and the fixed offset of the lowest-latency preset
/// (D15 Presets). `MIN_PLAYOUT_OFFSET_MS` in `playout.ts`.
pub const MIN_PLAYOUT_OFFSET_MS: f64 = 50.0;

/// The offset's ceiling. `MAX_PLAYOUT_OFFSET_MS` in `playout.ts`; it must
/// stay at or below [`crate::jitter::QUANTILE_RANGE_MS`], or the clamp is
/// dead envelope the histogram cannot express.
pub const MAX_PLAYOUT_OFFSET_MS: f64 = 350.0;

// `playout.ts`: "Keep `quantileRangeMs >= maxMs` in every profile". Checked
// at build time so a retuned constant can't silently cap the offset.
const _: () = assert!(crate::jitter::QUANTILE_RANGE_MS >= MAX_PLAYOUT_OFFSET_MS);

/// Up-slew rate, ms of offset per second. `OFFSET_SLEW_UP_MS_PER_S` in
/// `playout.ts`.
pub const OFFSET_SLEW_UP_MS_PER_S: f64 = 50.0;

/// Down-slew rate, ms of offset per second. `OFFSET_SLEW_DOWN_MS_PER_S` in
/// `playout.ts`.
pub const OFFSET_SLEW_DOWN_MS_PER_S: f64 = 5.0;

/// The target must sit this far below the offset to arm a descent.
/// `OFFSET_DOWN_MARGIN_MS` in `playout.ts`.
pub const OFFSET_DOWN_MARGIN_MS: f64 = 30.0;

/// How long an armed descent waits before the offset starts falling.
/// `OFFSET_DOWN_DWELL_MS` in `playout.ts`.
pub const OFFSET_DOWN_DWELL_MS: f64 = 15_000.0;

/// The seed holds this long after the first jitter reading, while the window
/// fills. `OFFSET_WARMUP_MS` in `playout.ts`.
pub const OFFSET_WARMUP_MS: f64 = 5000.0;

/// How often the SPA reads the jitter and updates the controller: its stats
/// tick (`setInterval(publishStats, 500)`, `viewer.ts`). The slew is rate ×
/// dt, so the exact cadence doesn't change the trajectory.
pub const PLAYOUT_UPDATE_INTERVAL_MS: f64 = 500.0;

/// The SPA's two non-reconnecting playout presets (docs/37, docs/67 D15
/// Presets).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum PlayoutPreset {
    /// The adaptive offset. The default (D15).
    #[default]
    Balanced,
    /// The SPA's `off`: video displays immediately and audio is scheduled
    /// at the fixed minimum offset, [`MIN_PLAYOUT_OFFSET_MS`].
    LowestLatency,
}

impl PlayoutPreset {
    /// The offset this preset plays at, ms: the controller's for
    /// [`PlayoutPreset::Balanced`], the fixed minimum otherwise.
    pub fn offset_ms(self, controller: &PlayoutController) -> f64 {
        match self {
            Self::Balanced => controller.offset_ms(),
            Self::LowestLatency => MIN_PLAYOUT_OFFSET_MS,
        }
    }
}

/// The adaptive offset controller. `PlayoutController` in `playout.ts`, on
/// `DEFAULT_PLAYOUT_PROFILE` (whose `stepUpAboveMs` is infinite, so a rise
/// always slews).
#[derive(Debug, Clone)]
pub struct PlayoutController {
    current: f64,
    first_jitter_at: Option<f64>,
    last_update_at: Option<f64>,
    below_since: Option<f64>,
}

impl Default for PlayoutController {
    fn default() -> Self {
        Self::new()
    }
}

impl PlayoutController {
    pub fn new() -> Self {
        Self {
            current: PLAYOUT_OFFSET_MS,
            first_jitter_at: None,
            last_update_at: None,
            below_since: None,
        }
    }

    /// Feeds the current arrival jitter (`None`: no data yet, which holds the
    /// offset) at `now_ms`. The cadence is the caller's; the slew is
    /// rate × dt, so it doesn't change the trajectory.
    pub fn update(&mut self, jitter_ms: Option<f64>, now_ms: f64) {
        let dt_ms = self
            .last_update_at
            .map_or(0.0, |last| (now_ms - last).max(0.0));
        self.last_update_at = Some(now_ms);
        let Some(jitter_ms) = jitter_ms else {
            return;
        };
        let first = *self.first_jitter_at.get_or_insert(now_ms);
        if now_ms - first < OFFSET_WARMUP_MS {
            return;
        }

        let target = (jitter_ms + HEADROOM_MS).clamp(MIN_PLAYOUT_OFFSET_MS, MAX_PLAYOUT_OFFSET_MS);
        if target > self.current {
            self.below_since = None;
            self.current = target.min(self.current + OFFSET_SLEW_UP_MS_PER_S * dt_ms / 1000.0);
        } else if target < self.current {
            // Arm a descent only on a clear gap; once armed, keep descending
            // all the way to the target even as the gap narrows below the
            // margin, or the offset would floor at target + margin forever
            // (docs/17 Decision 6).
            if self.below_since.is_none() && target < self.current - OFFSET_DOWN_MARGIN_MS {
                self.below_since = Some(now_ms);
            }
            if self
                .below_since
                .is_some_and(|since| now_ms - since >= OFFSET_DOWN_DWELL_MS)
            {
                self.current =
                    target.max(self.current - OFFSET_SLEW_DOWN_MS_PER_S * dt_ms / 1000.0);
            }
        } else {
            self.below_since = None;
        }
    }

    /// The current offset, ms.
    pub fn offset_ms(&self) -> f64 {
        self.current
    }

    /// Broadcaster restart: a new timestamp timeline and stale jitter.
    /// Re-seeds and re-arms the warmup.
    pub fn reset(&mut self) {
        *self = Self::new();
    }
}

/// When a frame should display, on the caller's clock: its capture timestamp
/// plus the arrival baseline ([`crate::jitter::ArrivalJitter::baseline_ms`])
/// plus the offset. `displayTargetMs` in `viewer.ts`.
pub fn display_target_ms(timestamp_us: u64, baseline_ms: f64, offset_ms: f64) -> f64 {
    timestamp_us as f64 / 1000.0 + baseline_ms + offset_ms
}

/// When a frame should be released to the decoder: [`DECODE_LEAD_MS`] before
/// its display target. `releasableAt` in `reorder-buffer.ts`.
pub fn release_at_ms(timestamp_us: u64, baseline_ms: f64, offset_ms: f64) -> f64 {
    display_target_ms(timestamp_us, baseline_ms, offset_ms) - DECODE_LEAD_MS
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::jitter::ArrivalJitter;

    // `playout.test.ts`, describe('playout modes (R12 T2)'), the cases that
    // apply to a controller the caller owns.

    #[test]
    fn names_the_decode_lead_beside_the_offsets() {
        const { assert!(DECODE_LEAD_MS > 0.0) };
        const { assert!(DECODE_LEAD_MS < PLAYOUT_OFFSET_MS) };
    }

    #[test]
    fn a_new_controller_starts_at_the_seed_offset() {
        assert_eq!(PlayoutController::new().offset_ms(), PLAYOUT_OFFSET_MS);
    }

    // `playout.test.ts`, describe('PlayoutController (R12 T3)').

    /// Drives `update` once per second from t = 0. `warmedController` in
    /// `playout.test.ts`.
    fn warmed_controller(jitter_ms: f64, seconds: u32) -> PlayoutController {
        let mut c = PlayoutController::new();
        for s in 0..=seconds {
            c.update(Some(jitter_ms), f64::from(s) * 1000.0);
        }
        c
    }

    #[test]
    fn holds_the_seed_through_the_warmup_window() {
        let mut c = PlayoutController::new();
        let mut t = 0.0;
        while t < OFFSET_WARMUP_MS {
            c.update(Some(300.0), t);
            assert_eq!(c.offset_ms(), PLAYOUT_OFFSET_MS);
            t += 1000.0;
        }
    }

    #[test]
    fn converges_to_jitter_plus_headroom_on_a_jittery_link() {
        let c = warmed_controller(300.0, 20);
        assert_eq!(c.offset_ms(), 300.0 + HEADROOM_MS);
    }

    #[test]
    fn clamps_at_both_bounds() {
        assert_eq!(
            warmed_controller(1000.0, 30).offset_ms(),
            MAX_PLAYOUT_OFFSET_MS
        );
        // A clean link wants ~headroom only, but never below the floor.
        // Reaching the floor takes the dwell plus a long slew-down; drive it
        // far enough.
        let mut c = PlayoutController::new();
        let mut t = 0.0;
        while t <= OFFSET_WARMUP_MS + (OFFSET_DOWN_DWELL_MS + 60_000.0) {
            c.update(Some(0.0), t);
            t += 1000.0;
        }
        assert_eq!(c.offset_ms(), MIN_PLAYOUT_OFFSET_MS);
    }

    #[test]
    fn rises_fast_but_never_faster_than_the_up_slew_rate() {
        let mut c = PlayoutController::new();
        let mut t = 0.0;
        while t <= OFFSET_WARMUP_MS {
            c.update(Some(0.0), t);
            t += 1000.0;
        }
        let mut prev = c.offset_ms();
        for _ in 0..5 {
            c.update(Some(300.0), t); // contiguous 1 s updates: the bound is rate × 1 s
            let step = c.offset_ms() - prev;
            assert!(step <= OFFSET_SLEW_UP_MS_PER_S + 1e-9, "{step}");
            prev = c.offset_ms();
            t += 1000.0;
        }
        assert!(c.offset_ms() > PLAYOUT_OFFSET_MS);
    }

    #[test]
    fn decreases_only_after_the_dwell_period_then_slowly() {
        // Converge high first.
        let mut c = warmed_controller(300.0, 20);
        let high = c.offset_ms();
        let mut t = 21_000.0;
        // Jitter vanishes: the offset must hold through the dwell…
        let dwell_end = t + OFFSET_DOWN_DWELL_MS;
        while t < dwell_end {
            c.update(Some(0.0), t);
            assert_eq!(c.offset_ms(), high);
            t += 1000.0;
        }
        // …then descend, bounded by the down-slew rate.
        let mut prev = c.offset_ms();
        for _ in 0..5 {
            c.update(Some(0.0), t);
            let step = prev - c.offset_ms();
            assert!(step > 0.0, "{step}");
            assert!(step <= OFFSET_SLEW_DOWN_MS_PER_S + 1e-9, "{step}");
            prev = c.offset_ms();
            t += 1000.0;
        }
    }

    #[test]
    fn a_jitter_spike_mid_descent_flips_straight_back_to_rising() {
        let mut c = warmed_controller(300.0, 20);
        let mut t = 21_000.0;
        while t < 21_000.0 + OFFSET_DOWN_DWELL_MS + 3000.0 {
            c.update(Some(0.0), t);
            t += 1000.0;
        }
        let mid_descent = c.offset_ms();
        assert!(mid_descent < 300.0 + HEADROOM_MS); // it really is descending
        c.update(Some(300.0), t);
        assert!(c.offset_ms() >= mid_descent);
    }

    #[test]
    fn reset_re_seeds_and_re_arms_the_warmup() {
        let mut c = warmed_controller(300.0, 20);
        c.reset();
        assert_eq!(c.offset_ms(), PLAYOUT_OFFSET_MS);
        c.update(Some(300.0), 100_000.0);
        assert_eq!(c.offset_ms(), PLAYOUT_OFFSET_MS); // warming up again
    }

    #[test]
    fn no_jitter_yet_keeps_the_current_offset() {
        let mut c = warmed_controller(300.0, 20);
        let v = c.offset_ms();
        c.update(None, 30_000.0);
        assert_eq!(c.offset_ms(), v);
    }

    // `playout.test.ts`, describe('adaptive mode wiring (R12 T3)'), 'tracks
    // the controller in adaptive mode only', restated for D15's presets: the
    // SPA's `off` plays at 0, the iOS lowest-latency preset at the floor.

    #[test]
    fn balanced_tracks_the_controller_and_lowest_latency_holds_the_floor() {
        let mut c = PlayoutController::new();
        let mut t = 0.0;
        while t <= 30_000.0 {
            c.update(Some(250.0), t);
            t += 1000.0;
        }
        assert_eq!(PlayoutPreset::default(), PlayoutPreset::Balanced);
        assert_eq!(PlayoutPreset::Balanced.offset_ms(&c), 250.0 + HEADROOM_MS);
        assert_eq!(
            PlayoutPreset::LowestLatency.offset_ms(&c),
            MIN_PLAYOUT_OFFSET_MS
        );
    }

    // `reorder-buffer.ts` `releasableAt` and `viewer.ts` `displayTargetMs`.

    #[test]
    fn schedules_display_at_timestamp_plus_baseline_plus_offset() {
        // Captured at 9 800 ms, best-observed arrival delta 200 ms, offset
        // 84 ms: displays at 10 084 ms, released to decode 35 ms earlier.
        assert_eq!(display_target_ms(9_800_000, 200.0, 84.0), 10_084.0);
        assert_eq!(
            release_at_ms(9_800_000, 200.0, 84.0),
            10_084.0 - DECODE_LEAD_MS
        );
    }

    // The loop the SPA runs (`viewer.ts` `publishStats`): every arrival is
    // recorded, and every PLAYOUT_UPDATE_INTERVAL_MS the jitter is read and
    // fed to the controller. A steady 60 fps link with 20 ms of late tail
    // lands the offset on jitter + headroom, within a bin, once the warmup,
    // the dwell and the slow descent from the seed (~40 s) have run.
    #[test]
    fn the_update_loop_converges_from_measured_arrivals() {
        let mut jitter = ArrivalJitter::new();
        let mut c = PlayoutController::new();
        let mut next_update = 0.0;
        for frame in 0..(60 * 60) {
            let ts_ms = f64::from(frame) * 1000.0 / 60.0;
            // Every tenth frame is 20 ms late; the rest arrive 1 s after
            // capture on the viewer's clock.
            let late = if frame % 10 == 0 { 20.0 } else { 0.0 };
            let now = ts_ms + 1000.0 + late;
            jitter.record_arrival((ts_ms * 1000.0).round() as u64, now);
            if now >= next_update {
                c.update(jitter.jitter_ms(now), now);
                next_update += PLAYOUT_UPDATE_INTERVAL_MS;
            }
        }
        let expected = 20.0 + HEADROOM_MS;
        assert!((c.offset_ms() - expected).abs() <= 4.0, "{}", c.offset_ms());
    }
}
