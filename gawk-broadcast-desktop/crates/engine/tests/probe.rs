//! The relay probe against a scripted `/echo` session (R62, docs/64 D1):
//! the median, the identity deadline and the one failed state, on a paused
//! tokio clock so every delay is exact.

use gawk_engine::probe::{PROBE_SAMPLES, ProbeResult, probe_session};
use gawk_engine::relay::{
    BoxFuture, KeyframeWriter, RelaySession, SendDatagramError, ServerStream, SessionClose,
};
use gawk_wire as wire;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::mpsc;

/// An `/echo` session: each datagram comes back after its sample's delay
/// (`None` loses it), and the identity stream arrives after its own delay.
struct EchoRelay {
    delays: Vec<Option<Duration>>,
    /// The server's uni streams, in arrival order, each after its delay
    /// from the previous accept.
    uni: Mutex<std::collections::VecDeque<(Duration, Vec<u8>)>>,
    back_tx: mpsc::UnboundedSender<Vec<u8>>,
    back_rx: tokio::sync::Mutex<mpsc::UnboundedReceiver<Vec<u8>>>,
    sends: AtomicUsize,
    closed: AtomicBool,
}

impl EchoRelay {
    fn new(delays: Vec<Option<Duration>>, identity: Option<(Duration, Vec<u8>)>) -> Arc<Self> {
        Self::with_streams(delays, identity.into_iter().collect())
    }

    fn with_streams(delays: Vec<Option<Duration>>, uni: Vec<(Duration, Vec<u8>)>) -> Arc<Self> {
        let (back_tx, back_rx) = mpsc::unbounded_channel();
        Arc::new(Self {
            delays,
            uni: Mutex::new(uni.into()),
            back_tx,
            back_rx: tokio::sync::Mutex::new(back_rx),
            sends: AtomicUsize::new(0),
            closed: AtomicBool::new(false),
        })
    }
}

struct Bytes(Vec<u8>);

impl ServerStream for Bytes {
    fn read_to_end(&mut self, _limit: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        let b = std::mem::take(&mut self.0);
        Box::pin(async move { Ok(b) })
    }
}

impl RelaySession for EchoRelay {
    fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError> {
        let n = self.sends.fetch_add(1, Ordering::SeqCst);
        if let Some(Some(delay)) = self.delays.get(n).copied() {
            let tx = self.back_tx.clone();
            let d = dgram.to_vec();
            tokio::spawn(async move {
                tokio::time::sleep(delay).await;
                let _ = tx.send(d);
            });
        }
        Ok(())
    }
    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async { Err("not a publish session".into()) })
    }
    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        let next = self.uni.lock().unwrap().pop_front();
        Box::pin(async move {
            match next {
                Some((delay, msg)) => {
                    tokio::time::sleep(delay).await;
                    Ok(Box::new(Bytes(msg)) as Box<dyn ServerStream>)
                }
                None => std::future::pending().await,
            }
        })
    }
    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(async {
            match self.back_rx.lock().await.recv().await {
                Some(d) => Ok(d),
                None => Err("closed".into()),
            }
        })
    }
    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        Box::pin(std::future::pending())
    }
    fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
    }
}

fn identity(name: &str) -> Vec<u8> {
    identity_with(name, 0)
}

fn identity_with(name: &str, notices: u8) -> Vec<u8> {
    let mut msg = Vec::new();
    wire::append_relay_identity(
        &mut msg,
        &wire::RelayIdentity {
            server_version: "1.42.0",
            name,
            notices,
        },
    )
    .unwrap();
    msg
}

fn ms(v: u64) -> Option<Duration> {
    Some(Duration::from_millis(v))
}

#[tokio::test(start_paused = true)]
async fn the_rtt_is_the_median_and_the_name_is_the_relays() {
    let relay = EchoRelay::new(
        vec![ms(10), ms(50), ms(20), ms(40), ms(30)],
        Some((Duration::from_millis(200), identity(" Homelab relay "))),
    );
    let got = probe_session(relay.clone()).await;
    assert_eq!(
        got,
        ProbeResult::Ok {
            rtt_ms: 30,
            name: Some("Homelab relay".into()),
            screens: false,
            local: None,
            offer: None,
        }
    );
    assert_eq!(relay.sends.load(Ordering::SeqCst), PROBE_SAMPLES);
    assert!(
        relay.closed.load(Ordering::SeqCst),
        "the probe closes its session"
    );
}

