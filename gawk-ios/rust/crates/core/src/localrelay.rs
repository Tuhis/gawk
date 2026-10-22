//! A party's local relay, as Swift sees it (R74 PP14, docs/76 §6.12): home's
//! offer (`0x19`), a PoP's info (`0x18`), the parallel pinned probe of an
//! offer's candidates, the party wall's Hide and Show, and the one retry
//! iOS owes its local-network prompt (TN3179, item 44). The decisions are
//! the shared engine's ([`gawk_engine::localrelay`]); nothing here
//! re-implements them.

use gawk_engine::localrelay as lr;
use gawk_engine::probe::{self, ProbeResult};
use gawk_engine::transport::{CertHash, cert_hashes_from_hex};
use std::sync::Mutex;

use crate::servers::ServerProbe;

/// One way to reach the PoP an offer names: its relay origin and the
/// certificate hashes to pin (lowercase hex; empty: the system store, the
/// named tier's public certificate).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct LocalRelayCandidate {
    pub url: String,
    pub cert_hashes: Vec<String>,
}

/// A home relay's suggestion to use a party's local relay (`0x19`). Act on
/// it only while fresh (item 43.1): never store it to switch later.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct LocalRelayOffer {
    /// The opaque PoP handle the "Use … instead" opt-out is remembered
    /// against (item 43.6).
    pub handle: String,
    /// The PoP's display name, sanitized ("A local relay" when it has none).
    pub name: String,
    pub validity_secs: u64,
    pub candidates: Vec<LocalRelayCandidate>,
}

/// What a PoP says about itself on a session (`0x18`).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct LocalRelayInfo {
    /// Linked to its home fleet: share links are home links.
    pub linked: bool,
    /// On a publish session: this broadcast is on the party wall.
    pub on_wall: bool,
    pub name: Option<String>,
    pub home_origin: String,
    /// The party wall's room code; empty without one.
    pub wall_code: String,
}

/// The candidate that answered: where to switch.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct LocalRelaySwitch {
    pub handle: String,
    pub name: String,
    /// The relay origin to use in place of home, with its pins.
    pub url: String,
    pub cert_hashes: Vec<String>,
    pub rtt_ms: u32,
}

/// A probe of home: what [`crate::servers::probe_server`] finds, and home's
/// offer when it has one for this device.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct HomeProbe {
    pub probe: ServerProbe,
    pub offer: Option<LocalRelayOffer>,
}

fn hex(h: &CertHash) -> String {
    h.iter().map(|b| format!("{b:02x}")).collect()
}

impl From<&lr::Offer> for LocalRelayOffer {
    fn from(o: &lr::Offer) -> Self {
        Self {
            handle: o.handle.clone(),
            name: o.name.clone(),
            validity_secs: o.validity.as_secs(),
            candidates: o
                .candidates
                .iter()
                .map(|c| LocalRelayCandidate {
                    url: c.url.clone(),
                    cert_hashes: c.hashes.iter().map(hex).collect(),
                })
                .collect(),
        }
    }
}

impl From<&LocalRelayOffer> for lr::Offer {
    fn from(o: &LocalRelayOffer) -> Self {
        Self {
            handle: o.handle.clone(),
            name: o.name.clone(),
            validity: std::time::Duration::from_secs(o.validity_secs),
            candidates: o
                .candidates
                .iter()
                .map(|c| lr::Candidate {
                    url: c.url.clone(),
                    hashes: cert_hashes_from_hex(&c.cert_hashes),
                })
                .collect(),
        }
    }
}

impl From<&lr::LocalRelay> for LocalRelayInfo {
    fn from(l: &lr::LocalRelay) -> Self {
        Self {
            linked: l.linked,
            on_wall: l.on_wall,
            name: l.name.clone(),
            home_origin: l.home_origin.clone(),
            wall_code: l.wall_code.clone(),
        }
    }
}

fn runtime() -> Option<tokio::runtime::Runtime> {
    tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .ok()
}

