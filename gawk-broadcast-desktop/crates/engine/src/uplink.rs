//! Uplink-health detection (docs/38 D12 amendment, 2026-07-31): decides
//! when the shell should warn that the user's upload bandwidth is not
//! keeping up with the stream. Pure counter arithmetic over 1 Hz stats
//! snapshots — no clocks, no transport — so the whole policy is unit
//! tests.
//!
//! It is a **capacity** signal: a second is bad when the sender could not
//! hand the stream to the network as fast as the encoder made it — work
//! backing up or thrown away on this side of the uplink. Packet loss alone
//! never decides it: a lost packet is the network's, and whether it was a
//! radio going off-channel or a full queue the count cannot say. Loss is
//! the Wi-Fi/network notice's input (`lossnotice`, docs/57 D7, docs/75 D6).
//! R73's lab measured why (docs/75 RU4, 2026-10-21): a ≥ 5 % packet-loss
//! signal held this warning through every 80 ms radio outage while the
//! uplink carried the stream.
//!
//! In both delivery modes (the F-12 signals, and R75's):
//! - a keyframe stream superseded or failed in the second: a keyframe
//!   outlived its GOP;
//! - frames dropped at send: sends are failing;
//! - sent fps far below encode fps: most frames never leave the box;
//! - every keyframe acknowledged in the second drained slowly
//!   ([`crate::drain`]: under three quarters of the stream's own wire rate).
//!   This is the one that sees a full uplink under BBR (R75, docs/85): BBR
//!   holds the stream's rate and the network drops the excess, so nothing
//!   backs up on this side and the counters above stay clean, but every
//!   keyframe crawls. A radio outage or a lost packet stretches one keyframe
//!   in a few; a link that cannot carry the stream stretches them all.
//!
//! And per delivery mode, judged only in the mode in use (R73, docs/75):
//! - *Lowest latency* (datagrams): quinn's send buffer fell further behind —
//!   its dropped-plus-queued count ([`Stats::uplink_datagrams_dropped`])
//!   grew by at least [`BAD_SECOND_DATAGRAMS_BEHIND`]. quinn takes every
//!   datagram into that buffer, so the send counters above stay clean;
//! - *Reliable* (carriers): records shed — a GOP still queued when the next
//!   began — or every carrier that retired in the second expired. One GOP
//!   in a few missing the 150 ms deadline is round-trip time (a shared
//!   link's full queue, a long path), not capacity; and on a path whose
//!   minimum RTT is over a third of that deadline every carrier that meets a
//!   loss expires, so expiries are not read there at all (R75,
//!   [`CARRIER_EXPIRY_MAX_MIN_RTT_MS`]).
//!
//! Hysteresis on both edges: several consecutive bad seconds raise the
//! warning (one slow keyframe is weather, not climate), and it takes a
//! longer run of clean seconds to clear it (a flapping banner is worse
//! than a sticky one).

use crate::carrier::{CARRIER_RETIRE_DEADLINE, UplinkMode};
use crate::stats::Stats;

/// Consecutive bad seconds before the warning raises.
pub const WARN_AFTER_BAD_SECONDS: u32 = 5;
/// Consecutive clean seconds before it clears.
pub const CLEAR_AFTER_GOOD_SECONDS: u32 = 15;

/// How far quinn's send buffer must fall behind in one second — datagrams
/// newly dropped or still queued — for a bad second on datagrams. On the
/// lab's clean paths it never grew by 10 in a second, and where it did on a
/// path with room it was for a second or two; on a thin uplink beside an
/// upload it grew by up to 270 at once (docs/75 §11, tables R and O3).
pub const BAD_SECOND_DATAGRAMS_BEHIND: u64 = 10;

/// The longest minimum RTT on which an expired carrier still says anything
/// about capacity (R75, docs/85 D5): a third of the carrier's deadline. A
/// lost packet costs a carrier about three round trips — the first try, loss
/// detection, the retransmission's acknowledgement — so on a longer path
/// every carrier that meets a loss expires, whatever the uplink's capacity.
/// The lab's 100 ms path with 2 % random loss held the warning that way
/// while viewers lost 1.6 % of frames.
pub const CARRIER_EXPIRY_MAX_MIN_RTT_MS: f64 = CARRIER_RETIRE_DEADLINE.as_millis() as f64 / 3.0;

