//! How fast the uplink drains a keyframe, against the stream's own rate
//! (R75, docs/85 D1–D3). Pure arithmetic over a keyframe's size, how long
//! its reliable stream took to be acknowledged, and the connection's
//! cumulative wire bytes, so the whole rule is unit tests.
//!
//! A keyframe is the one burst the stream sends often: tens of kilobytes,
//! twice a second, on a reliable stream whose `finish` resolves only once
//! every byte is acknowledged (`transport::WtKeyframeWriter`). The rate it
//! drained at — its bits over the time to acknowledgement less one minimum
//! round trip — is what the path delivered while it carried the stream, so
//! it is a bandwidth measurement taken by the stream itself. quinn's own
//! estimates cannot stand in for it (docs/85 §2): BBR's bottleneck estimate
//! updates only on samples that were not application-limited, which a video
//! stream almost never produces, so on a clean 30 Mbps uplink it read
//! 6.8 Mbps under an 11.6 Mbps stream (and 0 for a whole run); Cubic's
//! window over RTT grows without bound while the stream is app-limited.
//!
//! A keyframe is **slow** when it drained at under [`SLOW_DRAIN_SHARE`] of
//! the stream's wire rate over the last [`RATE_WINDOW_US`]. Radio outages
//! and random loss stretch a keyframe now and then; a full uplink stretches
//! every one (docs/85 §2), which is what the monitor (`uplink`) looks for.

use std::collections::VecDeque;

use crate::relay::PathCounters;

/// A keyframe is slow when it drained at under this share of the stream's
/// wire rate (docs/85 D3, tuned on §5's matrix).
pub const SLOW_DRAIN_SHARE: f64 = 0.85;
/// The stream's wire rate is averaged over this much of the recent past.
pub const RATE_WINDOW_US: u64 = 5_000_000;
/// …and is unknown until the samples span at least this much.
pub const MIN_RATE_SPAN_US: u64 = 1_000_000;

/// The stream's recent wire rate, sampled from the connection's cumulative
/// `wire_bytes` whenever a keyframe is acknowledged.
#[derive(Debug, Default)]
pub struct KeyframeDrain {
    samples: VecDeque<(u64, u64)>,
}

impl KeyframeDrain {
    pub fn new() -> Self {
        Self::default()
    }

    /// A new connection (a resume): its counters restart, so its rate does.
    pub fn reset(&mut self) {
        self.samples.clear();
    }