/// Probes home's `/echo` the way a client that follows offers does: with
/// `lr=1` (item 43.7). Blocking, a few seconds at most: call it off the
/// main thread. The broadcaster's start screen and the player call it on
/// open, so a fresh offer arrives before going live (item 43.3).
#[uniffi::export(default(cert_hashes = []))]
pub fn probe_home(relay_url: String, insecure: bool, cert_hashes: Vec<String>) -> HomeProbe {
    let pins = cert_hashes_from_hex(&cert_hashes);
    let Some(rt) = runtime() else {
        return HomeProbe {
            probe: ServerProbe::Unreachable,
            offer: None,
        };
    };
    let origin = gawk_engine::defaults::origin().to_owned();
    match rt.block_on(probe::probe_home(
        &relay_url, &origin, insecure, &pins, true,
    )) {
        ProbeResult::Ok {
            rtt_ms,
            name,
            screens,
            offer,
            ..
        } => HomeProbe {
            probe: ServerProbe::Reachable {
                rtt_ms,
                name,
                screens,
            },
            offer: offer.as_ref().map(Into::into),
        },
        ProbeResult::Failed => HomeProbe {
            probe: ServerProbe::Unreachable,
            offer: None,
        },
    }
}

/// Probes every candidate of `offer` at once over `/echo`, each pinned to
/// its hashes, and returns the first that answers (item 43.2); `None` when
/// none does. Blocking: call it off the main thread, and only while the app
/// is in the foreground ([`LocalRelayPrompt`]).
#[uniffi::export]
pub fn probe_local_relay(offer: LocalRelayOffer) -> Option<LocalRelaySwitch> {
    let offer: lr::Offer = (&offer).into();
    let rt = runtime()?;
    let origin = gawk_engine::defaults::origin().to_owned();
    let found = rt.block_on(lr::probe_candidates(&offer, &origin))?;
    Some(LocalRelaySwitch {
        handle: found.handle,
        name: found.name,
        url: found.url,
        cert_hashes: found.hashes.iter().map(hex).collect(),
        rtt_ms: found.rtt_ms,
    })
}

/// Hides this broadcast from the PoP's party wall (`show == false`) or
/// shows it again (docs/76 item 34): a Detach or Attach of its own
/// broadcast, proven by its resume token. Blocking: off the main thread.
/// `None` when it went out, else why not.
#[uniffi::export(default(cert_hashes = []))]
pub fn local_relay_wall(
    pop_url: String,
    wall_code: String,
    broadcast_id: String,
    resume_token_hex: String,
    label: String,
    show: bool,
    cert_hashes: Vec<String>,
) -> Option<String> {
    let target = lr::WallTarget {
        pop_url,
        origin: gawk_engine::defaults::origin().to_owned(),
        pins: cert_hashes_from_hex(&cert_hashes),
        wall_code,
        broadcast_id,
        resume_token_hex,
        label,
    };
    let Some(rt) = runtime() else {
        return Some("no runtime".into());
    };
    rt.block_on(lr::wall_command(&target, show)).err()
}

/// The viewer copy for 4012 (docs/76 item 28), the shared engine's: "This
/// stream is at a LAN party with limited internet. …"
#[uniffi::export]
pub fn pop_uplink_full_message() -> String {
    gawk_engine::resume::POP_UPLINK_FULL_MESSAGE.to_owned()
}

/// A URL with `wall=hidden` added: the next publish of a broadcaster who
/// hid from this PoP's wall (item 34), so it never flashes onto it.
#[uniffi::export]
pub fn with_wall_hidden(url: String) -> String {
    lr::with_wall_hidden(&url)
}

/// The next step of a switch dial under Apple's local-network privacy.
#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum LocalRelayDialStep {
    /// Probe the candidates now ([`probe_local_relay`]).
    Dial,
    /// Do nothing until the scene phase changes or the retry is due.
    Wait,
    /// Call [`LocalRelayPrompt::retry_due`] after this many milliseconds.
    RetryAfter { ms: u64 },
    /// Stay on home for this app session.
    GiveUp,
    /// The switch answered.
    Done,
}

impl From<lr::DialStep> for LocalRelayDialStep {
    fn from(s: lr::DialStep) -> Self {
        match s {
            lr::DialStep::Dial => Self::Dial,
            lr::DialStep::Wait => Self::Wait,
            lr::DialStep::RetryAfter(d) => Self::RetryAfter {
                ms: u64::try_from(d.as_millis()).unwrap_or(u64::MAX),
            },
            lr::DialStep::GiveUp => Self::GiveUp,
            lr::DialStep::Done => Self::Done,
        }
    }
}

