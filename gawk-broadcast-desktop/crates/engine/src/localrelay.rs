//! The switch to a party's local relay (R74 PP14, docs/76 §6.12 items 43–45):
//! what a native client does with a home relay's `LocalRelayOffer` (`0x19`)
//! and a PoP's `LocalRelayInfo` (`0x18`).
//!
//! - **Only on a fresh offer** (item 43.1): an offer is acted on while its
//!   validity lasts, and nothing remembered ever starts a switch. What is
//!   remembered is consent only: the per-handle opt-out (item 43.6).
//! - **Candidates probed in parallel**, first success wins (item 43.2), each
//!   pinned to the hashes it carries ([`crate::probe::probe_pinned`]).
//! - **A broadcaster switches only before going live** (item 43.3):
//!   [`SwitchPolicy::decide`] answers [`Verdict::NextBroadcast`] for a live
//!   one, which the window words as "your next broadcast will use it".
//! - **Failure stays on home** and marks the PoP unreachable for this app
//!   session, with no retry loop (item 43.5) — beyond the one retry an Apple
//!   platform owes the local-network prompt ([`PromptRetry`], item 44).
//! - Clients that follow offers say so with `lr=1` on every CONNECT to home
//!   (item 43.7, [`with_follow_offers`]).
//!
//! This module is shared by the desktop shells and the iOS core; neither
//! re-implements it.

use crate::probe::{self, ProbeResult};
use crate::transport::CertHash;
use gawk_wire as wire;
use std::collections::HashSet;
use std::time::Duration;

/// The query parameter a client that will follow an offer adds to every
/// CONNECT to home (docs/76 item 21): home may hold a matching session's
/// demand for `-pop-offer-defer` while the client switches.
pub const FOLLOW_OFFERS_PARAM: (&str, &str) = ("lr", "1");

/// The query parameter a broadcaster who hid from the party wall adds to
/// its next `/publish` on that PoP (docs/76 item 34), so it never flashes
/// onto the wall.
pub const WALL_HIDDEN_PARAM: (&str, &str) = ("wall", "hidden");

/// What a PoP says about itself on a session (`0x18`), owned and with the
/// display name sanitized for the window.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct LocalRelay {
    /// The PoP is linked to its home fleet: share links are home links.
    pub linked: bool,
    /// This publish session's broadcast is on the party wall.
    pub on_wall: bool,
    /// The PoP's display name, sanitized; `None` when it sent none.
    pub name: Option<String>,
    /// The home fleet's app origin; empty when unlinked.
    pub home_origin: String,
    /// The party wall's room code; empty without a wall.
    pub wall_code: String,
}

impl LocalRelay {
    /// The owned form of a parsed `0x18`.
    pub fn from_wire(m: &wire::LocalRelayInfo<'_>) -> Self {
        Self {
            linked: m.linked,
            on_wall: m.on_wall,
            name: probe::sanitize_name(m.name),
            home_origin: m.home_origin.to_owned(),
            wall_code: m.wall_code.to_owned(),
        }
    }

    /// Parses one `0x18` uni-stream payload; `None` for anything else.
    pub fn parse(msg: &[u8]) -> Option<Self> {
        wire::parse_local_relay_info(msg)
            .ok()
            .map(|m| Self::from_wire(&m))
    }
}

/// One way to reach the PoP an offer names.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Candidate {
    /// The relay origin to dial (`https://host[:port]`), path dropped.
    pub url: String,
    /// The certificate hashes to pin; empty trusts the system store (the
    /// named tier's public certificate).
    pub hashes: Vec<CertHash>,
}

/// A home relay's suggestion to use a party's local relay (`0x19`), owned.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Offer {
    /// The opaque PoP handle the opt-out is remembered against.
    pub handle: String,
    /// The PoP's display name, sanitized; the handle's stand-in is never
    /// shown, so a nameless PoP reads "A local relay".
    pub name: String,
    /// How long the offer may be acted on.
    pub validity: Duration,
    /// In home's preference order; never empty.
    pub candidates: Vec<Candidate>,
}

/// What the window calls a PoP that sent no name.
pub const UNNAMED_LOCAL_RELAY: &str = "A local relay";