    /// One acknowledged keyframe: `bytes` long, acknowledged `elapsed_us`
    /// after its write began, at `now_us`, with the connection's counters as
    /// read then. `Some(true)` when it drained slowly, `Some(false)` when it
    /// did not, `None` when there is nothing to judge it against yet (no
    /// counters, or under [`MIN_RATE_SPAN_US`] of rate history).
    pub fn observe(
        &mut self,
        now_us: u64,
        bytes: u64,
        elapsed_us: u64,
        counters: Option<PathCounters>,
    ) -> Option<bool> {
        let c = counters?;
        if self.samples.back().is_some_and(|&(_, b)| c.wire_bytes < b) {
            self.samples.clear(); // a counter that went backwards: a new connection
        }
        self.samples.push_back((now_us, c.wire_bytes));
        while self
            .samples
            .front()
            .is_some_and(|&(t, _)| now_us.saturating_sub(t) > RATE_WINDOW_US)
        {
            self.samples.pop_front();
        }
        let (t0, b0) = *self.samples.front()?;
        let span_us = now_us.saturating_sub(t0);
        if span_us < MIN_RATE_SPAN_US {
            return None;
        }
        let rate_bps = (c.wire_bytes - b0) as f64 * 8.0 * 1e6 / span_us as f64;
        let drain_us = elapsed_us.saturating_sub(c.min_rtt_us);
        if drain_us == 0 {
            return Some(false); // inside one round trip: as fast as the path goes
        }
        let drain_bps = bytes as f64 * 8.0 * 1e6 / drain_us as f64;
        Some(drain_bps < SLOW_DRAIN_SHARE * rate_bps)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn counters(wire_bytes: u64, min_rtt_us: u64) -> Option<PathCounters> {
        Some(PathCounters {
            wire_bytes,
            min_rtt_us,
            ..Default::default()
        })
    }

    /// A 12 Mbps stream (1.5 MB/s) sampled every 500 ms, then one keyframe
    /// judged: `kf_bytes` acknowledged `elapsed_ms` after its write began.
    fn judge(kf_bytes: u64, elapsed_ms: u64, min_rtt_ms: u64) -> Option<bool> {
        let mut d = KeyframeDrain::new();
        let mut wire = 0;
        for i in 0..4 {
            assert_eq!(
                d.observe(i * 500_000, 0, 0, counters(wire, min_rtt_ms * 1000)),
                if i >= 2 { Some(false) } else { None }
            );
            wire += 750_000;
        }
        d.observe(
            2_000_000,
            kf_bytes,
            elapsed_ms * 1000,
            counters(wire, min_rtt_ms * 1000),
        )
    }

    #[test]
    fn a_keyframe_draining_under_85_percent_of_the_stream_is_slow() {
        // 75 kB is 0.6 Mbit: at the stream's 12 Mbps it drains in 50 ms, at
        // 10.2 Mbps (the threshold) in 58.8 ms. 10 ms of minimum RTT on top.
        assert_eq!(judge(75_000, 60, 10), Some(false), "12 Mbps");
        assert_eq!(judge(75_000, 68, 10), Some(false), "10.3 Mbps");
        assert_eq!(judge(75_000, 70, 10), Some(true), "10.0 Mbps");
        assert_eq!(judge(75_000, 310, 10), Some(true), "2 Mbps");
    }

    #[test]
    fn one_round_trip_is_not_drain_time() {
        // A 100 ms path: the same keyframe at the same rate takes 150 ms.
        assert_eq!(judge(75_000, 150, 100), Some(false));
        // Acknowledged within the minimum RTT (an RTT that dropped since):
        // as fast as it gets.
        assert_eq!(judge(75_000, 90, 100), Some(false));
    }

    #[test]
    fn nothing_is_judged_without_a_rate() {
        let mut d = KeyframeDrain::new();
        assert_eq!(d.observe(0, 75_000, 1_000_000, None), None, "no counters");
        assert_eq!(d.observe(0, 75_000, 1_000_000, counters(0, 0)), None);
        assert_eq!(
            d.observe(900_000, 75_000, 1_000_000, counters(1_000_000, 0)),
            None,
            "under a second of history"
        );
        assert_eq!(
            d.observe(1_000_000, 75_000, 1_000_000, counters(1_500_000, 0)),
            Some(true),
            "12 Mbps over a second against 0.6 Mbps"
        );
    }

    #[test]
    fn the_rate_is_the_recent_one() {
        let mut d = KeyframeDrain::new();
        // 60 s at 1.2 Mbps, then 5 s at 12 Mbps: the window has forgotten
        // the slow minute, so a keyframe at 6 Mbps is slow against 12.
        let mut wire = 0;
        for i in 0..=120 {
            d.observe(i * 500_000, 0, 0, counters(wire, 0));
            wire += 75_000;
        }
        let t = 120 * 500_000;
        for i in 1..=10 {
            wire += 750_000;
            d.observe(t + i * 500_000, 0, 0, counters(wire, 0));
        }
        assert_eq!(
            d.observe(t + 5_000_000, 75_000, 100_000, counters(wire, 0)),
            Some(true)
        );
    }

    #[test]
    fn a_new_connection_starts_its_rate_afresh() {
        let mut d = KeyframeDrain::new();
        for i in 0..4 {
            d.observe(i * 500_000, 0, 0, counters(10_000_000 + i * 750_000, 0));
        }
        // The resume's counters restart near zero: no rate until a second
        // of the new connection's history, never a negative one.
        assert_eq!(
            d.observe(2_000_000, 75_000, 1_000, counters(1_000, 0)),
            None
        );
        assert_eq!(
            d.observe(3_000_000, 75_000, 1_000_000, counters(1_501_000, 0)),
            Some(true)
        );
        d.reset();
        assert_eq!(
            d.observe(3_500_000, 75_000, 1_000, counters(2_000_000, 0)),
            None
        );
    }
}
