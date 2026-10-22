use std::fmt::{Debug, Display, Formatter};

use super::min_max::MinMax;
use crate::{Duration, Instant};

#[derive(Clone, Debug, Default)]
pub(crate) struct BandwidthEstimation {
    total_acked: u64,
    prev_total_acked: u64,
    acked_time: Option<Instant>,
    prev_acked_time: Option<Instant>,
    total_sent: u64,
    prev_total_sent: u64,
    sent_time: Option<Instant>,
    prev_sent_time: Option<Instant>,
    max_filter: MinMax,
    acked_at_last_window: u64,
}

impl BandwidthEstimation {
    pub(crate) fn on_sent(&mut self, now: Instant, bytes: u64) {
        self.prev_total_sent = self.total_sent;
        self.total_sent += bytes;
        self.prev_sent_time = self.sent_time;
        self.sent_time = Some(now);
    }

    pub(crate) fn on_ack(
        &mut self,
        now: Instant,
        _sent: Instant,
        bytes: u64,
        round: u64,
        app_limited: bool,
    ) {
        self.prev_total_acked = self.total_acked;
        self.total_acked += bytes;
        self.prev_acked_time = self.acked_time;
        self.acked_time = Some(now);

        let prev_sent_time = match self.prev_sent_time {
            Some(prev_sent_time) => prev_sent_time,
            None => return,
        };

        let send_rate = match self.sent_time {
            Some(sent_time) if sent_time > prev_sent_time => Self::bw_from_delta(
                self.total_sent - self.prev_total_sent,
                sent_time - prev_sent_time,
            )
            .unwrap_or(0),
            _ => u64::MAX, // will take the min of send and ack, so this is just a skip
        };

        let ack_rate = match self.prev_acked_time {
            Some(prev_acked_time) => Self::bw_from_delta(
                self.total_acked - self.prev_total_acked,
                now - prev_acked_time,
            )
            .unwrap_or(0),
            None => 0,
        };

        let bandwidth = send_rate.min(ack_rate);
        // GAWK PATCH (docs/38 F-14): every sample goes to the windowed max
        // filter, which is what ages old maxima out. Upstream passed only
        // new maxima, so the estimate could never fall: one ack-compression
        // spike was the bandwidth for the rest of the connection. Zero is a
        // second ack at the same `now`, not a measurement.
        if !app_limited && bandwidth > 0 {
            self.max_filter.update_max(round, bandwidth);
        }
    }

    pub(crate) fn bytes_acked_this_window(&self) -> u64 {
        self.total_acked - self.acked_at_last_window
    }

    pub(crate) fn end_acks(&mut self, _current_round: u64, _app_limited: bool) {
        self.acked_at_last_window = self.total_acked;
    }

    pub(crate) fn get_estimate(&self) -> u64 {
        self.max_filter.get()
    }

    pub(crate) const fn bw_from_delta(bytes: u64, delta: Duration) -> Option<u64> {
        let window_duration_ns = delta.as_nanos();
        if window_duration_ns == 0 {
            return None;
        }
        let b_ns = bytes * 1_000_000_000;
        let bytes_per_second = b_ns / (window_duration_ns as u64);
        Some(bytes_per_second)
    }
}

impl Display for BandwidthEstimation {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{:.3} MB/s",
            self.get_estimate() as f32 / (1024 * 1024) as f32
        )
    }
}

// GAWK PATCH (docs/38 F-14): the max filter must see every sample, or a
// spike is the estimate forever. See vendor/quinn-proto/GAWK-PATCH.md.
#[cfg(test)]
mod gawk_tests {
    use super::*;

    const PACKET: u64 = 1200;

    /// One round as quinn produces it: a send batch shares one `now`
    /// (`drive_transmit`), so the estimate is the ack rate; its acks then
    /// arrive `ack_gap` apart.
    fn round(est: &mut BandwidthEstimation, t: &mut Instant, round: u64, ack_gap: Duration) {
        for _ in 0..10 {
            est.on_sent(*t, PACKET);
        }
        for _ in 0..10 {
            *t += ack_gap;
            est.on_ack(*t, *t, PACKET, round, false);
        }
        est.end_acks(round, false);
    }

    /// Two acks 10 µs apart: an ack-compression sample of 120 MB/s.
    fn spike(est: &mut BandwidthEstimation, t: &mut Instant, round: u64) {
        est.on_sent(*t, PACKET);
        est.on_sent(*t, PACKET);
        *t += Duration::from_millis(1);
        est.on_ack(*t, *t, PACKET, round, false);
        *t += Duration::from_micros(10);
        est.on_ack(*t, *t, PACKET, round, false);
        est.end_acks(round, false);
    }

    const STEADY: u64 = 1_200_000; // 1200 B per 1 ms

    #[test]
    fn a_spike_ages_out_of_the_window() {
        let mut est = BandwidthEstimation::default();
        let mut t = Instant::now();
        for r in 0..20 {
            round(&mut est, &mut t, r, Duration::from_millis(1));
        }
        assert_eq!(est.get_estimate(), STEADY);
        spike(&mut est, &mut t, 20);
        assert_eq!(est.get_estimate(), 100 * STEADY);
        for r in 21..41 {
            round(&mut est, &mut t, r, Duration::from_millis(1));
        }
        assert_eq!(est.get_estimate(), STEADY, "20 rounds on, past the 10-round window");
    }

    #[test]
    fn a_spike_holds_within_the_window() {
        let mut est = BandwidthEstimation::default();
        let mut t = Instant::now();
        for r in 0..20 {
            round(&mut est, &mut t, r, Duration::from_millis(1));
        }
        spike(&mut est, &mut t, 20);
        for r in 21..28 {
            round(&mut est, &mut t, r, Duration::from_millis(1));
        }
        assert_eq!(est.get_estimate(), 100 * STEADY, "a max filter, not a mean");
    }

    #[test]
    fn a_slower_path_lowers_the_estimate() {
        let mut est = BandwidthEstimation::default();
        let mut t = Instant::now();
        for r in 0..20 {
            round(&mut est, &mut t, r, Duration::from_millis(1));
        }
        for r in 20..40 {
            round(&mut est, &mut t, r, Duration::from_millis(4));
        }
        assert_eq!(est.get_estimate(), STEADY / 4);
    }
}