impl Offer {
    /// The owned form of a parsed `0x19`. Candidates whose URL has no
    /// usable origin are left out; an offer left with none is `None`.
    pub fn from_wire(m: &wire::LocalRelayOffer<'_>) -> Option<Self> {
        let candidates: Vec<Candidate> = m
            .candidates
            .iter()
            .filter_map(|c| {
                Some(Candidate {
                    url: candidate_origin(c.url)?,
                    hashes: c.hashes.iter().map(|h| **h).collect(),
                })
            })
            .collect();
        if candidates.is_empty() {
            return None;
        }
        Some(Self {
            handle: m.handle.to_owned(),
            name: probe::sanitize_name(m.name).unwrap_or_else(|| UNNAMED_LOCAL_RELAY.to_owned()),
            validity: Duration::from_secs(u64::from(m.validity)),
            candidates,
        })
    }

    /// Parses one `0x19` uni-stream payload; `None` for anything else.
    pub fn parse(msg: &[u8]) -> Option<Self> {
        wire::parse_local_relay_offer(msg)
            .ok()
            .and_then(|m| Self::from_wire(&m))
    }
}

/// A candidate URL reduced to the relay origin a session dials: the wire
/// already refused userinfo, queries and fragments; a path is dropped,
/// since every relay route is rooted.
fn candidate_origin(raw: &str) -> Option<String> {
    let u = url::Url::parse(raw).ok()?;
    if u.scheme() != "https" || u.host_str().is_none_or(str::is_empty) {
        return None;
    }
    Some(u.origin().ascii_serialization())
}

/// The candidate that answered: where the client switches to.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Switch {
    pub handle: String,
    pub name: String,
    /// The relay origin to use in place of home.
    pub url: String,
    /// Its pins (empty: the system store).
    pub hashes: Vec<CertHash>,
    /// The winning probe's round trip.
    pub rtt_ms: u32,
}

/// Probes every candidate of `offer` at once over `/echo` and returns the
/// first that answers (docs/76 item 43.2). `None` when none does: the
/// caller stays on home and marks the PoP unreachable.
pub async fn probe_candidates(offer: &Offer, origin: &str) -> Option<Switch> {
    probe_candidates_with(offer, |url, hashes| {
        let origin = origin.to_owned();
        async move { probe::probe_pinned(&url, &origin, false, &hashes).await }
    })
    .await
}

/// [`probe_candidates`] with the probe injected: the seam the tests script.
/// Every probe runs concurrently; the first `Ok` wins and the rest are
/// dropped (cancelled) with it.
pub async fn probe_candidates_with<F, Fut>(offer: &Offer, probe_one: F) -> Option<Switch>
where
    F: Fn(String, Vec<CertHash>) -> Fut,
    Fut: std::future::Future<Output = ProbeResult> + Send + 'static,
{
    let mut set = tokio::task::JoinSet::new();
    for (i, c) in offer.candidates.iter().enumerate() {
        let fut = probe_one(c.url.clone(), c.hashes.clone());
        set.spawn(async move { (i, fut.await) });
    }
    while let Some(joined) = set.join_next().await {
        if let Ok((i, ProbeResult::Ok { rtt_ms, .. })) = joined {
            set.abort_all();
            let c = &offer.candidates[i];
            return Some(Switch {
                handle: offer.handle.clone(),
                name: offer.name.clone(),
                url: c.url.clone(),
                hashes: c.hashes.clone(),
                rtt_ms,
            });
        }
    }
    None
}

/// `url` with `lr=1` added (docs/76 item 21). A URL that does not parse is
/// returned unchanged: the dial reports it.
pub fn with_follow_offers(url: &str) -> String {
    with_param(url, FOLLOW_OFFERS_PARAM)
}

/// `url` with `wall=hidden` added (docs/76 item 34).
pub fn with_wall_hidden(url: &str) -> String {
    with_param(url, WALL_HIDDEN_PARAM)
}

fn with_param(url: &str, (k, v): (&str, &str)) -> String {
    let Ok(mut u) = url::Url::parse(url) else {
        return url.to_owned();
    };
    if u.query_pairs().any(|(key, _)| key == k) {
        return url.to_owned();
    }
    u.query_pairs_mut().append_pair(k, v);
    u.into()
}