#[tokio::test(start_paused = true)]
async fn lost_echoes_are_left_out_of_the_median() {
    let relay = EchoRelay::new(vec![ms(80), None, ms(20), None, ms(40)], None);
    assert_eq!(
        probe_session(relay).await,
        ProbeResult::Ok {
            rtt_ms: 40,
            name: None,
            screens: false,
            local: None,
            offer: None,
        }
    );
}

#[tokio::test(start_paused = true)]
async fn an_identity_after_the_deadline_costs_only_the_name() {
    let relay = EchoRelay::new(
        vec![ms(5); PROBE_SAMPLES],
        Some((Duration::from_millis(1600), identity("late"))),
    );
    assert_eq!(
        probe_session(relay).await,
        ProbeResult::Ok {
            rtt_ms: 5,
            name: None,
            screens: false,
            local: None,
            offer: None,
        }
    );
}

#[tokio::test(start_paused = true)]
async fn an_empty_name_is_no_name() {
    let relay = EchoRelay::new(
        vec![ms(5); PROBE_SAMPLES],
        Some((Duration::from_millis(1), identity(""))),
    );
    assert_eq!(
        probe_session(relay).await,
        ProbeResult::Ok {
            rtt_ms: 5,
            name: None,
            screens: false,
            local: None,
            offer: None,
        }
    );
}

// R40 (docs/71 D13): the pre-live probe carries the relay's screening
// notice, so the window can say it before the broadcaster goes live — and a
// late identity costs the notice along with the name.
#[tokio::test(start_paused = true)]
async fn the_probe_carries_the_screening_notice() {
    let relay = EchoRelay::new(
        vec![ms(5); PROBE_SAMPLES],
        Some((
            Duration::from_millis(1),
            identity_with("Homelab", wire::RELAY_NOTICE_SCREENING),
        )),
    );
    assert_eq!(
        probe_session(relay).await,
        ProbeResult::Ok {
            rtt_ms: 5,
            name: Some("Homelab".into()),
            screens: true,
            local: None,
            offer: None,
        }
    );

    let late = EchoRelay::new(
        vec![ms(5); PROBE_SAMPLES],
        Some((
            Duration::from_millis(1600),
            identity_with("Homelab", wire::RELAY_NOTICE_SCREENING),
        )),
    );
    assert_eq!(
        probe_session(late).await,
        ProbeResult::Ok {
            rtt_ms: 5,
            name: None,
            screens: false,
            local: None,
            offer: None,
        }
    );
}

#[tokio::test(start_paused = true)]
async fn a_relay_that_never_echoes_fails() {
    let relay = EchoRelay::new(
        vec![None; PROBE_SAMPLES],
        Some((ms(1).unwrap(), identity("x"))),
    );
    assert_eq!(probe_session(relay.clone()).await, ProbeResult::Failed);
    assert!(relay.closed.load(Ordering::SeqCst));
}

#[tokio::test(start_paused = true)]
async fn an_echo_slower_than_the_wait_is_lost() {
    // Only the last sample echoes, and later than the wait after the last
    // send: nothing is measured.
    let relay = EchoRelay::new(vec![None, None, None, None, ms(1500)], None);
    assert_eq!(probe_session(relay).await, ProbeResult::Failed);
}

// --- R74 PP14 (docs/76 §6.12): the offer and the PoP's info ride the same
// /echo session as the identity, in no fixed order; each is read by its
// type byte, never by its position.

fn offer_msg() -> Vec<u8> {
    let h = [9u8; 32];
    let mut msg = Vec::new();
    wire::append_local_relay_offer(
        &mut msg,
        &wire::LocalRelayOffer {
            handle: "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab",
            name: "Assembly 2026",
            validity: 3600,
            candidates: vec![wire::LocalRelayCandidate {
                url: "https://192.168.1.20:4433",
                hashes: vec![&h],
            }],
        },
    )
    .unwrap();
    msg
}