#[derive(Debug, Clone, Copy, Default)]
struct Counters {
    late_keyframes: u64,
    dropped: u64,
    datagrams_behind: u64,
    carriers_opened: u64,
    carriers_expired: u64,
    records_shed: u64,
    keyframes_acked: u64,
    keyframes_slow: u64,
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
            late_keyframes: st.keyframe_streams_superseded + st.keyframe_streams_failed,
            dropped: st.frames_dropped_at_send,
            datagrams_behind: st.uplink_datagrams_dropped,
            carriers_opened: st.uplink_carriers_opened,
            carriers_expired: st.uplink_carriers_expired,
            records_shed: st.uplink_records_shed,
            keyframes_acked: st.keyframe_streams_sent,
            keyframes_slow: st.keyframe_streams_slow,
        };
        let Some(prev) = self.last.replace(now) else {
            return self.warned; // first sample: no deltas yet
        };
        let delta = |f: fn(&Counters) -> u64| f(&now).saturating_sub(f(&prev));

        // Keyframes come about twice a second: one a second outliving its
        // GOP, held for the streak, is the uplink running late (docs/75 RU4:
        // beside an upload on a thin uplink that is what moved, one in two).
        // R75: every keyframe acknowledged in the second drained slowly —
        // the path delivered less than the stream needs, whatever the
        // controller did with its window (docs/85 D3). One slow keyframe
        // among fast ones is an outage or a loss recovery, not the link.
        let slow = delta(|c| c.keyframes_slow);
        let full_uplink = slow >= 1 && slow >= delta(|c| c.keyframes_acked);
        let both_modes = delta(|c| c.late_keyframes) >= 1
            || delta(|c| c.dropped) > 0
            || (st.encoder_fps > 20.0 && st.sent_fps < st.encoder_fps * 0.75)
            || full_uplink;
        let this_mode = if st.uplink_mode == UplinkMode::Carrier.as_str() {
            let expired = delta(|c| c.carriers_expired);
            let expiry_is_capacity = st.uplink_min_rtt_ms <= CARRIER_EXPIRY_MAX_MIN_RTT_MS;
            delta(|c| c.records_shed) > 0
                || (expiry_is_capacity && expired > 0 && expired >= delta(|c| c.carriers_opened))
        } else {
            // A draining buffer reads as no growth, so one burst it
            // absorbed is a single bad second, not a streak.
            delta(|c| c.datagrams_behind) >= BAD_SECOND_DATAGRAMS_BEHIND
        };
        let bad = both_modes || this_mode;

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

    /// One second of a lab run (docs/75 §11, RU6's per-second `Y` lines from
    /// `tools/uplinklab`): QUIC packets sent and declared lost, keyframe
    /// streams superseded or failed, the send buffer's dropped-datagram
    /// counter *as read* (drops plus whatever still sits in the buffer), and
    /// carriers opened, carriers expired and records shed — every other
    /// column a delta over the second.
    type LabSecond = (u64, u64, u64, u64, u64, u64, u64);

    /// Replays lab seconds through a fresh monitor in one delivery mode, the
    /// encoder and sender at 60 fps throughout (no frame failed to leave in
    /// any of these runs); returns whether the warning showed after each.
    fn replay(mode: UplinkMode, seconds: &[LabSecond]) -> Vec<bool> {
        let mut m = UplinkMonitor::new();
        let mut st = Stats {
            encoder_fps: 60.0,
            sent_fps: 60.0,
            uplink_packets_available: true,
            uplink_mode: mode.as_str().to_owned(),
            ..Default::default()
        };
        m.observe(&st);
        let mut shown = Vec::new();
        for &(sent, lost, kf, dropped, opened, expired, shed) in seconds {
            st.uplink_packets_sent += sent;
            st.uplink_packets_lost += lost;
            st.keyframe_streams_superseded += kf;
            st.uplink_datagrams_dropped = dropped;
            st.uplink_carriers_opened += opened;
            st.uplink_carriers_expired += expired;
            st.uplink_records_shed += shed;
            shown.push(m.observe(&st));
        }
        shown
    }

    // Table R's *radio away 80 ms* at 10 ms RTT (rtt10-out80drop, seconds
    // 4–28): the Wi-Fi/AWDL shape. 5–20 % of every second's packets are
    // lost to the outages, and the link carries the stream — the Reliable
    // viewer saw 97 %. That is the loss notice's case (`lossnotice`), not an
    // upload one; RU4's packet-loss signal held this warning for the whole
    // run in both modes, with both controllers.
    const RADIO_AWAY_DATAGRAM_BBR: &[LabSecond] = &[
        (1206, 95, 0, 0, 0, 0, 0),
        (1116, 92, 0, 0, 0, 0, 0),
        (927, 58, 0, 0, 0, 0, 0),
        (894, 66, 0, 0, 0, 0, 0),
        (886, 70, 0, 0, 0, 0, 0),
        (1051, 113, 0, 0, 0, 0, 0),
        (1283, 138, 0, 0, 0, 0, 0),
        (1429, 90, 0, 0, 0, 0, 0),
        (1551, 112, 0, 0, 0, 0, 0),
        (1495, 104, 0, 0, 0, 0, 0),
        (1366, 103, 0, 0, 0, 0, 0),
        (1177, 84, 0, 0, 0, 0, 0),
        (1090, 0, 0, 0, 0, 0, 0),
        (1068, 53, 0, 0, 0, 0, 0),
        (1404, 268, 0, 0, 0, 0, 0),
        (1155, 63, 0, 0, 0, 0, 0),
        (1212, 66, 0, 0, 0, 0, 0),
        (1019, 65, 0, 0, 0, 0, 0),
        (1075, 58, 0, 0, 0, 0, 0),
        (966, 59, 0, 0, 0, 0, 0),
        (1074, 63, 0, 0, 0, 0, 0),
        (1428, 298, 0, 0, 0, 0, 0),
        (960, 41, 0, 0, 0, 0, 0),
        (994, 41, 0, 0, 0, 0, 0),
        (1113, 67, 0, 0, 0, 0, 0),
    ];
    const RADIO_AWAY_CARRIER_CUBIC: &[LabSecond] = &[
        (1106, 65, 0, 0, 2, 0, 0),
        (1030, 58, 0, 0, 2, 0, 0),
        (880, 64, 0, 0, 2, 0, 0),
        (842, 56, 0, 0, 2, 0, 0),
        (836, 57, 0, 0, 2, 0, 0),
        (909, 41, 0, 0, 2, 0, 0),
        (1087, 39, 0, 0, 2, 0, 0),
        (1338, 96, 0, 0, 2, 0, 0),
        (1445, 96, 0, 0, 2, 0, 0),
        (1390, 89, 0, 0, 2, 0, 0),
        (1269, 73, 0, 0, 2, 0, 0),
        (1114, 74, 0, 0, 2, 0, 0),
        (982, 0, 0, 0, 2, 0, 0),
        (1052, 89, 0, 0, 2, 0, 0),
        (1166, 106, 0, 0, 2, 1, 0),
        (1111, 55, 0, 0, 2, 0, 0),
        (1142, 48, 0, 0, 2, 0, 0),
        (976, 56, 0, 0, 2, 0, 0),
        (1029, 62, 0, 0, 2, 0, 0),
        (933, 50, 0, 0, 2, 0, 0),
        (1021, 53, 0, 0, 2, 0, 0),
        (1139, 81, 0, 0, 2, 1, 0),
        (923, 39, 0, 0, 2, 0, 0),
        (951, 45, 0, 0, 2, 0, 0),
        (1074, 60, 0, 0, 2, 0, 0),
    ];

    // Table O's thin cell: 15 Mbps beside a household's TCP upload, which
    // starts at 20 s (r15-*-engine-cubic-tcp-s1, seconds 15–44). On
    // datagrams the send buffer backs up and drops, and a keyframe outlives
    // its GOP most seconds (one of two — never the two a second docs/38
    // counted); on the carrier every GOP's carrier expires. Undecodable
    // 58 % and 12 %; neither raised the warning RU4 shipped on datagrams.
    const THIN_BESIDE_AN_UPLOAD_DATAGRAM_CUBIC: &[LabSecond] = &[
        (1252, 14, 0, 15, 0, 0, 0),
        (1193, 28, 0, 15, 0, 0, 0),
        (1155, 20, 0, 23, 0, 0, 0),
        (1285, 19, 0, 23, 0, 0, 0),
        (1262, 18, 0, 23, 0, 0, 0),
        (1324, 21, 0, 23, 0, 0, 0),
        (665, 129, 0, 297, 0, 0, 0),
        (608, 0, 1, 574, 0, 0, 0),
        (697, 0, 2, 560, 0, 0, 0),
        (835, 0, 1, 442, 0, 0, 0),
        (920, 0, 2, 234, 0, 0, 0),
        (1049, 0, 1, 90, 0, 0, 0),
        (925, 10, 0, 90, 0, 0, 0),
        (931, 10, 1, 123, 0, 0, 0),
        (833, 0, 1, 160, 0, 0, 0),
        (790, 6, 2, 205, 0, 0, 0),
        (744, 0, 1, 226, 0, 0, 0),
        (782, 0, 2, 185, 0, 0, 0),
        (881, 0, 3, 90, 0, 0, 0),
        (1022, 0, 1, 89, 0, 0, 0),
        (1032, 2, 0, 89, 0, 0, 0),
        (1006, 0, 1, 90, 0, 0, 0),
        (1031, 5, 0, 93, 0, 0, 0),
        (866, 0, 0, 142, 0, 0, 0),
        (896, 0, 1, 95, 0, 0, 0),
        (967, 0, 1, 93, 0, 0, 0),
        (1030, 5, 1, 94, 0, 0, 0),
        (1035, 0, 0, 93, 0, 0, 0),
        (1136, 0, 1, 93, 0, 0, 0),
        (1184, 0, 0, 93, 0, 0, 0),
    ];
    const THIN_BESIDE_AN_UPLOAD_CARRIER_CUBIC: &[LabSecond] = &[
        (1058, 8, 0, 0, 2, 0, 0),
        (994, 17, 0, 0, 2, 0, 0),
        (977, 24, 0, 0, 2, 0, 0),
        (1056, 0, 0, 0, 2, 0, 0),
        (1057, 20, 0, 0, 2, 0, 0),
        (1109, 30, 0, 0, 2, 0, 0),
        (366, 89, 0, 1, 2, 2, 0),
        (352, 0, 0, 1, 2, 2, 0),
        (497, 0, 0, 1, 2, 2, 0),
        (593, 0, 0, 1, 2, 2, 0),
        (676, 0, 0, 1, 2, 2, 0),
        (784, 0, 0, 0, 2, 2, 0),
        (856, 4, 0, 0, 2, 1, 0),
        (828, 3, 0, 0, 2, 1, 0),
        (764, 0, 0, 1, 2, 2, 0),
        (777, 0, 0, 1, 2, 2, 0),
        (845, 0, 0, 0, 2, 2, 0),
        (834, 13, 0, 1, 2, 1, 0),
        (836, 7, 0, 0, 2, 2, 0),
        (823, 0, 0, 1, 2, 0, 0),
        (771, 0, 0, 1, 2, 2, 0),
        (860, 0, 0, 1, 2, 2, 0),
        (821, 15, 0, 0, 2, 1, 0),
        (918, 6, 0, 0, 2, 2, 0),
        (945, 0, 0, 1, 2, 1, 0),
        (912, 0, 0, 1, 2, 2, 0),
        (953, 3, 0, 0, 2, 2, 0),
        (978, 0, 0, 1, 2, 0, 0),
        (958, 3, 0, 1, 2, 2, 0),
        (889, 0, 0, 1, 2, 2, 0),
    ];

    // Table O3's 30 Mbps cell beside the upload, carrier · BBR
    // (r30-carrier-engine-bbr-tcp-s1, seconds 19–43): the uplink has room
    // for the stream (the upload kept 102 % of its share), but the upload's
    // full queue puts 40–60 ms on every round trip, so one GOP in a few
    // misses the carrier's 150 ms deadline. RU4's warning raised at 30 s and
    // stayed; it is not the upload that cannot keep up.
    const SHARED_WITH_ROOM_CARRIER_BBR: &[LabSecond] = &[
        (1177, 110, 0, 0, 2, 0, 0),
        (1123, 20, 0, 0, 2, 0, 0),
        (1054, 309, 0, 0, 2, 1, 0),
        (909, 0, 0, 0, 2, 0, 0),
        (840, 0, 0, 0, 2, 0, 0),
        (935, 0, 0, 0, 2, 0, 0),
        (982, 0, 0, 1, 2, 0, 0),
        (836, 0, 0, 0, 2, 1, 0),
        (872, 0, 0, 0, 2, 2, 0),
        (955, 0, 0, 0, 2, 1, 0),
        (888, 0, 0, 1, 2, 1, 0),
        (858, 0, 0, 1, 2, 2, 0),
        (771, 0, 0, 0, 2, 2, 0),
        (940, 0, 0, 1, 2, 0, 0),
        (852, 0, 0, 0, 2, 2, 0),
        (905, 0, 0, 0, 2, 0, 0),
        (964, 0, 0, 0, 2, 1, 0),
        (998, 0, 0, 0, 2, 1, 0),
        (816, 0, 0, 0, 2, 0, 0),
        (913, 0, 0, 0, 2, 2, 0),
        (982, 0, 0, 0, 2, 0, 0),
        (986, 0, 0, 0, 2, 0, 0),
        (958, 0, 0, 0, 2, 0, 0),
        (1016, 0, 0, 0, 2, 0, 0),
        (1007, 0, 0, 0, 2, 0, 0),
    ];

    // R75's engine matrix, *100 ms, 2 % loss*, carrier · BBR
    // (rtt100-loss0.02/eng-carrier-bbr, seconds 4–28): every carrier that
    // meets a loss retransmits past its 150 ms deadline — three round trips
    // are 300 ms — so most seconds every carrier expires, while the uplink
    // carries the stream (1.6 % undecodable). RU4's warning raised at 13 s
    // and held (docs/75 table R's false raise, docs/85 D5).
    const LONG_LOSSY_PATH_CARRIER_BBR: &[LabSecond] = &[
        (1075, 19, 0, 0, 2, 2, 0),
        (997, 16, 0, 0, 2, 2, 0),
        (838, 19, 0, 0, 2, 1, 0),
        (819, 25, 0, 0, 2, 1, 0),
        (794, 14, 0, 0, 2, 1, 0),
        (889, 21, 0, 0, 2, 2, 0),
        (1085, 28, 0, 1, 2, 2, 0),
        (1278, 29, 0, 0, 2, 2, 0),
        (1393, 28, 0, 0, 2, 2, 0),
        (1327, 16, 0, 0, 2, 2, 0),
        (1211, 20, 0, 0, 2, 2, 0),
        (1064, 30, 0, 0, 2, 2, 0),
        (1009, 27, 0, 1, 2, 1, 0),
        (984, 26, 0, 0, 2, 2, 0),
        (1094, 33, 0, 0, 2, 2, 0),
        (1094, 33, 0, 0, 2, 1, 0),
        (1106, 22, 0, 0, 2, 0, 0),
        (936, 23, 0, 0, 2, 2, 0),
        (993, 22, 0, 1, 2, 2, 0),
        (884, 16, 0, 1, 2, 2, 0),
        (995, 15, 0, 0, 2, 0, 0),
        (1097, 27, 0, 0, 2, 2, 0),
        (885, 15, 0, 0, 2, 2, 0),
        (911, 17, 0, 0, 2, 2, 0),
        (1029, 16, 0, 0, 2, 2, 0),
    ];

    #[test]
    fn a_long_lossy_path_is_not_an_upload_problem() {
        let mut m = UplinkMonitor::new();
        let mut st = Stats {
            encoder_fps: 60.0,
            sent_fps: 60.0,
            uplink_packets_available: true,
            uplink_mode: UplinkMode::Carrier.as_str().to_owned(),
            uplink_min_rtt_ms: 100.4,
            ..Default::default()
        };
        m.observe(&st);
        let mut shown = Vec::new();
        for &(sent, lost, kf, dropped, opened, expired, shed) in LONG_LOSSY_PATH_CARRIER_BBR {
            st.uplink_packets_sent += sent;
            st.uplink_packets_lost += lost;
            st.keyframe_streams_superseded += kf;
            st.uplink_datagrams_dropped = dropped;
            st.uplink_carriers_opened += opened;
            st.uplink_carriers_expired += expired;
            st.uplink_records_shed += shed;
            shown.push(m.observe(&st));
        }
        assert!(!shown.contains(&true), "{shown:?}");
        // The same seconds on a 10 ms path are a full uplink: expiries count.
        assert!(replay(UplinkMode::Carrier, LONG_LOSSY_PATH_CARRIER_BBR).contains(&true));
    }

    #[test]
    fn a_radio_outage_is_not_an_upload_problem() {
        for (name, mode, seconds) in [
            (
                "datagram · BBR",
                UplinkMode::Datagram,
                RADIO_AWAY_DATAGRAM_BBR,
            ),
            (
                "carrier · Cubic",
                UplinkMode::Carrier,
                RADIO_AWAY_CARRIER_CUBIC,
            ),
        ] {
            let shown = replay(mode, seconds);
            assert!(!shown.contains(&true), "{name}: {shown:?}");
        }
    }

    #[test]
    fn a_thin_uplink_beside_an_upload_warns_in_both_modes() {
        for (name, mode, seconds) in [
            (
                "datagram · Cubic",
                UplinkMode::Datagram,
                THIN_BESIDE_AN_UPLOAD_DATAGRAM_CUBIC,
            ),
            (
                "carrier · Cubic",
                UplinkMode::Carrier,
                THIN_BESIDE_AN_UPLOAD_CARRIER_CUBIC,
            ),
        ] {
            let shown = replay(mode, seconds);
            // The upload starts at second 20 (row 5): five bad seconds
            // later, at 25 (row 10), the warning shows, and it holds.
            assert!(!shown[..10].contains(&true), "{name}: early, {shown:?}");
            assert!(!shown[10..].contains(&false), "{name}: {shown:?}");
        }
    }

    /// One second of an R75 lab run (docs/85 §7): a [`LabSecond`], then the
    /// keyframe streams acknowledged in it and how many of those drained
    /// slowly (`Stats::keyframe_streams_slow`, the lab's `Y` lines).
    type LabSecondKf = (u64, u64, u64, u64, u64, u64, u64, u64, u64);

    fn replay_kf(mode: UplinkMode, seconds: &[LabSecondKf]) -> Vec<bool> {
        let mut m = UplinkMonitor::new();
        let mut st = Stats {
            encoder_fps: 60.0,
            sent_fps: 60.0,
            uplink_packets_available: true,
            uplink_mode: mode.as_str().to_owned(),
            uplink_min_rtt_ms: 10.0,
            ..Default::default()
        };
        m.observe(&st);
        let mut shown = Vec::new();
        for &(sent, lost, kf, dropped, opened, expired, shed, acked, slow) in seconds {
            st.uplink_packets_sent += sent;
            st.uplink_packets_lost += lost;
            st.keyframe_streams_superseded += kf;
            st.uplink_datagrams_dropped = dropped;
            st.uplink_carriers_opened += opened;
            st.uplink_carriers_expired += expired;
            st.uplink_records_shed += shed;
            st.keyframe_streams_sent += acked;
            st.keyframe_streams_slow += slow;
            shown.push(m.observe(&st));
        }
        shown
    }

    // R75's acceptance run (docs/85 §7): table O's thin cell, 15 Mbps beside
    // a household's TCP upload from 20 s, now with BBR (r15-*-engine-bbr-
    // tcp-s1, seconds 15–44). BBR holds the stream's rate and the network
    // drops the excess: a quarter of the packets lost, the send buffer
    // mostly keeping up, keyframes finishing inside their GOP, carriers
    // expiring in runs of four at most — RU4's monitor stayed silent on both
    // (docs/75 table O3). Every keyframe from second 21 drains at under three
    // quarters of the stream's wire rate.
    const THIN_BESIDE_AN_UPLOAD_DATAGRAM_BBR: &[LabSecondKf] = &[
        (1276, 26, 0, 0, 0, 0, 0, 2, 0),
        (1308, 120, 0, 0, 0, 0, 0, 2, 0),
        (1446, 296, 0, 0, 0, 0, 0, 2, 0),
        (1572, 294, 0, 0, 0, 0, 0, 2, 0),
        (1365, 139, 0, 0, 0, 0, 0, 2, 0),
        (1453, 152, 0, 0, 0, 0, 0, 2, 0),
        (1553, 598, 0, 0, 0, 0, 0, 2, 2),
        (1291, 317, 0, 100, 0, 0, 0, 1, 1),
        (1116, 34, 0, 0, 0, 0, 0, 3, 3),
        (1326, 192, 0, 0, 0, 0, 0, 2, 2),
        (1464, 245, 0, 0, 0, 0, 0, 2, 2),
        (1027, 19, 0, 0, 0, 0, 0, 2, 2),
        (1119, 79, 0, 0, 0, 0, 0, 2, 2),
        (1415, 258, 0, 0, 0, 0, 0, 2, 2),
        (1594, 352, 0, 0, 0, 0, 0, 2, 2),
        (1345, 116, 0, 0, 0, 0, 0, 1, 1),
        (1130, 59, 0, 0, 0, 0, 0, 3, 3),
        (637, 0, 0, 180, 0, 0, 0, 0, 0),
        (1544, 319, 1, 0, 0, 0, 0, 2, 2),
        (1196, 116, 0, 0, 0, 0, 0, 3, 3),
        (1238, 83, 0, 0, 0, 0, 0, 2, 2),
        (1265, 97, 0, 0, 0, 0, 0, 1, 1),
        (1030, 68, 0, 0, 0, 0, 0, 3, 3),
        (946, 0, 0, 63, 0, 0, 0, 1, 1),
        (992, 39, 1, 0, 0, 0, 0, 1, 1),
        (1191, 31, 0, 5, 0, 0, 0, 2, 2),
        (1008, 0, 0, 0, 0, 0, 0, 2, 2),
        (1295, 294, 0, 76, 0, 0, 0, 2, 2),
        (1420, 221, 1, 0, 0, 0, 0, 2, 2),
        (1157, 119, 0, 19, 0, 0, 0, 1, 1),
    ];
    const THIN_BESIDE_AN_UPLOAD_CARRIER_BBR: &[LabSecondKf] = &[
        (1060, 20, 0, 0, 2, 0, 0, 2, 0),
        (1093, 97, 0, 0, 2, 0, 0, 2, 0),
        (1206, 244, 0, 0, 2, 0, 0, 2, 1),
        (1477, 408, 0, 0, 2, 0, 0, 2, 1),
        (1419, 365, 0, 0, 2, 0, 0, 2, 0),
        (1203, 130, 0, 0, 2, 0, 0, 2, 0),
        (1515, 643, 0, 0, 2, 1, 0, 2, 2),
        (399, 0, 0, 0, 2, 2, 0, 1, 1),
        (513, 0, 0, 0, 2, 2, 0, 3, 3),
        (701, 84, 0, 1, 2, 2, 0, 1, 1),
        (1063, 94, 0, 0, 2, 2, 0, 3, 1),
        (1142, 285, 0, 0, 2, 1, 0, 2, 0),
        (1133, 237, 0, 0, 2, 0, 0, 2, 0),
        (1315, 327, 0, 0, 2, 0, 0, 2, 1),
        (1166, 163, 0, 1, 2, 0, 0, 2, 2),
        (1377, 300, 0, 0, 2, 1, 0, 2, 2),
        (1017, 101, 0, 0, 2, 1, 0, 2, 2),
        (933, 140, 0, 0, 2, 1, 0, 2, 2),
        (993, 123, 0, 0, 2, 1, 0, 2, 1),
        (1147, 217, 0, 0, 2, 0, 0, 2, 2),
        (1240, 231, 0, 0, 2, 0, 0, 2, 2),
        (1481, 459, 0, 0, 2, 0, 0, 2, 2),
        (1004, 181, 0, 0, 2, 0, 0, 2, 2),
        (1135, 198, 0, 0, 2, 1, 0, 2, 1),
        (1333, 330, 0, 0, 2, 0, 0, 2, 2),
        (1176, 198, 0, 0, 2, 0, 0, 2, 2),
        (1371, 377, 0, 0, 2, 0, 0, 2, 2),
        (1377, 387, 0, 1, 2, 0, 0, 2, 2),
        (1338, 328, 0, 0, 2, 1, 0, 2, 2),
        (1047, 74, 0, 0, 2, 1, 0, 2, 2),
    ];

    #[test]
    fn a_thin_uplink_beside_an_upload_warns_with_bbr_too() {
        for (name, mode, seconds) in [
            (
                "datagram · BBR",
                UplinkMode::Datagram,
                THIN_BESIDE_AN_UPLOAD_DATAGRAM_BBR,
            ),
            (
                "carrier · BBR",
                UplinkMode::Carrier,
                THIN_BESIDE_AN_UPLOAD_CARRIER_BBR,
            ),
        ] {
            let shown = replay_kf(mode, seconds);
            // The upload starts at second 20 (row 5): five bad seconds
            // later, at 25 (row 10), the warning shows, and it holds.
            assert!(!shown[..10].contains(&true), "{name}: early, {shown:?}");
            assert!(!shown[10..].contains(&false), "{name}: {shown:?}");
        }
    }

    #[test]
    fn a_shared_uplink_with_room_for_the_stream_does_not_warn() {
        let shown = replay(UplinkMode::Carrier, SHARED_WITH_ROOM_CARRIER_BBR);
        assert!(!shown.contains(&true), "{shown:?}");
    }

    // The principle (docs/75 RU4, 2026-10-21): this warning is about
    // capacity, and packet loss alone is the loss notice's input. BBR's own
    // loss on a nearly full uplink (7–30 % a second, F17) is not told apart
    // from a radio's by the packet count, so it never decides here.
    #[test]
    fn packet_loss_alone_never_warns() {
        let lossy = [(1300, 260, 0, 0, 2, 0, 0); 30];
        for mode in [UplinkMode::Datagram, UplinkMode::Carrier] {
            assert!(!replay(mode, &lossy).contains(&true), "{mode:?}");
        }
    }

    #[test]
    fn a_send_buffer_that_keeps_falling_behind_warns_on_datagrams() {
        // Dropped-or-queued datagrams growing by 30 a second.
        let behind: Vec<LabSecond> = (1..=6).map(|i| (1300, 0, 0, 30 * i, 0, 0, 0)).collect();
        assert_eq!(
            replay(UplinkMode::Datagram, &behind),
            [false, false, false, false, true, true]
        );
        // A buffer that fills and drains again is a burst it absorbed.
        let absorbed: Vec<LabSecond> = (0..20)
            .map(|i| (1300, 0, 0, if i % 2 == 0 { 300 } else { 0 }, 0, 0, 0))
            .collect();
        assert!(!replay(UplinkMode::Datagram, &absorbed).contains(&true));
        // A handful in a second is not falling behind.
        let few: Vec<LabSecond> = (1..=20).map(|i| (1300, 0, 0, 5 * i, 0, 0, 0)).collect();
        assert!(!replay(UplinkMode::Datagram, &few).contains(&true));
    }

    #[test]
    fn a_carrier_shedding_or_every_carrier_expiring_warns() {
        let warns = |s: LabSecond| replay(UplinkMode::Carrier, &[s; 6])[4];
        assert!(
            warns((1300, 0, 0, 0, 2, 0, 12)),
            "records shed every second"
        );
        assert!(warns((1300, 0, 0, 0, 2, 2, 0)), "every carrier expired");
        assert!(warns((1300, 0, 0, 0, 1, 1, 0)), "a one-second GOP's, too");
        assert!(!warns((1300, 0, 0, 0, 2, 1, 0)), "one GOP in two is not");
        assert!(!warns((1300, 0, 0, 0, 2, 0, 0)));
    }

    // Each delivery mode is judged on its own signals: after a switch the
    // other mode's counters can still move (a retiring carrier expiring, the
    // audio's datagrams), and say nothing about the video's uplink.
    #[test]
    fn each_mode_is_judged_on_its_own_signals() {
        let carriers_expiring = [(1300, 0, 0, 0, 2, 2, 12); 10];
        assert!(!replay(UplinkMode::Datagram, &carriers_expiring).contains(&true));
        let buffer_behind: Vec<LabSecond> =
            (1..=10).map(|i| (1300, 0, 0, 30 * i, 2, 0, 0)).collect();
        assert!(!replay(UplinkMode::Carrier, &buffer_behind).contains(&true));
    }

    // R75 (docs/85 D3): every keyframe acknowledged in the second slow is a
    // bad second; one slow among fast ones is not, nor a second with no
    // keyframe acknowledged at all.
    #[test]
    fn every_keyframe_draining_slowly_warns_in_both_modes() {
        let run = |mode: UplinkMode, per_second: (u64, u64)| {
            let mut m = UplinkMonitor::new();
            let mut st = Stats {
                encoder_fps: 60.0,
                sent_fps: 60.0,
                uplink_mode: mode.as_str().to_owned(),
                ..Default::default()
            };
            m.observe(&st);
            (0..WARN_AFTER_BAD_SECONDS)
                .map(|_| {
                    st.keyframe_streams_sent += per_second.0;
                    st.keyframe_streams_slow += per_second.1;
                    m.observe(&st)
                })
                .last()
                .unwrap()
        };
        for mode in [UplinkMode::Datagram, UplinkMode::Carrier] {
            assert!(run(mode, (2, 2)), "{mode:?}: both slow");
            assert!(run(mode, (1, 1)), "{mode:?}: the one slow");
            assert!(!run(mode, (2, 1)), "{mode:?}: one of two");
            assert!(!run(mode, (2, 0)), "{mode:?}: none");
            assert!(!run(mode, (0, 0)), "{mode:?}: none acknowledged");
        }
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
