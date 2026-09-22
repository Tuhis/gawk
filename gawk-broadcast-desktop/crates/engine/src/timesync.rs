//! The TimeSync client and ClockMapping cadence, ported from
//! gawk-broadcast/internal/engine/timesync.go (itself the mirror of
//! gawk-app's time-sync.ts). NTP-style: rtt = t1−t0, offset = server −
//! (t0 + rtt/2); the lowest-RTT sample in a rolling window wins, because the
//! fastest exchange is the most symmetric one.

use crate::clock::Clock;
use std::sync::{Arc, Mutex};

/// Ping cadence. A CONSTANT, not a knob: the relay rate-caps TimeSync
/// replies at 5/s per session, and a knob would let a user silently
/// configure their own measurements into being dropped.
pub const TIME_SYNC_INTERVAL_MS: u64 = 2000;
/// Rolling sample count (TS: TIME_SYNC_SAMPLE_WINDOW).
pub const TIME_SYNC_SAMPLE_WINDOW: usize = 8;
/// ClockMapping re-publish cadence (TS: CLOCK_MAPPING_INTERVAL_MS).
pub const CLOCK_MAPPING_INTERVAL_MS: u64 = 5000;

/// The winning sample: relayClockUs ≈ localClockUs + offset_us.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct TimeSyncStats {
    pub offset_us: i64,
    pub rtt_ms: f64,
}

#[derive(Debug, Clone, Copy)]
struct Sample {
    offset_us: i64,
    rtt_us: u64,
}

/// The pure half: samples in, best out. Mirrors the Go/TS estimators so the
/// implementations can be compared on identical sample sequences.
#[derive(Debug, Default)]
pub struct TimeSyncEstimator {
    samples: Vec<Sample>,
}

impl TimeSyncEstimator {
    /// Records one exchange: t0 = sent, server = the relay's echoed clock,
    /// t1 = reply landed. An impossible exchange (t1 < t0) is discarded.
    pub fn record(&mut self, t0_us: u64, server_time_us: u64, t1_us: u64) {
        if t1_us < t0_us {
            return;
        }
        let rtt_us = t1_us - t0_us;
        let offset_us = server_time_us as i64 - (t0_us + rtt_us / 2) as i64;
        self.samples.push(Sample { offset_us, rtt_us });
        if self.samples.len() > TIME_SYNC_SAMPLE_WINDOW {
            self.samples.remove(0);
        }
    }

    /// The lowest-RTT sample in the window, if any.
    pub fn best(&self) -> Option<TimeSyncStats> {
        self.samples
            .iter()
            .min_by_key(|s| s.rtt_us)
            .map(|s| TimeSyncStats {
                offset_us: s.offset_us,
                rtt_ms: s.rtt_us as f64 / 1000.0,
            })
    }
}

/// How the client puts a ping on the wire (datagram send, failures
/// swallowed by the closure).
pub type SendFn = Box<dyn Fn(&[u8]) + Send + Sync>;

/// Owns the estimator and the ping encoding; the session drives the cadence
/// and feeds every received datagram through `handle_datagram`.
pub struct TimeSyncClient {
    send: SendFn,
    clock: Arc<dyn Clock>,
    est: Mutex<TimeSyncEstimator>,
}

impl TimeSyncClient {
    /// `send` failures are the sender's to swallow: a ping must never take
    /// the pipeline down — the session's own lifecycle owns that.
    pub fn new(send: SendFn, clock: Arc<dyn Clock>) -> Self {
        Self {
            send,
            clock,
            est: Mutex::new(TimeSyncEstimator::default()),
        }
    }

    /// Sends one TimeSync request (server field zero).
    pub fn ping(&self) {
        let mut dgram = Vec::with_capacity(gawk_wire::TIME_SYNC_SIZE);
        gawk_wire::append_time_sync(&mut dgram, self.clock.now_us(), 0);
        (self.send)(&dgram);
    }