/// What to do with a fresh offer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Verdict {
    /// Opted out, or already found unreachable this app session: stay on
    /// home and say nothing.
    Ignore,
    /// Probe the candidates and switch to the first that answers.
    Probe,
    /// Live already: a live broadcast is never moved. Probe for the next
    /// one and tell the broadcaster "Your next broadcast will use it."
    NextBroadcast,
}

/// The consent and failure memory the switch consults (docs/76 items 43.5
/// and 43.6). `opted_out` is the persisted per-handle "Use … instead";
/// `unreachable` lives for the app session only.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SwitchPolicy {
    pub opted_out: HashSet<String>,
    pub unreachable: HashSet<String>,
}

impl SwitchPolicy {
    /// A policy seeded with the persisted opt-outs.
    pub fn with_opt_outs<I: IntoIterator<Item = String>>(opt_outs: I) -> Self {
        Self {
            opted_out: opt_outs.into_iter().collect(),
            unreachable: HashSet::new(),
        }
    }

    /// The verdict on a fresh `offer`; `live` is whether a broadcast is
    /// running (a broadcaster) — a viewer passes `false`.
    pub fn decide(&self, offer: &Offer, live: bool) -> Verdict {
        if self.opted_out.contains(&offer.handle) || self.unreachable.contains(&offer.handle) {
            Verdict::Ignore
        } else if live {
            Verdict::NextBroadcast
        } else {
            Verdict::Probe
        }
    }

    /// Every candidate failed: stay on home for the rest of the session.
    pub fn mark_unreachable(&mut self, handle: &str) {
        self.unreachable.insert(handle.to_owned());
    }

    /// The chip's "Use … instead": remembered against the handle until the
    /// user picks the PoP again.
    pub fn opt_out(&mut self, handle: &str) {
        self.opted_out.insert(handle.to_owned());
    }

    /// The user picked the PoP again.
    pub fn opt_in(&mut self, handle: &str) {
        self.opted_out.remove(handle);
        self.unreachable.remove(handle);
    }
}

/// An offer the client acted on, kept only while its validity lasts: a
/// remembered PoP never starts a switch on its own (item 43.1).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HeldSwitch {
    pub switch: Switch,
    pub expires_at: std::time::Instant,
}

impl HeldSwitch {
    pub fn new(switch: Switch, validity: Duration, now: std::time::Instant) -> Self {
        Self {
            switch,
            expires_at: now + validity,
        }
    }

    pub fn live_at(&self, now: std::time::Instant) -> bool {
        now < self.expires_at
    }
}

/// How long after a first failed dial the one retry waits when the app
/// never left the foreground (the prompt may have been answered at once, or
/// none was shown).
pub const PROMPT_RETRY_DELAY: Duration = Duration::from_secs(3);

/// The next step of a switch dial under Apple's local-network privacy
/// (docs/76 item 44, TN3179).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DialStep {
    /// Probe the candidates now.
    Dial,
    /// Do nothing until [`PromptRetry::set_foreground`] or
    /// [`PromptRetry::retry_due`] says otherwise.
    Wait,
    /// Retry after [`PROMPT_RETRY_DELAY`] unless the app goes inactive and
    /// back first.
    RetryAfter(Duration),
    /// Stay on home: mark the PoP unreachable.
    GiveUp,
    /// The switch answered; nothing more to do.
    Done,
}

/// The first dial to a LAN address may be refused before the user answers
/// the local-network prompt, so a switch retries **once** after the prompt;
/// and while the app is backgrounded it never dials (TN3179, docs/76 item
/// 44). Platform-neutral state: the iOS app feeds it scene phases, a
/// desktop shell stays foreground.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PromptRetry {
    foreground: bool,
    attempts: u8,
    /// A dial is owed as soon as the app is in the foreground.
    owed: bool,
    /// The app went inactive after the first failure (the prompt showed).
    left_after_failure: bool,
    done: bool,
}

impl PromptRetry {
    /// The most dials one offer gets: the first, and one retry.
    pub const MAX_ATTEMPTS: u8 = 2;

