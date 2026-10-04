//! Arrival jitter: the measurement the adaptive playout offset reads
//! (docs/67 D15, docs/17 Decision 6). A port of the SPA's windowed min and
//! windowed quantile trackers (`gawk-app/src/transport/live-edge.ts`) and of
//! the slice of the reorder buffer that feeds them
//! (`gawk-app/src/transport/reorder-buffer.ts`, `insert` and
//! `arrivalJitterMs`).
//!
//! Every frame's arrival delta is `arrival_ms − timestamp_us / 1000`: the
//! (unknown) clock offset between broadcaster and viewer plus the true
//! capture → here latency. The windowed minimum is the session-best delta,
//! with the clock offset in it; `p95 − min` over the window is how much later
//! than that best the slow tail arrives, the offset cancelled. The minimum is
//! also the release-schedule anchor: a frame plays at
//! `timestamp + min + offset` (see [`crate::playout`]).
//!
//! Pure and clock-free like the TS: every call takes `now_ms`, the caller's
//! monotonic milliseconds, so the whole estimator is a unit test.

/// Sliding window both trackers cover. `LIVE_EDGE_WINDOW_MS` in
/// `live-edge.ts`, the default profile's `jitterWindowMs`. The window is what
/// absorbs crystal skew between the two clocks (tens of ppm): a session-long
/// minimum would silently drift.
pub const LIVE_EDGE_WINDOW_MS: f64 = 60_000.0;

/// Bucket width: memory is O(window / bucket) regardless of frame rate.
/// `LIVE_EDGE_BUCKET_MS` in `live-edge.ts`.
pub const LIVE_EDGE_BUCKET_MS: f64 = 1000.0;

/// Histogram bin width; quantiles are accurate to one bin.
/// `QUANTILE_BIN_MS` in `live-edge.ts`.
pub const QUANTILE_BIN_MS: f64 = 4.0;

/// Histogram range above each bucket's minimum. Values past it clamp into the
/// top bin, so this is the largest jitter the estimator can see; it must stay
/// at or above [`crate::playout::MAX_PLAYOUT_OFFSET_MS`] (`playout.ts`:
/// "Keep `quantileRangeMs >= maxMs` in every profile"). `QUANTILE_RANGE_MS`
/// in `live-edge.ts`.
pub const QUANTILE_RANGE_MS: f64 = 500.0;

/// The tail quantile jitter is read at: `arrivalJitterMs()` in
/// `reorder-buffer.ts` takes `quantile(0.95, …)`.
pub const ARRIVAL_JITTER_QUANTILE: f64 = 0.95;

#[derive(Debug, Clone, Copy)]
struct MinBucket {
    start: f64,
    min: f64,
}

/// Sliding-window minimum over a monotonic clock, bucketed so memory stays
/// O(window / bucket). `WindowedMinTracker` in `live-edge.ts`.
#[derive(Debug, Clone)]
pub struct WindowedMinTracker {
    window_ms: f64,
    bucket_ms: f64,
    buckets: std::collections::VecDeque<MinBucket>,
}

impl Default for WindowedMinTracker {
    fn default() -> Self {
        Self::new(LIVE_EDGE_WINDOW_MS, LIVE_EDGE_BUCKET_MS)
    }
}

impl WindowedMinTracker {
    pub fn new(window_ms: f64, bucket_ms: f64) -> Self {
        Self {
            window_ms,
            bucket_ms,
            buckets: std::collections::VecDeque::new(),
        }
    }

    pub fn observe(&mut self, value: f64, now_ms: f64) {
        self.evict(now_ms);
        let start = bucket_start(now_ms, self.bucket_ms);
        if let Some(last) = self.buckets.back_mut()
            && last.start == start
        {
            if value < last.min {
                last.min = value;
            }
            return;
        }
        self.buckets.push_back(MinBucket { start, min: value });
    }