    /// Reports whether the datagram was a TimeSync message. True means
    /// "consumed here" — well-formed or not — so it never reaches the video
    /// path; malformed ones are dropped (strict parsing).
    pub fn handle_datagram(&self, dgram: &[u8]) -> bool {
        if dgram.len() < 2 || dgram[1] != gawk_wire::TYPE_TIME_SYNC {
            return false;
        }
        if dgram.len() == gawk_wire::TIME_SYNC_SIZE
            && let Ok((t0, server)) = gawk_wire::parse_time_sync(dgram)
        {
            self.est
                .lock()
                .unwrap()
                .record(t0, server, self.clock.now_us());
        }
        true
    }

    /// Discards every sample. MANDATORY on resume: the samples measure
    /// against the relay's *process* monotonic clock, and a reclaim landing
    /// on a different pod — the normal case behind a load balancer — is
    /// measuring a different origin entirely.
    pub fn reset(&self) {
        *self.est.lock().unwrap() = TimeSyncEstimator::default();
    }

    pub fn sample(&self) -> Option<TimeSyncStats> {
        self.est.lock().unwrap().best()
    }
}

/// Decides when a ClockMapping goes out. Pure and clock-injectable so the
/// cadence is a unit test rather than a sleep. The rule: nothing before the
/// first pong (publishing a zero would assert this machine's clock IS the
/// relay's), then every CLOCK_MAPPING_INTERVAL_MS.
#[derive(Debug)]
pub struct ClockMappingPublisher {
    interval_us: u64,
    last_sent_us: u64,
    ever_sent: bool,
}

impl ClockMappingPublisher {
    pub fn new() -> Self {
        Self {
            interval_us: CLOCK_MAPPING_INTERVAL_MS * 1000,
            last_sent_us: 0,
            ever_sent: false,
        }
    }

    /// Re-arms "publish as soon as there is a sample". Used on resume: the
    /// relay dropped the cached mapping when the new publisher session
    /// claimed the hub, and waiting out the ordinary interval would leave
    /// every joining viewer without an offset for up to that long.
    pub fn reset(&mut self) {
        self.ever_sent = false;
        self.last_sent_us = 0;
    }

    /// Whether to publish now; `have_sample` is whether a pong has landed.
    pub fn due(&mut self, now_us: u64, have_sample: bool) -> bool {
        if !have_sample {
            return false;
        }
        if !self.ever_sent {
            self.ever_sent = true;
            self.last_sent_us = now_us;
            return true;
        }
        if now_us - self.last_sent_us >= self.interval_us {
            self.last_sent_us = now_us;
            return true;
        }
        false
    }
}

impl Default for ClockMappingPublisher {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lowest_rtt_sample_wins_within_the_window() {
        let mut e = TimeSyncEstimator::default();
        e.record(0, 1_000_000, 40_000); // rtt 40 ms, offset 1_000_000 - 20_000
        e.record(100_000, 2_000_000, 110_000); // rtt 10 ms — the winner
        let best = e.best().unwrap();
        assert_eq!(best.rtt_ms, 10.0);
        assert_eq!(best.offset_us, 2_000_000 - 105_000);

        // The window is 8: a cheap old sample eventually ages out.
        for i in 0..TIME_SYNC_SAMPLE_WINDOW as u64 {
            let t0 = 1_000_000 + i * 100_000;
            e.record(t0, 5_000_000, t0 + 30_000); // rtt 30 ms each
        }
        assert_eq!(e.best().unwrap().rtt_ms, 30.0);

        // An impossible exchange is discarded, not recorded.
        e.record(100, 42, 50);
        assert_eq!(e.best().unwrap().rtt_ms, 30.0);
    }

    #[test]
    fn clock_mapping_waits_for_a_sample_then_holds_cadence() {
        let mut p = ClockMappingPublisher::new();
        // Nothing without a sample, no matter how long.
        assert!(!p.due(10_000_000, false));
        // First sample: immediately due.
        assert!(p.due(10_000_000, true));
        // Not again inside the interval…
        assert!(!p.due(10_000_000 + 4_999_999, true));
        // …due at the interval.
        assert!(p.due(10_000_000 + 5_000_000, true));
        // Reset re-arms the immediate publish (resume re-prime).
        p.reset();
        assert!(p.due(20_000_000, true));
    }
}
