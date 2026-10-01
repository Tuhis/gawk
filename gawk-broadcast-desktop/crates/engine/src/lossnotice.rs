//! When to tell the broadcaster their network is costing viewers video
//! (docs/57 D7, WU3). The bandwidth watchdog (`uplink`) cannot see this
//! case: a datagram lost in the air — AWDL taking a Mac's Wi-Fi radio
//! off-channel, interference, roaming — "sent" fine, so every local send
//! counter stays clean while viewers freeze on each lost chunk (one lost
//! chunk costs the rest of the GOP). QUIC's own loss detection sees it, and
//! [`Stats::uplink_packets_lost`] carries that count.
//!
//! Pure counter arithmetic over 1 Hz stats snapshots plus the platform's
//! network facts, so the whole policy is table tests:
//!
//! * **speak only on measured harm** (principle 2) — being on Wi-Fi, or
//!   AWDL being up, is never a reason on its own;
//! * **sustained**: packets lost in at least [`RAISE_LOSSY_SECONDS`] of the
//!   last [`WINDOW_SECONDS`] seconds, while someone is watching. At gawk's
//!   rates every lossy second is a likely visible pause, so the count of
//!   lossy seconds, not a loss percentage, is the signal;
//! * **goes away by itself** after [`CLEAR_AFTER_CLEAN_SECONDS`] clean
//!   seconds, like FaceTime's "Poor connection".

use crate::stats::Stats;
use std::collections::VecDeque;

/// The sliding window the harm is judged over.
pub const WINDOW_SECONDS: usize = 10;
/// Lossy seconds inside the window that raise the notice.
pub const RAISE_LOSSY_SECONDS: usize = 4;
/// Consecutive loss-free seconds that clear it. There is no dismissal:
/// while viewers are losing video the broadcaster sees it (docs/57 OD7).
pub const CLEAR_AFTER_CLEAN_SECONDS: u32 = 30;

/// What the platform knows about the path the broadcast leaves on. `None`
/// from a platform means "not probed": the notice stays off there (Windows
/// gets its line as a later chunk, D7).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, serde::Serialize)]
#[serde(rename_all = "camelCase")]
pub struct NetworkFacts {
    /// The link carrying the broadcast is Wi-Fi — looked through a VPN
    /// tunnel to the link the tunnel itself rides on.
    pub wifi: bool,
    /// The relay is reached through a tunnel (VPN). Diagnostics only.
    pub vpn: bool,
    /// `awdl0` is up (macOS). Diagnostics and logs only — never a trigger.
    pub awdl_up: bool,
}

/// Which line the Live page shows.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Notice {
    None,
    /// On Wi-Fi: Help's remedies apply on this machine.
    Wifi,
    /// Not on Wi-Fi: nothing to fix on this machine.
    Network,
}

impl Notice {
    /// D7's copy, normative. No jargon (principle 3): the string test in
    /// this module is the gate.
    pub fn text(self) -> &'static str {
        match self {
            Notice::None => "",
            Notice::Wifi => "Your Wi-Fi is dropping some video. Viewers may see brief pauses.",
            Notice::Network => "Your network is dropping some video. Viewers may see brief pauses.",
        }
    }

    /// The one-line form the desktop window's alert strip shows (R64,
    /// docs/66 D4): the first sentence of [`Notice::text`].
    pub fn short_text(self) -> &'static str {
        match self {
            Notice::None => "",
            Notice::Wifi => "Your Wi-Fi is dropping some video",
            Notice::Network => "Your network is dropping some video",
        }
    }
}

/// One second's worth of the window.
#[derive(Debug, Clone, Copy)]
struct Second {
    lost: u64,
}

/// Feed [`LossMonitor::observe`] one stats snapshot per second.
#[derive(Debug, Default)]
pub struct LossMonitor {
    last: Option<(u64, u64)>,
    window: VecDeque<Second>,
    clean_streak: u32,
    raised: bool,
}

impl LossMonitor {
    pub fn new() -> Self {
        Self::default()
    }