fn info_msg() -> Vec<u8> {
    let mut msg = Vec::new();
    wire::append_local_relay_info(
        &mut msg,
        &wire::LocalRelayInfo {
            linked: true,
            on_wall: false,
            name: "Assembly 2026",
            home_origin: "https://gawk.ioio.fi",
            wall_code: "assembly-2026",
        },
    )
    .unwrap();
    msg
}

#[tokio::test(start_paused = true)]
async fn a_home_relays_offer_is_read_whatever_its_order() {
    for offer_first in [true, false] {
        let mut uni = vec![
            (Duration::from_millis(20), identity("gawk")),
            (Duration::from_millis(5), offer_msg()),
        ];
        if offer_first {
            uni.reverse();
        }
        let relay = EchoRelay::with_streams(vec![ms(5); PROBE_SAMPLES], uni);
        let ProbeResult::Ok {
            name, offer, local, ..
        } = probe_session(relay).await
        else {
            panic!("reachable");
        };
        assert_eq!(name.as_deref(), Some("gawk"), "offer first: {offer_first}");
        let offer = offer.expect("the offer");
        assert_eq!(offer.handle, "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab");
        assert_eq!(offer.name, "Assembly 2026");
        assert_eq!(offer.candidates[0].url, "https://192.168.1.20:4433");
        assert_eq!(offer.candidates[0].hashes, vec![[9u8; 32]]);
        assert_eq!(local, None);
    }
}

#[tokio::test(start_paused = true)]
async fn a_pops_info_is_read_beside_its_identity() {
    let relay = EchoRelay::with_streams(
        vec![ms(5); PROBE_SAMPLES],
        vec![
            (Duration::from_millis(1), info_msg()),
            (Duration::from_millis(1), identity("Assembly 2026")),
        ],
    );
    let ProbeResult::Ok { local, offer, .. } = probe_session(relay).await else {
        panic!("reachable");
    };
    let local = local.expect("the info");
    assert!(local.linked);
    assert_eq!(local.home_origin, "https://gawk.ioio.fi");
    assert_eq!(local.wall_code, "assembly-2026");
    assert_eq!(offer, None);
}

// A malformed offer and unknown types are skipped, not fatal, and do not
// cost the identity behind them.
#[tokio::test(start_paused = true)]
async fn a_bad_offer_or_an_unknown_stream_costs_nothing_else() {
    let mut bad = offer_msg();
    bad[2] = 0x01;
    let relay = EchoRelay::with_streams(
        vec![ms(5); PROBE_SAMPLES],
        vec![
            (Duration::from_millis(1), vec![0x01, 0x7f, 0x00]),
            (Duration::from_millis(1), bad),
            (Duration::from_millis(1), identity("gawk")),
        ],
    );
    assert_eq!(
        probe_session(relay).await,
        ProbeResult::Ok {
            rtt_ms: 5,
            name: Some("gawk".into()),
            screens: false,
            local: None,
            offer: None,
        }
    );
}

// Home sends the offer beside the identity, not after it: a stream landing
// just after the echoes finish is kept (the grace), and a relay that sends
// nothing more does not hold the probe open to the identity deadline.
#[tokio::test(start_paused = true)]
async fn an_offer_just_after_the_echoes_is_kept_and_none_costs_no_wait() {
    // The echoes finish at about 485 ms; the offer lands at about 601 ms.
    let relay = EchoRelay::with_streams(
        vec![ms(5); PROBE_SAMPLES],
        vec![
            (Duration::from_millis(1), identity("gawk")),
            (Duration::from_millis(600), offer_msg()),
        ],
    );
    let ProbeResult::Ok { offer, .. } = probe_session(relay).await else {
        panic!("reachable");
    };
    assert!(offer.is_some());

    let relay = EchoRelay::new(
        vec![ms(5); PROBE_SAMPLES],
        Some((Duration::from_millis(1), identity("gawk"))),
    );
    let started = tokio::time::Instant::now();
    assert!(matches!(probe_session(relay).await, ProbeResult::Ok { .. }));
    let took = started.elapsed();
    assert!(
        took < gawk_engine::probe::PROBE_IDENTITY_DEADLINE,
        "{took:?}: no wait for an offer that never comes"
    );
}
