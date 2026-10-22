//! The desktop's switch to a party's local relay (R74 PP14, docs/76 §6.12
//! item 43.3 and §7.3): the Ready page probes home's `/echo` on open and
//! every 30 s (docs/64 D1), so a fresh offer reaches it before the
//! broadcaster goes live. The decisions live in
//! [`gawk_engine::localrelay`]; this is the window's state around them,
//! kept free of Slint so it is tested as plain Rust.
//!
//! - A fresh offer is probed (its candidates in parallel, pinned); the first
//!   to answer is **held** for the offer's validity, and the next broadcast
//!   publishes there. Nothing remembered starts a switch (item 43.1).
//! - **A live broadcast is never moved**: an offer that arrives while live
//!   is probed for the next one, and the window says so.
//! - The chip's "Use … instead" is remembered per PoP handle (item 43.6).
//! - Every candidate failing marks the PoP unreachable for this app session
//!   (item 43.5); on macOS the first failure is retried once, after the
//!   local-network prompt (item 44).

use gawk_engine::localrelay::{
    DialStep, HeldSwitch, LocalRelay, Offer, PromptRetry, Switch, SwitchPolicy, Verdict,
};
use gawk_engine::transport::CertHash;
use std::time::{Duration, Instant};

/// What the shell does with a fresh offer.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum OfferStep {
    /// Nothing: opted out, unreachable, or already being probed.
    Ignore,
    /// The PoP is already held: its validity was renewed.
    Renewed,
    /// Probe this offer's candidates.
    Probe(Offer),
}

/// What the shell does when a probe of an offer's candidates came back.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ProbedStep {
    /// A candidate answered: the next broadcast publishes there.
    Switched,
    /// Probe the same offer again after this delay (the one retry an Apple
    /// platform owes its local-network prompt).
    RetryAfter(Duration),
    /// Stay on home.
    StayHome,
}

/// Where a broadcast publishes.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Target {
    pub relay_url: String,
    pub pins: Vec<CertHash>,
    /// `lr=1` on the session's CONNECTs: only toward home.
    pub follow_offers: bool,
    /// `wall=hidden` on the publish: the broadcaster hid from this PoP's
    /// wall.
    pub wall_hidden: bool,
    /// The PoP switched to, when it is one.
    pub pop: Option<Switch>,
}

/// The local-relay state of one app session.
#[derive(Debug)]
pub struct LocalSwitch {
    pub policy: SwitchPolicy,
    /// The PoP the next broadcast publishes on, while its offer is valid.
    pub held: Option<HeldSwitch>,
    /// The offer whose candidates are being probed, and its retry state.
    probing: Option<(Offer, PromptRetry)>,
    /// An offer arrived while live: the window says the next broadcast
    /// will use it.
    pub next_broadcast: bool,
    /// The PoP this broadcast publishes on.
    pub live_on: Option<Switch>,
    /// The PoP's latest `0x18` on this broadcast's publish session.
    pub info: Option<LocalRelay>,
    /// A Hide or Show is on its way.
    pub wall_busy: bool,
    /// Retry once after a failed probe (macOS: the local-network prompt).
    apple: bool,
}

impl LocalSwitch {
    pub fn new<I: IntoIterator<Item = String>>(opt_outs: I, apple: bool) -> Self {
        Self {
            policy: SwitchPolicy::with_opt_outs(opt_outs),
            held: None,
            probing: None,
            next_broadcast: false,
            live_on: None,
            info: None,
            wall_busy: false,
            apple,
        }
    }

    /// The held switch, if its offer is still valid at `now`.
    pub fn held_at(&self, now: Instant) -> Option<&Switch> {
        self.held
            .as_ref()
            .filter(|h| h.live_at(now))
            .map(|h| &h.switch)
    }

    /// A fresh offer, from home's probe or a live publish session.
    pub fn on_offer(&mut self, offer: Offer, live: bool, now: Instant) -> OfferStep {
        match self.policy.decide(&offer, live) {
            Verdict::Ignore => return OfferStep::Ignore,
            Verdict::Probe | Verdict::NextBroadcast => {}
        }
        if let Some(h) = self.held.as_mut()
            && h.switch.handle == offer.handle
            && h.live_at(now)
        {
            h.expires_at = now + offer.validity;
            if live && self.live_on.is_none() {
                self.next_broadcast = true;
            }
            return OfferStep::Renewed;
        }
        if self.probing.is_some() {
            return OfferStep::Ignore;
        }
        let mut retry = PromptRetry::new(true);
        retry.start();
        self.probing = Some((offer.clone(), retry));
        OfferStep::Probe(offer)
    }