    pub fn new(foreground: bool) -> Self {
        Self {
            foreground,
            attempts: 0,
            owed: false,
            left_after_failure: false,
            done: false,
        }
    }

    /// Dials made so far.
    pub fn attempts(&self) -> u8 {
        self.attempts
    }

    /// A fresh offer to act on.
    pub fn start(&mut self) -> DialStep {
        self.owed = true;
        self.next()
    }

    /// The probe of every candidate failed.
    pub fn failed(&mut self) -> DialStep {
        if self.attempts >= Self::MAX_ATTEMPTS {
            self.done = true;
            return DialStep::GiveUp;
        }
        self.owed = true;
        self.left_after_failure = false;
        if self.foreground {
            DialStep::RetryAfter(PROMPT_RETRY_DELAY)
        } else {
            DialStep::Wait
        }
    }

    /// A candidate answered.
    pub fn succeeded(&mut self) -> DialStep {
        self.done = true;
        self.owed = false;
        DialStep::Done
    }

    /// The app became active (`true`) or inactive/backgrounded (`false`).
    /// Coming back after the first failure is the prompt answered: retry
    /// at once.
    pub fn set_foreground(&mut self, foreground: bool) -> DialStep {
        let was = self.foreground;
        self.foreground = foreground;
        if !foreground {
            if self.attempts > 0 {
                self.left_after_failure = true;
            }
            return DialStep::Wait;
        }
        if was && !self.left_after_failure {
            return DialStep::Wait;
        }
        self.next()
    }

    /// The [`DialStep::RetryAfter`] delay passed.
    pub fn retry_due(&mut self) -> DialStep {
        self.next()
    }

    fn next(&mut self) -> DialStep {
        if self.done || !self.owed {
            return DialStep::Wait;
        }
        if !self.foreground {
            return DialStep::Wait;
        }
        if self.attempts >= Self::MAX_ATTEMPTS {
            self.done = true;
            return DialStep::GiveUp;
        }
        self.owed = false;
        self.left_after_failure = false;
        self.attempts += 1;
        DialStep::Dial
    }
}

// --- the party wall's Hide / Show (docs/76 item 34) --------------------------

/// How long a wall command waits for the PoP to answer the join, and then
/// for the command to leave, before it gives up.
pub const WALL_COMMAND_TIMEOUT: Duration = Duration::from_secs(5);

/// The room URL a wall command dials on the PoP: the wall's `/room/{code}`
/// with this broadcast's ID and resume token, the PoP's proof that the
/// sender publishes it there (it never travels home).
pub fn wall_command_url(
    pop_url: &str,
    wall_code: &str,
    broadcast_id: &str,
    resume_token_hex: &str,
) -> Result<String, String> {
    if broadcast_id.is_empty() || resume_token_hex.is_empty() {
        return Err("the broadcast's identity is not known yet".into());
    }
    let mut url = url::Url::parse(&crate::room::room_url(pop_url, wall_code, "", "")?)
        .map_err(|e| e.to_string())?;
    url.query_pairs_mut()
        .append_pair("broadcast", broadcast_id)
        .append_pair("resume", resume_token_hex);
    Ok(url.into())
}

/// Hides this broadcast from the PoP's party wall (`show == false`) or
/// shows it again: an R42 Detach or Attach of its own broadcast on the
/// wall, which the PoP checks against its resume token and forwards home
/// vouched. The broadcast itself is untouched; the PoP's next `0x18` on the
/// publish session carries the new on-wall state.
pub async fn wall_command(w: &WallTarget, show: bool) -> Result<(), String> {
    let url = wall_command_url(
        &w.pop_url,
        &w.wall_code,
        &w.broadcast_id,
        &w.resume_token_hex,
    )?;
    let token = hex_decode(&w.resume_token_hex).ok_or("the resume token is not hex")?;
    let conn = tokio::time::timeout(
        WALL_COMMAND_TIMEOUT,
        crate::transport::dial_room_pinned(&url, &w.origin, false, &w.pins),
    )
    .await
    .map_err(|_| "the local relay did not answer".to_owned())?
    .map_err(|e| crate::room::room_dial_message(&e))?;
    wall_command_on(&conn, &w.broadcast_id, &token, &w.label, show).await
}