    /// The minimum over the window; `None` when empty.
    pub fn min(&mut self, now_ms: f64) -> Option<f64> {
        self.evict(now_ms);
        self.buckets.iter().map(|b| b.min).reduce(f64::min)
    }

    pub fn reset(&mut self) {
        self.buckets.clear();
    }

    fn evict(&mut self, now_ms: f64) {
        let cutoff = now_ms - self.window_ms;
        while self
            .buckets
            .front()
            .is_some_and(|b| b.start + self.bucket_ms <= cutoff)
        {
            self.buckets.pop_front();
        }
    }
}

/// `Math.floor(nowMs / bucketMs) * bucketMs`, as both TS trackers bucket.
fn bucket_start(now_ms: f64, bucket_ms: f64) -> f64 {
    (now_ms / bucket_ms).floor() * bucket_ms
}

#[derive(Debug, Clone)]
struct QuantileBucket {
    start: f64,
    /// Lower edge of bin 0.
    min: f64,
    counts: Vec<u32>,
    total: u32,
}

/// Windowed quantile, the sibling of [`WindowedMinTracker`]. Each bucket holds
/// a fixed-width histogram of `value − bucket min`; quantiles are accurate to
/// one bin, and values past the range clamp into the top bin.
/// `WindowedQuantileTracker` in `live-edge.ts`.
#[derive(Debug, Clone)]
pub struct WindowedQuantileTracker {
    window_ms: f64,
    bucket_ms: f64,
    bin_ms: f64,
    num_bins: usize,
    buckets: std::collections::VecDeque<QuantileBucket>,
}

impl Default for WindowedQuantileTracker {
    fn default() -> Self {
        Self::new(
            LIVE_EDGE_WINDOW_MS,
            LIVE_EDGE_BUCKET_MS,
            QUANTILE_BIN_MS,
            QUANTILE_RANGE_MS,
        )
    }
}

impl WindowedQuantileTracker {
    pub fn new(window_ms: f64, bucket_ms: f64, bin_ms: f64, range_ms: f64) -> Self {
        Self {
            window_ms,
            bucket_ms,
            bin_ms,
            // +1: the clamp bin.
            num_bins: (range_ms / bin_ms).ceil() as usize + 1,
            buckets: std::collections::VecDeque::new(),
        }
    }

    pub fn observe(&mut self, value: f64, now_ms: f64) {
        self.evict(now_ms);
        let start = bucket_start(now_ms, self.bucket_ms);
        if self.buckets.back().is_none_or(|b| b.start != start) {
            self.buckets.push_back(QuantileBucket {
                start,
                min: value,
                counts: vec![0; self.num_bins],
                total: 0,
            });
        }
        let (bin_ms, num_bins) = (self.bin_ms, self.num_bins);
        let bucket = self.buckets.back_mut().expect("pushed above");
        if value < bucket.min {
            rebase(bucket, value, bin_ms, num_bins);
        }
        let idx = (((value - bucket.min) / bin_ms).floor() as usize).min(num_bins - 1);
        bucket.counts[idx] += 1;
        bucket.total += 1;
    }

    /// The `q`-th quantile's bin lower edge over the window; `None` when
    /// empty.
    pub fn quantile(&mut self, q: f64, now_ms: f64) -> Option<f64> {
        self.evict(now_ms);
        let mut total: u64 = 0;
        let mut bins: Vec<(f64, u32)> = Vec::new();
        for b in &self.buckets {
            total += u64::from(b.total);
            for (i, &count) in b.counts.iter().enumerate() {
                if count > 0 {
                    bins.push((b.min + i as f64 * self.bin_ms, count));
                }
            }
        }
        if total == 0 {
            return None;
        }
        bins.sort_by(|a, b| a.0.total_cmp(&b.0));
        let target_rank = ((q * total as f64).ceil() as u64).max(1);
        let mut cum: u64 = 0;
        for &(value, count) in &bins {
            cum += u64::from(count);
            if cum >= target_rank {
                return Some(value);
            }
        }
        bins.last().map(|&(value, _)| value)
    }