/// TN3179 (docs/76 item 44): the first dial to a LAN address may be refused
/// before the user answers the local-network prompt, so one offer's switch
/// is retried **once**, after the prompt; and while the app is not active it
/// never dials. Swift feeds it the scene phase and the probe's outcomes,
/// and dials only on [`LocalRelayDialStep::Dial`].
#[derive(uniffi::Object)]
pub struct LocalRelayPrompt {
    inner: Mutex<lr::PromptRetry>,
}

#[uniffi::export]
impl LocalRelayPrompt {
    /// `active`: the scene phase is `.active` now.
    #[uniffi::constructor]
    pub fn new(active: bool) -> std::sync::Arc<Self> {
        std::sync::Arc::new(Self {
            inner: Mutex::new(lr::PromptRetry::new(active)),
        })
    }

    /// A fresh offer to act on.
    pub fn start(&self) -> LocalRelayDialStep {
        self.inner.lock().unwrap().start().into()
    }

    /// [`probe_local_relay`] found nothing.
    pub fn failed(&self) -> LocalRelayDialStep {
        self.inner.lock().unwrap().failed().into()
    }

    /// [`probe_local_relay`] found a candidate.
    pub fn succeeded(&self) -> LocalRelayDialStep {
        self.inner.lock().unwrap().succeeded().into()
    }

    /// The scene phase became `.active` (`true`) or not (`false`).
    pub fn set_active(&self, active: bool) -> LocalRelayDialStep {
        self.inner.lock().unwrap().set_foreground(active).into()
    }

    /// The [`LocalRelayDialStep::RetryAfter`] delay passed.
    pub fn retry_due(&self) -> LocalRelayDialStep {
        self.inner.lock().unwrap().retry_due().into()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn offer() -> LocalRelayOffer {
        LocalRelayOffer {
            handle: "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab".into(),
            name: "Assembly 2026".into(),
            validity_secs: 3600,
            candidates: vec![LocalRelayCandidate {
                url: "https://192.168.1.20:4433".into(),
                cert_hashes: vec!["07".repeat(32)],
            }],
        }
    }

    // The offer crosses to Swift and back with its pins intact.
    #[test]
    fn an_offer_round_trips_through_the_swift_shape() {
        let engine: lr::Offer = (&offer()).into();
        assert_eq!(engine.candidates[0].hashes, vec![[7u8; 32]]);
        assert_eq!(LocalRelayOffer::from(&engine), offer());
    }

    #[test]
    fn a_malformed_home_is_unreachable_with_no_offer() {
        assert_eq!(
            probe_home("not a url".into(), false, vec![]),
            HomeProbe {
                probe: ServerProbe::Unreachable,
                offer: None,
            }
        );
    }

    #[test]
    fn a_wall_command_without_an_identity_says_why() {
        let why = local_relay_wall(
            "https://192.168.1.20:4433".into(),
            "assembly-2026".into(),
            String::new(),
            String::new(),
            String::new(),
            false,
            vec![],
        );
        assert!(why.is_some_and(|w| w.contains("identity")));
    }

    // TN3179 through the Swift surface: one retry after the prompt, none
    // while not active.
    #[test]
    fn the_prompt_retry_dials_once_more_after_the_prompt() {
        let p = LocalRelayPrompt::new(true);
        assert_eq!(p.start(), LocalRelayDialStep::Dial);
        assert_eq!(p.set_active(false), LocalRelayDialStep::Wait);
        assert_eq!(p.failed(), LocalRelayDialStep::Wait);
        assert_eq!(p.set_active(true), LocalRelayDialStep::Dial);
        assert_eq!(p.failed(), LocalRelayDialStep::GiveUp);

        let p = LocalRelayPrompt::new(false);
        assert_eq!(
            p.start(),
            LocalRelayDialStep::Wait,
            "never while backgrounded"
        );
        assert_eq!(p.set_active(true), LocalRelayDialStep::Dial);
        assert!(matches!(
            p.failed(),
            LocalRelayDialStep::RetryAfter { ms: 3000 }
        ));
        assert_eq!(p.retry_due(), LocalRelayDialStep::Dial);
        assert_eq!(p.succeeded(), LocalRelayDialStep::Done);
    }
}