/// Which broadcast a wall command is for, and the PoP it is published on.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct WallTarget {
    /// The PoP's relay origin (the switched-to candidate).
    pub pop_url: String,
    pub origin: String,
    /// The PoP's pins (empty: the system store).
    pub pins: Vec<CertHash>,
    /// The wall's room code, from the PoP's `0x18`.
    pub wall_code: String,
    pub broadcast_id: String,
    pub resume_token_hex: String,
    /// The tile label a Show restores (the broadcaster's nickname).
    pub label: String,
}

/// [`wall_command`] on an open room control stream: the seam the tests
/// script. Hello, then (once the PoP answered the join, so the command is
/// never sent into a refused session) the command.
pub async fn wall_command_on(
    conn: &dyn crate::room::RoomConn,
    broadcast_id: &str,
    resume_token: &[u8],
    label: &str,
    show: bool,
) -> Result<(), String> {
    let run = async {
        conn.write(&crate::room::hello_record("")?).await?;
        crate::room::read_record(conn).await?;
        let cmd = if show {
            wire::RoomCommand {
                kind: wire::ROOM_COMMAND_ATTACH,
                broadcast_id: broadcast_id.to_owned(),
                resume_token,
                label: crate::room::truncate_utf8(label, wire::MAX_ROOM_LABEL_LEN),
                ..wire::RoomCommand::default()
            }
        } else {
            wire::RoomCommand {
                kind: wire::ROOM_COMMAND_DETACH,
                broadcast_id: broadcast_id.to_owned(),
                ..wire::RoomCommand::default()
            }
        };
        conn.write(&crate::room::command_record(&cmd)?).await
    };
    tokio::time::timeout(WALL_COMMAND_TIMEOUT, run)
        .await
        .map_err(|_| "the local relay did not answer".to_owned())?
}