    pub fn reset(&mut self) {
        self.buckets.clear();
    }

    fn evict(&mut self, now_ms: f64) {
        let cutoff = now_ms - self.window_ms;
        while self
            .buckets
            .front()
            .is_some_and(|b| b.start + self.bucket_ms <= cutoff)
        {
            self.buckets.pop_front();
        }
    }
}

/// A value below the bucket's current minimum shifts bin 0 down (edges stay
/// on the original grid); counts pushed past the end merge into the clamp
/// bin. `rebase` in `live-edge.ts`.
fn rebase(bucket: &mut QuantileBucket, value: f64, bin_ms: f64, num_bins: usize) {
    let shift = ((bucket.min - value) / bin_ms).ceil();
    bucket.min -= shift * bin_ms;
    let shift = shift as usize;
    let mut counts = vec![0; num_bins];
    for (i, &count) in bucket.counts.iter().enumerate() {
        counts[i.saturating_add(shift).min(num_bins - 1)] += count;
    }
    bucket.counts = counts;
}

/// The arrival-jitter estimator a viewer owns: the reorder buffer's two
/// trackers (`arrivalBaseline`, `arrivalQuantile` in `reorder-buffer.ts`),
/// on the default profile's geometry.
#[derive(Debug, Clone, Default)]
pub struct ArrivalJitter {
    baseline: WindowedMinTracker,
    quantile: WindowedQuantileTracker,
}

impl ArrivalJitter {
    pub fn new() -> Self {
        Self::default()
    }

    /// Records one frame's arrival: its capture timestamp (µs, broadcaster
    /// clock, as on the wire) and when it arrived (`arrival_ms`, the
    /// caller's monotonic clock).
    pub fn record_arrival(&mut self, timestamp_us: u64, arrival_ms: f64) {
        let delta_ms = arrival_ms - timestamp_us as f64 / 1000.0;
        self.baseline.observe(delta_ms, arrival_ms);
        self.quantile.observe(delta_ms, arrival_ms);
    }

    /// Windowed `p95 − min` of the arrival delta, ms, never negative; `None`
    /// before any frame. `arrivalJitterMs` in `reorder-buffer.ts`.
    pub fn jitter_ms(&mut self, now_ms: f64) -> Option<f64> {
        let p95 = self.quantile.quantile(ARRIVAL_JITTER_QUANTILE, now_ms)?;
        let min = self.baseline.min(now_ms)?;
        Some((p95 - min).max(0.0))
    }

    /// The pacing anchor: the windowed minimum arrival delta, ms. `None`
    /// before any frame. `arrivalBaselineMs` in `reorder-buffer.ts`.
    pub fn baseline_ms(&mut self, now_ms: f64) -> Option<f64> {
        self.baseline.min(now_ms)
    }