    /// The offer being probed, for a retry.
    pub fn probing(&self) -> Option<&Offer> {
        self.probing.as_ref().map(|(o, _)| o)
    }

    /// The probe of the offer's candidates came back.
    pub fn on_probed(&mut self, found: Option<Switch>, live: bool, now: Instant) -> ProbedStep {
        let Some((offer, mut retry)) = self.probing.take() else {
            return ProbedStep::StayHome;
        };
        if let Some(s) = found {
            if s.handle != offer.handle || self.policy.decide(&offer, false) == Verdict::Ignore {
                // Opted out while it was probing.
                return ProbedStep::StayHome;
            }
            self.held = Some(HeldSwitch::new(s, offer.validity, now));
            self.next_broadcast = live && self.live_on.is_none();
            return ProbedStep::Switched;
        }
        if self.apple
            && let DialStep::RetryAfter(d) = retry.failed()
        {
            self.probing = Some((offer, retry));
            return ProbedStep::RetryAfter(d);
        }
        self.policy.mark_unreachable(&offer.handle);
        ProbedStep::StayHome
    }

    /// A retry's delay passed: the offer to probe again, if still owed.
    pub fn retry_due(&mut self) -> Option<Offer> {
        let (offer, retry) = self.probing.as_mut()?;
        match retry.retry_due() {
            DialStep::Dial => Some(offer.clone()),
            _ => None,
        }
    }

    /// Where the next broadcast publishes: the held PoP, else home.
    pub fn target(
        &self,
        home_url: &str,
        home_pins: Vec<CertHash>,
        wall_hidden: &[String],
        now: Instant,
    ) -> Target {
        match self.held_at(now) {
            Some(s) => Target {
                relay_url: s.url.clone(),
                pins: s.hashes.clone(),
                follow_offers: false,
                wall_hidden: wall_hidden.contains(&s.handle),
                pop: Some(s.clone()),
            },
            None => Target {
                relay_url: home_url.to_owned(),
                pins: home_pins,
                follow_offers: true,
                wall_hidden: false,
                pop: None,
            },
        }
    }

    /// A broadcast started on `target`.
    pub fn on_start(&mut self, target: &Target) {
        self.live_on = target.pop.clone();
        self.next_broadcast = false;
        self.info = None;
        self.wall_busy = false;
    }

    /// The broadcast ended.
    pub fn on_end(&mut self) {
        self.live_on = None;
        self.info = None;
        self.wall_busy = false;
    }

    /// The PoP's session would not take the broadcast: back to home for
    /// the rest of the app session.
    pub fn on_pop_refused(&mut self) {
        if let Some(h) = self.held.take() {
            self.policy.mark_unreachable(&h.switch.handle);
        }
        self.live_on = None;
    }

    /// The chip's "Use … instead": the handle to remember. The held PoP is
    /// dropped; a broadcast already on it carries on.
    pub fn opt_out(&mut self, now: Instant) -> Option<String> {
        let handle = self.held_at(now)?.handle.clone();
        self.policy.opt_out(&handle);
        self.held = None;
        self.next_broadcast = false;
        Some(handle)
    }

    /// The chip on the Ready page: "● Assembly 2026 · local relay" while a
    /// PoP is held and nothing is live (empty otherwise).
    pub fn chip(&self, live: bool, now: Instant) -> String {
        if live {
            return String::new();
        }
        self.held_at(now)
            .map(|s| format!("{} · local relay", s.name))
            .unwrap_or_default()
    }

    /// The live line (§7.3): "Live on Assembly 2026's local relay", or an
    /// offer that came while live on home.
    pub fn live_line(&self, now: Instant) -> String {
        if let Some(s) = &self.live_on {
            return format!("Live on {}'s local relay", s.name);
        }
        match (self.next_broadcast, self.held_at(now)) {
            (true, Some(s)) => format!(
                "{} has a local relay. Your next broadcast will use it.",
                s.name
            ),
            _ => String::new(),
        }
    }