fn hex_decode(s: &str) -> Option<Vec<u8>> {
    if !s.len().is_multiple_of(2) {
        return None;
    }
    (0..s.len())
        .step_by(2)
        .map(|i| u8::from_str_radix(s.get(i..i + 2)?, 16).ok())
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The contract's 0x19 vector (docs/76 §9): "Assembly 2026", 3600 s,
    /// a named candidate and an IP-literal one with two hashes.
    pub(crate) fn vector() -> Vec<u8> {
        let mut hex = String::new();
        hex.push_str("011900");
        hex.push_str("24");
        hex.push_str("36663163326433652d346135622d346336642d386537662d303132333435363738396162");
        hex.push_str("0d417373656d626c792032303236");
        hex.push_str("00000e10");
        hex.push_str("02");
        hex.push_str("0026");
        hex.push_str(
            "68747470733a2f2f702d376b326d39782e706f702e6761776b2e696f696f2e66693a34343333",
        );
        hex.push_str("00");
        hex.push_str("0019");
        hex.push_str("68747470733a2f2f3139322e3136382e312e32303a34343333");
        hex.push_str("02");
        for b in 0u8..64 {
            hex.push_str(&format!("{b:02x}"));
        }
        (0..hex.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&hex[i..i + 2], 16).unwrap())
            .collect()
    }

    #[test]
    fn the_offer_vector_parses_into_its_candidates() {
        let offer = Offer::parse(&vector()).expect("offer");
        assert_eq!(offer.handle, "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab");
        assert_eq!(offer.name, "Assembly 2026");
        assert_eq!(offer.validity, Duration::from_secs(3600));
        assert_eq!(offer.candidates.len(), 2);
        assert_eq!(
            offer.candidates[0].url,
            "https://p-7k2m9x.pop.gawk.ioio.fi:4433"
        );
        assert!(offer.candidates[0].hashes.is_empty());
        assert_eq!(offer.candidates[1].url, "https://192.168.1.20:4433");
        let mut h0 = [0u8; 32];
        let mut h1 = [0u8; 32];
        for i in 0..32 {
            h0[i] = i as u8;
            h1[i] = 32 + i as u8;
        }
        assert_eq!(offer.candidates[1].hashes, vec![h0, h1]);
    }

    #[test]
    fn a_malformed_offer_or_another_type_is_none() {
        let mut bad = vector();
        bad[2] = 1; // flags must be 0
        assert_eq!(Offer::parse(&bad), None);
        assert_eq!(Offer::parse(&[0x01, 0x11, 0x00]), None);
    }

    #[test]
    fn a_nameless_offer_reads_as_a_local_relay() {
        let mut msg = Vec::new();
        let h = [7u8; 32];
        wire::append_local_relay_offer(
            &mut msg,
            &wire::LocalRelayOffer {
                handle: "h",
                name: " \u{202e} ",
                validity: 60,
                candidates: vec![wire::LocalRelayCandidate {
                    url: "https://10.0.0.5:4433/ignored/path",
                    hashes: vec![&h],
                }],
            },
        )
        .unwrap();
        let offer = Offer::parse(&msg).unwrap();
        assert_eq!(offer.name, UNNAMED_LOCAL_RELAY);
        assert_eq!(offer.candidates[0].url, "https://10.0.0.5:4433");
    }

    #[test]
    fn the_info_vectors_parse_owned_and_sanitized() {
        let mut msg = Vec::new();
        wire::append_local_relay_info(
            &mut msg,
            &wire::LocalRelayInfo {
                linked: true,
                on_wall: true,
                name: "Assembly\u{202e} 2026",
                home_origin: "https://gawk.ioio.fi",
                wall_code: "assembly-2026",
            },
        )
        .unwrap();
        assert_eq!(
            LocalRelay::parse(&msg),
            Some(LocalRelay {
                linked: true,
                on_wall: true,
                name: Some("Assembly 2026".into()),
                home_origin: "https://gawk.ioio.fi".into(),
                wall_code: "assembly-2026".into(),
            })
        );
        assert_eq!(LocalRelay::parse(&vector()), None, "an offer is not info");
    }

    #[test]
    fn lr_and_wall_hidden_are_added_once() {
        assert_eq!(
            with_follow_offers("https://api.gawk.ioio.fi:4433/echo"),
            "https://api.gawk.ioio.fi:4433/echo?lr=1"
        );
        assert_eq!(
            with_follow_offers("https://h:4433/publish?secret=s&app=x"),
            "https://h:4433/publish?secret=s&app=x&lr=1"
        );
        assert_eq!(
            with_follow_offers("https://h:4433/publish?lr=1"),
            "https://h:4433/publish?lr=1"
        );
        assert_eq!(
            with_wall_hidden("https://192.168.1.20:4433/publish"),
            "https://192.168.1.20:4433/publish?wall=hidden"
        );
        assert_eq!(with_follow_offers("not a url"), "not a url");
    }

    #[test]
    fn opt_outs_and_unreachable_pops_are_ignored() {
        let offer = Offer::parse(&vector()).unwrap();
        let mut p = SwitchPolicy::default();
        assert_eq!(p.decide(&offer, false), Verdict::Probe);
        assert_eq!(p.decide(&offer, true), Verdict::NextBroadcast);
        p.mark_unreachable(&offer.handle);
        assert_eq!(p.decide(&offer, false), Verdict::Ignore);
        p.opt_in(&offer.handle);
        p.opt_out(&offer.handle);
        assert_eq!(p.decide(&offer, false), Verdict::Ignore);
        assert_eq!(p.decide(&offer, true), Verdict::Ignore);
        p.opt_in(&offer.handle);
        assert_eq!(p.decide(&offer, false), Verdict::Probe);
        let seeded = SwitchPolicy::with_opt_outs([offer.handle.clone()]);
        assert_eq!(seeded.decide(&offer, false), Verdict::Ignore);
    }

    #[test]
    fn a_held_switch_lasts_its_validity_only() {
        let now = std::time::Instant::now();
        let s = Switch {
            handle: "h".into(),
            name: "n".into(),
            url: "https://10.0.0.5:4433".into(),
            hashes: vec![],
            rtt_ms: 3,
        };
        let held = HeldSwitch::new(s, Duration::from_secs(60), now);
        assert!(held.live_at(now + Duration::from_secs(59)));
        assert!(!held.live_at(now + Duration::from_secs(60)));
    }

    fn ok(rtt_ms: u32) -> ProbeResult {
        ProbeResult::Ok {
            rtt_ms,
            name: None,
            screens: false,
            local: None,
            offer: None,
        }
    }

    // item 43.2: in parallel, first success wins — a slow failure does not
    // hold up a fast success, and a slow success is not waited for.
    #[tokio::test(start_paused = true)]
    async fn the_first_candidate_to_answer_wins() {
        let offer = Offer::parse(&vector()).unwrap();
        let started = tokio::time::Instant::now();
        let got = probe_candidates_with(&offer, |url, hashes| async move {
            if url.contains("p-7k2m9x") {
                // The named host: venue DNS filters it, and it times out.
                tokio::time::sleep(Duration::from_secs(4)).await;
                ProbeResult::Failed
            } else {
                assert_eq!(hashes.len(), 2, "the literal is dialed pinned");
                tokio::time::sleep(Duration::from_millis(30)).await;
                ok(2)
            }
        })
        .await
        .expect("a switch");
        assert_eq!(got.url, "https://192.168.1.20:4433");
        assert_eq!(got.hashes.len(), 2);
        assert_eq!(got.rtt_ms, 2);
        assert_eq!(got.handle, offer.handle);
        assert!(
            started.elapsed() < Duration::from_secs(1),
            "not serialised behind the failing candidate"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn every_candidate_failing_is_none() {
        let offer = Offer::parse(&vector()).unwrap();
        let got = probe_candidates_with(&offer, |_, _| async { ProbeResult::Failed }).await;
        assert_eq!(got, None);
    }

    // --- the wall's Hide / Show ---

    /// A scripted room stream: what the client wrote, and the relay's
    /// records to read (none: the session closed).
    struct WallConn {
        written: std::sync::Mutex<Vec<Vec<u8>>>,
        inbox: std::sync::Mutex<Vec<u8>>,
    }

    impl crate::room::RoomConn for WallConn {
        fn write(&self, record: &[u8]) -> crate::relay::BoxFuture<'_, Result<(), String>> {
            self.written.lock().unwrap().push(record.to_vec());
            Box::pin(async { Ok(()) })
        }
        fn read(&self, n: usize) -> crate::relay::BoxFuture<'_, Result<Vec<u8>, String>> {
            let mut g = self.inbox.lock().unwrap();
            let r = if g.len() >= n {
                Ok(g.drain(..n).collect())
            } else {
                Err("closed".into())
            };
            Box::pin(async move { r })
        }
        fn closed(&self) -> crate::relay::BoxFuture<'_, crate::relay::SessionClose> {
            Box::pin(std::future::pending())
        }
    }

    fn wall_conn(answers: bool) -> WallConn {
        let mut inbox = Vec::new();
        if answers {
            // Any record will do: the join was answered.
            let mut msg = Vec::new();
            wire::append_room_command(
                &mut msg,
                &wire::RoomCommand {
                    kind: wire::ROOM_COMMAND_RESYNC,
                    ..wire::RoomCommand::default()
                },
            )
            .unwrap();
            wire::append_room_record(&mut inbox, &msg).unwrap();
        }
        WallConn {
            written: Default::default(),
            inbox: std::sync::Mutex::new(inbox),
        }
    }

    fn commands(conn: &WallConn) -> Vec<(u8, String, Vec<u8>, String)> {
        conn.written.lock().unwrap()[1..]
            .iter()
            .map(|rec| {
                let c = wire::parse_room_command(&rec[wire::ROOM_RECORD_HEADER_SIZE..]).unwrap();
                (
                    c.kind,
                    c.broadcast_id.clone(),
                    c.resume_token.to_vec(),
                    c.label.to_owned(),
                )
            })
            .collect()
    }

    #[test]
    fn the_wall_command_url_proves_the_broadcast_to_the_pop() {
        assert_eq!(
            wall_command_url(
                "https://192.168.1.20:4433",
                "assembly-2026",
                "K7XQ2M",
                "c0ffee"
            )
            .unwrap(),
            "https://192.168.1.20:4433/room/assembly-2026?broadcast=K7XQ2M&resume=c0ffee"
        );
        assert!(wall_command_url("https://192.168.1.20:4433", "w", "", "c0ffee").is_err());
        assert!(wall_command_url("https://192.168.1.20:4433", "w", "K7XQ2M", "").is_err());
    }

    // docs/76 item 34: Hide is a Detach of its own broadcast, Show an Attach
    // with the resume token and the label, each after the hello was answered.
    #[tokio::test]
    async fn hide_detaches_and_show_attaches_the_own_broadcast() {
        let token = [0xabu8; wire::RESUME_TOKEN_SIZE];
        let conn = wall_conn(true);
        wall_command_on(&conn, "K7XQ2M", &token, "Tuhis", false)
            .await
            .unwrap();
        let first = conn.written.lock().unwrap()[0].clone();
        assert_eq!(
            first[wire::ROOM_RECORD_HEADER_SIZE + 1],
            wire::TYPE_ROOM_HELLO
        );
        assert_eq!(
            commands(&conn),
            vec![(
                wire::ROOM_COMMAND_DETACH,
                "K7XQ2M".into(),
                vec![],
                String::new()
            )]
        );

        let conn = wall_conn(true);
        wall_command_on(&conn, "K7XQ2M", &token, "Tuhis", true)
            .await
            .unwrap();
        assert_eq!(
            commands(&conn),
            vec![(
                wire::ROOM_COMMAND_ATTACH,
                "K7XQ2M".into(),
                token.to_vec(),
                "Tuhis".into()
            )]
        );
    }

    #[tokio::test]
    async fn a_refused_join_sends_no_command() {
        let conn = wall_conn(false);
        assert!(
            wall_command_on(&conn, "K7XQ2M", &[0; 16], "", false)
                .await
                .is_err()
        );
        assert_eq!(conn.written.lock().unwrap().len(), 1, "the hello only");
    }

    // TN3179 (docs/76 item 44): one retry after the prompt, none while
    // backgrounded, never a loop.
    #[test]
    fn a_first_denied_dial_is_retried_once_when_the_prompt_is_answered() {
        let mut r = PromptRetry::new(true);
        assert_eq!(r.start(), DialStep::Dial);
        // The prompt shows: the app goes inactive, the dial fails.
        assert_eq!(r.set_foreground(false), DialStep::Wait);
        assert_eq!(r.failed(), DialStep::Wait, "inactive: no dial");
        // The user answers: the app is active again, and it retries at once.
        assert_eq!(r.set_foreground(true), DialStep::Dial);
        assert_eq!(r.attempts(), 2);
        assert_eq!(r.failed(), DialStep::GiveUp, "once, not a loop");
        assert_eq!(r.set_foreground(false), DialStep::Wait);
        assert_eq!(r.set_foreground(true), DialStep::Wait);
        assert_eq!(r.retry_due(), DialStep::Wait);
    }

    #[test]
    fn a_failure_in_the_foreground_retries_after_the_delay() {
        let mut r = PromptRetry::new(true);
        assert_eq!(r.start(), DialStep::Dial);
        assert_eq!(r.failed(), DialStep::RetryAfter(PROMPT_RETRY_DELAY));
        assert_eq!(r.set_foreground(true), DialStep::Wait, "no change");
        assert_eq!(r.retry_due(), DialStep::Dial);
        assert_eq!(r.succeeded(), DialStep::Done);
        assert_eq!(r.retry_due(), DialStep::Wait);
    }

    #[test]
    fn a_backgrounded_app_never_dials() {
        let mut r = PromptRetry::new(false);
        assert_eq!(r.start(), DialStep::Wait);
        assert_eq!(r.retry_due(), DialStep::Wait);
        assert_eq!(r.attempts(), 0);
        assert_eq!(r.set_foreground(true), DialStep::Dial, "owed on return");
        assert_eq!(r.failed(), DialStep::RetryAfter(PROMPT_RETRY_DELAY));
        assert_eq!(r.set_foreground(false), DialStep::Wait);
        assert_eq!(
            r.retry_due(),
            DialStep::Wait,
            "backgrounded: the timer is not a dial"
        );
        assert_eq!(r.set_foreground(true), DialStep::Dial);
        assert_eq!(r.attempts(), 2);
    }
}
