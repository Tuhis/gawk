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
    identity: Option<(Duration, Vec<u8>)>,
    back_tx: mpsc::UnboundedSender<Vec<u8>>,
    back_rx: tokio::sync::Mutex<mpsc::UnboundedReceiver<Vec<u8>>>,
    sends: AtomicUsize,
    closed: AtomicBool,
    streams: Mutex<bool>,
}

impl EchoRelay {
    fn new(delays: Vec<Option<Duration>>, identity: Option<(Duration, Vec<u8>)>) -> Arc<Self> {
        let (back_tx, back_rx) = mpsc::unbounded_channel();
        Arc::new(Self {
            delays,
            identity,
            back_tx,
            back_rx: tokio::sync::Mutex::new(back_rx),
            sends: AtomicUsize::new(0),
            closed: AtomicBool::new(false),
            streams: Mutex::new(false),
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
        let first = !std::mem::replace(&mut *self.streams.lock().unwrap(), true);
        let identity = self.identity.clone();
        Box::pin(async move {
            match (first, identity) {
                (true, Some((delay, msg))) => {
                    tokio::time::sleep(delay).await;
                    Ok(Box::new(Bytes(msg)) as Box<dyn ServerStream>)
                }
                _ => std::future::pending().await,
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
    let mut msg = Vec::new();
    wire::append_relay_identity(
        &mut msg,
        &wire::RelayIdentity {
            server_version: "1.42.0",
            name,
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
            name: Some("Homelab relay".into())
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
            name: None
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
            name: None
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
            name: None
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