    /// Whether harm is currently measured, whatever the platform's facts.
    pub fn raised(&self) -> bool {
        self.raised
    }

    /// Packets lost over the current window, and the window's length in
    /// seconds — for Diagnostics and the transition log line.
    pub fn window_loss(&self) -> (u64, usize) {
        (self.window.iter().map(|s| s.lost).sum(), self.window.len())
    }

    /// One 1 Hz sample; returns the notice to show.
    pub fn observe(&mut self, st: &Stats, facts: Option<NetworkFacts>) -> Notice {
        if st.uplink_packets_available {
            let now = (st.uplink_packets_sent, st.uplink_packets_lost);
            if let Some(prev) = self.last.replace(now) {
                // Counters only go backwards if the transport restarted
                // them; that second carries no evidence either way.
                let lost = now.1.saturating_sub(prev.1);
                self.push(Second { lost });
            }
        }

        let lossy = self.window.iter().filter(|s| s.lost > 0).count();
        // An old relay without viewer counts cannot tell us; don't hide
        // real harm behind a missing number.
        let watched = !st.viewer_count_available || st.viewer_count > 0;
        if lossy >= RAISE_LOSSY_SECONDS && watched {
            self.raised = true;
        } else if self.raised && self.clean_streak >= CLEAR_AFTER_CLEAN_SECONDS {
            self.raised = false;
        }

        match (self.raised, facts) {
            (true, Some(f)) if f.wifi => Notice::Wifi,
            (true, Some(_)) => Notice::Network,
            _ => Notice::None,
        }
    }