    /// A broadcaster restart moves timestamps to a new timeline, against
    /// which the old deltas are meaningless. The caller pairs this with
    /// [`crate::playout::PlayoutController::reset`].
    pub fn reset(&mut self) {
        self.baseline.reset();
        self.quantile.reset();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // `live-edge.test.ts`, describe('WindowedMinTracker').

    #[test]
    fn min_tracks_the_minimum_across_observations() {
        let mut t = WindowedMinTracker::default();
        t.observe(50.0, 1000.0);
        t.observe(30.0, 2000.0);
        t.observe(80.0, 3000.0);
        assert_eq!(t.min(3000.0), Some(30.0));
    }

    #[test]
    fn min_ages_a_stale_minimum_out_of_the_window() {
        let mut t = WindowedMinTracker::new(10_000.0, 1000.0);
        t.observe(10.0, 0.0);
        t.observe(40.0, 5000.0);
        assert_eq!(t.min(5000.0), Some(10.0));
        // The bucket holding 10 ends at 1000; past 1000 + window it is gone.
        assert_eq!(t.min(11_500.0), Some(40.0));
    }

    #[test]
    fn min_is_none_when_empty_and_after_reset() {
        let mut t = WindowedMinTracker::default();
        assert_eq!(t.min(0.0), None);
        t.observe(5.0, 100.0);
        t.reset();
        assert_eq!(t.min(100.0), None);
    }

    // `live-edge.test.ts`, describe('WindowedQuantileTracker (R12 T1)').

    #[test]
    fn quantile_returns_the_requested_quantile_on_a_known_distribution() {
        // 1 ms bins make the quantile exact for integer values.
        let mut t = WindowedQuantileTracker::new(60_000.0, 1000.0, 1.0, 500.0);
        for v in 1..=100 {
            t.observe(f64::from(v), 500.0);
        }
        assert_eq!(t.quantile(0.95, 500.0), Some(95.0));
        assert_eq!(t.quantile(0.5, 500.0), Some(50.0));
        // q = 0 reads the smallest observed bin.
        assert_eq!(t.quantile(0.0, 500.0), Some(1.0));
    }

    #[test]
    fn quantile_is_none_when_empty_and_after_reset() {
        let mut t = WindowedQuantileTracker::default();
        assert_eq!(t.quantile(0.95, 0.0), None);
        t.observe(10.0, 100.0);
        t.reset();
        assert_eq!(t.quantile(0.95, 100.0), None);
    }

    #[test]
    fn quantile_ages_old_observations_out_of_the_window() {
        let mut t = WindowedQuantileTracker::new(10_000.0, 1000.0, 1.0, 500.0);
        t.observe(10.0, 0.0);
        t.observe(10.0, 0.0);
        t.observe(40.0, 5000.0);
        assert_eq!(t.quantile(0.0, 5000.0), Some(10.0));
        // The bucket holding the 10s ends at 1000; past 1000 + window it is
        // gone.
        assert_eq!(t.quantile(0.0, 11_500.0), Some(40.0));
        assert_eq!(t.quantile(1.0, 11_500.0), Some(40.0));
    }

    #[test]
    fn quantile_rebases_a_bucket_when_a_smaller_value_arrives_after_its_first() {
        let mut t =
            WindowedQuantileTracker::new(60_000.0, 1000.0, QUANTILE_BIN_MS, QUANTILE_RANGE_MS);
        t.observe(50.0, 100.0);
        t.observe(30.0, 200.0); // same bucket, below the initial min: rebase
        assert_eq!(t.quantile(0.0, 200.0), Some(30.0));
        // 50 stays counted (bin lower edge).
        assert!(t.quantile(1.0, 200.0).unwrap() >= 46.0);
    }

    #[test]
    fn quantile_clamps_far_outliers_into_the_top_bin() {
        let mut t =
            WindowedQuantileTracker::new(60_000.0, 1000.0, QUANTILE_BIN_MS, QUANTILE_RANGE_MS);
        t.observe(0.0, 100.0);
        t.observe(10_000.0, 200.0); // way past the range: top bin
        assert_eq!(t.quantile(1.0, 200.0), Some(QUANTILE_RANGE_MS));
    }

    // `reorder-buffer.test.ts`, describe('ReorderBuffer arrival jitter (R12
    // T1)'). The TS pushes frames through the reorder buffer under a fake
    // clock; here the same arrivals go straight to the estimator. `ts_us`
    // restates the TS helpers' `BigInt(Math.round(tsMs * 1000))`.

    fn ts_us(ts_ms: f64) -> u64 {
        (ts_ms * 1000.0).round() as u64
    }

    #[test]
    fn arrival_jitter_is_none_before_any_frame() {
        let mut j = ArrivalJitter::new();
        assert_eq!(j.jitter_ms(1000.0), None);
        assert_eq!(j.baseline_ms(1000.0), None);
    }

    #[test]
    fn arrival_jitter_reads_about_zero_for_a_perfectly_steady_delta() {
        let mut j = ArrivalJitter::new();
        j.record_arrival(ts_us(0.0), 1000.0); // delta 1000
        let mut now = 1000.0;
        for i in 1..=10 {
            now = 1000.0 + f64::from(i) * 1000.0;
            j.record_arrival(ts_us(f64::from(i) * 1000.0), now); // delta 1000, every time
        }
        assert!(j.jitter_ms(now).unwrap() < QUANTILE_BIN_MS);
        assert_eq!(j.baseline_ms(now), Some(1000.0));
    }

    #[test]
    fn arrival_jitter_reads_the_late_tail_as_p95_minus_min() {
        let mut j = ArrivalJitter::new();
        j.record_arrival(ts_us(0.0), 1000.0);
        let mut now = 1000.0;
        for i in 1..=9 {
            now = 1000.0 + f64::from(i) * 1000.0;
            j.record_arrival(ts_us(f64::from(i) * 1000.0), now); // 10 samples at delta 1000
        }
        for i in 10..=11 {
            now = 1100.0 + f64::from(i) * 1000.0;
            j.record_arrival(ts_us(f64::from(i) * 1000.0), now); // 2 samples 100 ms late
        }
        let jitter = j.jitter_ms(now).expect("jitter after frames");
        assert!(jitter >= 100.0 - QUANTILE_BIN_MS, "{jitter}");
        assert!(jitter <= 100.0 + QUANTILE_BIN_MS, "{jitter}");
    }

    #[test]
    fn arrival_jitter_resets_with_the_broadcaster_restart_signal() {
        let mut j = ArrivalJitter::new();
        j.record_arrival(ts_us(0.0), 1000.0);
        j.record_arrival(ts_us(1000.0), 2100.0); // delta 1100: some jitter on record
        // Restart: a fresh timestamp timeline with a completely different
        // arrival delta. The TS buffer resets itself on a serially-backwards
        // keyframe; here the caller owns that signal.
        j.reset();
        j.record_arrival(ts_us(5000.0), 20_000.0);
        let jitter = j.jitter_ms(20_000.0).expect("jitter after a fresh frame");
        assert!(jitter < QUANTILE_BIN_MS); // single fresh sample, old ones gone
    }

    // `reorder-buffer.test.ts`, describe('ReorderBuffer arrival jitter under
    // a deep stall (R19 PLAYOUT-1)'): the one case that applies to the
    // default profile, 'keeps the default profile on its 500 ms histogram'.
    #[test]
    fn arrival_jitter_saturates_at_the_default_histogram_range_under_a_stall() {
        const BASE_DELTA_MS: f64 = 1000.0;
        const FRAME_INTERVAL_MS: f64 = 1000.0 / 30.0;
        let mut j = ArrivalJitter::new();
        let mut capture_ms = 0.0;
        // The opening keyframe, then 20 frames, all on time.
        for _ in 0..21 {
            capture_ms += FRAME_INTERVAL_MS;
            j.record_arrival(ts_us(capture_ms), capture_ms + BASE_DELTA_MS);
        }
        // A 1400 ms stall: frames keep being captured but nothing is
        // delivered, then the backlog lands in one burst.
        let mut held = Vec::new();
        let mut held_ms = 0.0;
        while held_ms < 1400.0 {
            capture_ms += FRAME_INTERVAL_MS;
            held.push(capture_ms);
            held_ms += FRAME_INTERVAL_MS;
        }
        let now = capture_ms + BASE_DELTA_MS;
        for ts in held {
            j.record_arrival(ts_us(ts), now);
        }
        let jitter = j.jitter_ms(now).expect("jitter after frames");
        // The default profile never buffers past MAX_PLAYOUT_OFFSET_MS, so
        // the narrow histogram's saturation is by design here.
        assert!(jitter <= QUANTILE_RANGE_MS + QUANTILE_BIN_MS, "{jitter}");
    }
}
