//! The client side of TimeSync (0x05): the SPA's `transport/time-sync.ts`.
//!
//! Ping the relay every [`TIME_SYNC_INTERVAL_MS`]; from each echo take an
//! NTP-style sample mapping the local clock onto the relay's:
//! `rtt = t1 − t0`, `offset = server − (t0 + rtt/2)`, so
//! `relay_us ≈ local_us + offset_us`. The lowest-RTT sample of the last
//! [`TIME_SYNC_SAMPLE_WINDOW`] wins: queueing makes the out/back asymmetry
//! large, so the fastest exchange is the most symmetric one.

use gawk_wire::{TIME_SYNC_SIZE, TYPE_TIME_SYNC};

/// The SPA's `TIME_SYNC_INTERVAL_MS`.
pub const TIME_SYNC_INTERVAL_MS: f64 = 2000.0;
/// The SPA's `TIME_SYNC_SAMPLE_WINDOW`.
pub const TIME_SYNC_SAMPLE_WINDOW: usize = 8;

/// The winning sample.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TimeSyncSample {
    /// `relay_us ≈ local_us + offset_us`.
    pub offset_us: i64,
    pub rtt_us: u64,
}

/// See the module docs. Pure: the caller passes its clock in µs.
#[derive(Debug, Default)]
pub struct TimeSync {
    samples: Vec<TimeSyncSample>,
    next_ping_ms: Option<f64>,
}

impl TimeSync {
    pub fn new() -> Self {
        Self::default()
    }

    /// The ping datagram when one is due (the first at once on connect,
    /// then every [`TIME_SYNC_INTERVAL_MS`]).
    pub fn ping_due(&mut self, now_ms: f64) -> Option<Vec<u8>> {
        if self.next_ping_ms.is_some_and(|t| now_ms < t) {
            return None;
        }
        self.next_ping_ms = Some(now_ms + TIME_SYNC_INTERVAL_MS);
        let mut d = Vec::with_capacity(TIME_SYNC_SIZE);
        gawk_wire::append_time_sync(&mut d, (now_ms * 1000.0).round() as u64, 0);
        Some(d)
    }

    /// Consumes a TimeSync datagram (well-formed or not) and returns true;
    /// false means "not mine, route it on".
    pub fn handle(&mut self, dgram: &[u8], now_us: u64) -> bool {
        if dgram.len() < 2 || dgram[1] != TYPE_TIME_SYNC {
            return false;
        }
        if let Ok((t0, server)) = gawk_wire::parse_time_sync(dgram) {
            self.record(t0, server, now_us);
        }
        true
    }

    /// One exchange: sent at `t0_us`, echoed with `server_us`, back at
    /// `t1_us`.
    pub fn record(&mut self, t0_us: u64, server_us: u64, t1_us: u64) {
        if t1_us < t0_us {
            return; // impossible exchange: a bogus or forged echo
        }
        let rtt_us = t1_us - t0_us;
        let offset_us = server_us as i64 - (t0_us + rtt_us / 2) as i64;
        self.samples.push(TimeSyncSample { offset_us, rtt_us });
        if self.samples.len() > TIME_SYNC_SAMPLE_WINDOW {
            self.samples.remove(0);
        }
    }

    pub fn best(&self) -> Option<TimeSyncSample> {
        self.samples.iter().copied().min_by_key(|s| s.rtt_us)
    }

    /// A new session: new relay, new clock.
    pub fn reset(&mut self) {
        *self = Self::default();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn takes_the_lowest_rtt_sample_of_the_window() {
        let mut t = TimeSync::new();
        assert_eq!(t.best(), None);
        t.record(1_000, 50_000, 1_400); // rtt 400
        t.record(2_000, 51_000, 2_100); // rtt 100: wins
        t.record(3_000, 52_000, 3_900); // rtt 900
        let b = t.best().unwrap();
        assert_eq!(b.rtt_us, 100);
        assert_eq!(b.offset_us, 51_000 - (2_000 + 50));
    }

    #[test]
    fn forgets_samples_past_the_window() {
        let mut t = TimeSync::new();
        t.record(0, 0, 10); // the best, but oldest
        for i in 1..=TIME_SYNC_SAMPLE_WINDOW as u64 {
            t.record(i * 1000, 0, i * 1000 + 500);
        }
        assert_eq!(t.best().unwrap().rtt_us, 500);
    }

    #[test]
    fn rejects_an_impossible_exchange() {
        let mut t = TimeSync::new();
        t.record(2_000, 0, 1_000);
        assert_eq!(t.best(), None);
    }

    #[test]
    fn pings_at_once_then_on_the_interval() {
        let mut t = TimeSync::new();
        let first = t.ping_due(1000.0).unwrap();
        assert_eq!(gawk_wire::parse_time_sync(&first).unwrap(), (1_000_000, 0));
        assert!(t.ping_due(2999.0).is_none());
        assert!(t.ping_due(3000.0).is_some());
    }

    #[test]
    fn consumes_only_time_sync_datagrams() {
        let mut t = TimeSync::new();
        let mut echo = Vec::new();
        gawk_wire::append_time_sync(&mut echo, 1_000, 9_000);
        assert!(t.handle(&echo, 1_200));
        assert_eq!(t.best().unwrap().rtt_us, 200);
        assert!(t.handle(&echo[..5], 1_300), "malformed is still consumed");
        let mut count = Vec::new();
        gawk_wire::append_viewer_count(&mut count, 3);
        assert!(!t.handle(&count, 1_400));
    }
}