    fn push(&mut self, s: Second) {
        if s.lost > 0 {
            self.clean_streak = 0;
        } else {
            self.clean_streak = self.clean_streak.saturating_add(1);
        }
        self.window.push_back(s);
        while self.window.len() > WINDOW_SECONDS {
            self.window.pop_front();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const WIFI: Option<NetworkFacts> = Some(NetworkFacts {
        wifi: true,
        vpn: false,
        awdl_up: true,
    });
    const WIRED: Option<NetworkFacts> = Some(NetworkFacts {
        wifi: false,
        vpn: false,
        awdl_up: false,
    });

    /// Drives a monitor with per-second lost counts; returns each notice.
    fn run(
        m: &mut LossMonitor,
        sent: &mut u64,
        lost: &mut u64,
        per_second: &[u64],
        viewers: Option<u32>,
        facts: Option<NetworkFacts>,
    ) -> Vec<Notice> {
        per_second
            .iter()
            .map(|&l| {
                *sent += 1200;
                *lost += l;
                let st = Stats {
                    uplink_packets_available: true,
                    uplink_packets_sent: *sent,
                    uplink_packets_lost: *lost,
                    viewer_count_available: viewers.is_some(),
                    viewer_count: viewers.unwrap_or(0),
                    ..Default::default()
                };
                m.observe(&st, facts)
            })
            .collect()
    }

    fn fresh() -> (LossMonitor, u64, u64) {
        let mut m = LossMonitor::new();
        let (mut s, mut l) = (0, 0);
        run(&mut m, &mut s, &mut l, &[0], Some(1), WIFI); // baseline sample
        (m, s, l)
    }

    // The 2026-09-23 finding: AWDL costs a packet or two most seconds. The
    // line must appear within 15 s (G8), and name Wi-Fi.
    #[test]
    fn recurring_loss_on_wifi_raises_the_wifi_line_within_15_s() {
        let (mut m, mut s, mut l) = fresh();
        let out = run(&mut m, &mut s, &mut l, &[1, 0, 2, 0, 1, 1], Some(1), WIFI);
        let first = out.iter().position(|n| *n == Notice::Wifi).unwrap();
        assert!(first < 15);
        assert_eq!(out[first].text(), Notice::Wifi.text());
    }

    #[test]
    fn a_clean_link_never_speaks_even_with_awdl_up() {
        let (mut m, mut s, mut l) = fresh();
        let out = run(&mut m, &mut s, &mut l, &[0; 120], Some(3), WIFI);
        assert!(out.iter().all(|n| *n == Notice::None));
    }

    // One burst (a microwave, a roam) is weather, not climate.
    #[test]
    fn a_single_burst_does_not_raise() {
        let (mut m, mut s, mut l) = fresh();
        let mut seconds = vec![0; 30];
        seconds[5] = 40;
        seconds[6] = 12;
        let out = run(&mut m, &mut s, &mut l, &seconds, Some(1), WIFI);
        assert!(out.iter().all(|n| *n == Notice::None));
    }

    #[test]
    fn nobody_watching_means_nothing_to_say() {
        let (mut m, mut s, mut l) = fresh();
        let out = run(&mut m, &mut s, &mut l, &[1; 20], Some(0), WIFI);
        assert!(out.iter().all(|n| *n == Notice::None));
    }

    #[test]
    fn an_unknown_viewer_count_does_not_hide_harm() {
        let (mut m, mut s, mut l) = fresh();
        let out = run(&mut m, &mut s, &mut l, &[1; 10], None, WIFI);
        assert_eq!(*out.last().unwrap(), Notice::Wifi);
    }

    #[test]
    fn off_wifi_the_line_names_the_network_not_wifi() {
        let (mut m, mut s, mut l) = fresh();
        let out = run(&mut m, &mut s, &mut l, &[1; 10], Some(1), WIRED);
        assert_eq!(*out.last().unwrap(), Notice::Network);
    }

    // Windows has no probe yet (D7: its line is a later chunk).
    #[test]
    fn a_platform_without_a_probe_never_shows_the_line() {
        let (mut m, mut s, mut l) = fresh();
        let out = run(&mut m, &mut s, &mut l, &[1; 10], Some(1), None);
        assert!(out.iter().all(|n| *n == Notice::None));
        assert!(m.raised(), "harm is still measured, for logs");
    }

    #[test]
    fn the_line_goes_away_by_itself_after_30_clean_seconds() {
        let (mut m, mut s, mut l) = fresh();
        run(&mut m, &mut s, &mut l, &[1; 10], Some(1), WIFI);
        let out = run(&mut m, &mut s, &mut l, &[0; 30], Some(1), WIFI);
        assert_eq!(out[28], Notice::Wifi, "sticky until the clean run ends");
        assert_eq!(out[29], Notice::None);
    }

    // A resume's fresh connection restarts at zero in the transport; the
    // sender sums them, but a restart must never read as a loss burst.
    #[test]
    fn counters_going_backwards_are_not_loss() {
        let mut m = LossMonitor::new();
        for (sent, lost) in [(10_000, 500), (100, 0), (1300, 0)] {
            let st = Stats {
                uplink_packets_available: true,
                uplink_packets_sent: sent,
                uplink_packets_lost: lost,
                ..Default::default()
            };
            m.observe(&st, WIFI);
        }
        assert_eq!(m.window_loss().0, 0);
    }

    // Principle 3: no jargon where the user acts. Help and Diagnostics are
    // not main-flow strings and are not checked here.
    #[test]
    fn main_flow_strings_carry_no_jargon() {
        const BANNED: [&str; 9] = [
            "awdl", "channel", "packet", "uplink", "quic", "carrier", "dscp", "ghz", "band",
        ];
        for n in [Notice::Wifi, Notice::Network] {
            for text in [n.text(), n.short_text()] {
                let t = text.to_lowercase();
                for b in BANNED {
                    assert!(!t.contains(b), "{text:?} says {b:?}");
                }
            }
        }
    }

    // docs/66 D4: the strip's line is the full text's first sentence, so
    // the two can't drift apart.
    #[test]
    fn the_short_text_is_the_first_sentence() {
        for n in [Notice::None, Notice::Wifi, Notice::Network] {
            let first = n.text().split(". ").next().unwrap_or("");
            assert_eq!(n.short_text(), first, "{n:?}");
        }
    }
}