    /// The wall line's state: 0 no wall, 1 on the party wall (Hide), 2
    /// hidden from it (Show). Only a broadcast on a PoP with a wall has one.
    pub fn wall_state(&self) -> i32 {
        match (&self.live_on, &self.info) {
            (Some(_), Some(i)) if !i.wall_code.is_empty() => {
                if i.on_wall {
                    1
                } else {
                    2
                }
            }
            _ => 0,
        }
    }
}

/// Adds or removes `handle` in a remembered set; whether it changed.
pub fn remember(set: &mut Vec<String>, handle: &str, on: bool) -> bool {
    let has = set.iter().any(|h| h == handle);
    if on && !has {
        set.push(handle.to_owned());
        true
    } else if !on && has {
        set.retain(|h| h != handle);
        true
    } else {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gawk_engine::localrelay::Candidate;

    fn offer(handle: &str) -> Offer {
        Offer {
            handle: handle.into(),
            name: "Assembly 2026".into(),
            validity: Duration::from_secs(3600),
            candidates: vec![Candidate {
                url: "https://192.168.1.20:4433".into(),
                hashes: vec![[7; 32]],
            }],
        }
    }

    fn switch(handle: &str) -> Switch {
        Switch {
            handle: handle.into(),
            name: "Assembly 2026".into(),
            url: "https://192.168.1.20:4433".into(),
            hashes: vec![[7; 32]],
            rtt_ms: 2,
        }
    }

    const HOME: &str = "https://api.gawk.ioio.fi:4433";

    // item 43.3: a fresh offer on the Ready page is probed, and the next
    // broadcast publishes on the PoP, pinned, without `lr=1`.
    #[test]
    fn a_fresh_offer_switches_the_next_broadcast() {
        let now = Instant::now();
        let mut s = LocalSwitch::new([], false);
        assert_eq!(s.target(HOME, vec![], &[], now).relay_url, HOME);
        assert!(
            s.target(HOME, vec![], &[], now).follow_offers,
            "lr=1 to home"
        );

        assert_eq!(
            s.on_offer(offer("h"), false, now),
            OfferStep::Probe(offer("h"))
        );
        assert_eq!(
            s.on_offer(offer("h"), false, now),
            OfferStep::Ignore,
            "in flight"
        );
        assert_eq!(
            s.on_probed(Some(switch("h")), false, now),
            ProbedStep::Switched
        );
        assert_eq!(s.chip(false, now), "Assembly 2026 · local relay");

        let t = s.target(HOME, vec![], &[], now);
        assert_eq!(t.relay_url, "https://192.168.1.20:4433");
        assert_eq!(t.pins, vec![[7; 32]]);
        assert!(!t.follow_offers && !t.wall_hidden);
        assert!(s.target(HOME, vec![], &["h".into()], now).wall_hidden);

        s.on_start(&t);
        assert_eq!(s.chip(true, now), "");
        assert_eq!(s.live_line(now), "Live on Assembly 2026's local relay");
    }

    // item 43.1: the switch lasts the offer's validity only; a fresh offer
    // from the next home probe renews it without probing again.
    #[test]
    fn a_held_switch_expires_unless_renewed() {
        let now = Instant::now();
        let mut s = LocalSwitch::new([], false);
        s.on_offer(offer("h"), false, now);
        s.on_probed(Some(switch("h")), false, now);
        let later = now + Duration::from_secs(3000);
        assert_eq!(s.on_offer(offer("h"), false, later), OfferStep::Renewed);
        let after = now + Duration::from_secs(3700);
        assert!(s.held_at(after).is_some(), "renewed");
        let gone = later + Duration::from_secs(3601);
        assert_eq!(s.target(HOME, vec![], &[], gone).relay_url, HOME);
        assert_eq!(s.chip(false, gone), "");
    }

    // item 43.3: a live broadcast is never moved; the window says the next
    // one will use the PoP.
    #[test]
    fn an_offer_while_live_is_for_the_next_broadcast() {
        let now = Instant::now();
        let mut s = LocalSwitch::new([], false);
        s.on_start(&s.target(HOME, vec![], &[], now));
        assert!(matches!(
            s.on_offer(offer("h"), true, now),
            OfferStep::Probe(_)
        ));
        assert_eq!(
            s.on_probed(Some(switch("h")), true, now),
            ProbedStep::Switched
        );
        assert_eq!(
            s.live_line(now),
            "Assembly 2026 has a local relay. Your next broadcast will use it."
        );
        s.on_end();
        assert_eq!(
            s.target(HOME, vec![], &[], now).relay_url,
            "https://192.168.1.20:4433"
        );
    }

    // items 43.5 and 43.6: failure stays home for the app session with no
    // retry loop; an opt-out is remembered and ignores later offers.
    #[test]
    fn failure_and_opt_out_stay_home() {
        let now = Instant::now();
        let mut s = LocalSwitch::new([], false);
        s.on_offer(offer("h"), false, now);
        assert_eq!(s.on_probed(None, false, now), ProbedStep::StayHome);
        assert_eq!(s.on_offer(offer("h"), false, now), OfferStep::Ignore);

        let mut s = LocalSwitch::new([], false);
        s.on_offer(offer("h"), false, now);
        s.on_probed(Some(switch("h")), false, now);
        assert_eq!(s.opt_out(now), Some("h".into()));
        assert_eq!(s.target(HOME, vec![], &[], now).relay_url, HOME);
        assert_eq!(s.on_offer(offer("h"), false, now), OfferStep::Ignore);
        let seeded = &mut LocalSwitch::new(["h".to_owned()], false);
        assert_eq!(seeded.on_offer(offer("h"), false, now), OfferStep::Ignore);
        assert!(matches!(
            seeded.on_offer(offer("other"), false, now),
            OfferStep::Probe(_)
        ));
    }

    // item 44: on macOS the first failure is retried once (the prompt), the
    // second stays home.
    #[test]
    fn an_apple_desktop_retries_once() {
        let now = Instant::now();
        let mut s = LocalSwitch::new([], true);
        s.on_offer(offer("h"), false, now);
        let ProbedStep::RetryAfter(_) = s.on_probed(None, false, now) else {
            panic!("a retry");
        };
        assert_eq!(s.retry_due(), Some(offer("h")));
        assert_eq!(s.on_probed(None, false, now), ProbedStep::StayHome);
        assert_eq!(s.retry_due(), None);
        assert_eq!(s.on_offer(offer("h"), false, now), OfferStep::Ignore);
    }

    #[test]
    fn a_refused_pop_session_goes_home() {
        let now = Instant::now();
        let mut s = LocalSwitch::new([], false);
        s.on_offer(offer("h"), false, now);
        s.on_probed(Some(switch("h")), false, now);
        s.on_pop_refused();
        assert_eq!(s.target(HOME, vec![], &[], now).relay_url, HOME);
        assert_eq!(s.on_offer(offer("h"), false, now), OfferStep::Ignore);
    }

    // §7.3 "On the party wall · Hide": only a broadcast on a PoP with a
    // wall has the line, and it follows the PoP's 0x18.
    #[test]
    fn the_wall_line_follows_the_pops_info() {
        let now = Instant::now();
        let mut s = LocalSwitch::new([], false);
        assert_eq!(s.wall_state(), 0);
        s.on_offer(offer("h"), false, now);
        s.on_probed(Some(switch("h")), false, now);
        s.on_start(&s.target(HOME, vec![], &[], now));
        assert_eq!(s.wall_state(), 0, "no info yet");
        s.info = Some(LocalRelay {
            linked: true,
            on_wall: true,
            name: Some("Assembly 2026".into()),
            home_origin: "https://gawk.ioio.fi".into(),
            wall_code: "assembly-2026".into(),
        });
        assert_eq!(s.wall_state(), 1);
        s.info.as_mut().unwrap().on_wall = false;
        assert_eq!(s.wall_state(), 2);
        s.info.as_mut().unwrap().wall_code.clear();
        assert_eq!(s.wall_state(), 0);
    }

    #[test]
    fn remember_adds_and_removes_once() {
        let mut v = vec![];
        assert!(remember(&mut v, "h", true));
        assert!(!remember(&mut v, "h", true));
        assert_eq!(v, vec!["h".to_owned()]);
        assert!(remember(&mut v, "h", false));
        assert!(v.is_empty());
    }
}
