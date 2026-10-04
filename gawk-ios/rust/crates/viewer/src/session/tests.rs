//! The session's policy against a scripted fake transport, on tokio's
//! paused clock: what reconnects, how fast, and what ends a viewer.

use super::*;
use gawk_engine::relay::{KeyframeWriter, SendDatagramError, ServerStream, StartPhase};
use std::sync::Mutex;

/// A session that closes with `close` once `live_for` has passed, and
/// optionally hands the viewer one server stream first.
struct FakeSession {
    close: SessionClose,
    live_for: Duration,
    stream: Mutex<Option<Vec<u8>>>,
}

struct FakeStream(Option<Vec<u8>>);

impl ServerStream for FakeStream {
    fn read_to_end(&mut self, _limit: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        let msg = self.0.take();
        Box::pin(async move { msg.ok_or_else(|| "read twice".to_string()) })
    }
}

impl RelaySession for FakeSession {
    fn send_datagram(&self, _dgram: &[u8]) -> Result<(), SendDatagramError> {
        Ok(())
    }
    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async { Err("a viewer never opens streams".into()) })
    }
    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        let msg = self.stream.lock().unwrap().take();
        Box::pin(async move {
            match msg {
                Some(m) => Ok(Box::new(FakeStream(Some(m))) as Box<dyn ServerStream>),
                None => std::future::pending().await,
            }
        })
    }
    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(std::future::pending())
    }
    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        let (close, after) = (self.close.clone(), self.live_for);
        Box::pin(async move {
            tokio::time::sleep(after).await;
            close
        })
    }
}

/// Hands out one scripted outcome per dial; records every URL and when.
struct FakeDialer {
    script: Mutex<Vec<Result<FakeSession, u16>>>,
    dials: Mutex<Vec<(String, tokio::time::Instant)>>,
}

impl FakeDialer {
    fn new(script: Vec<Result<FakeSession, u16>>) -> Arc<Self> {
        Arc::new(Self {
            script: Mutex::new(script.into_iter().rev().collect()),
            dials: Mutex::new(Vec::new()),
        })
    }

    fn gaps_ms(&self) -> Vec<u64> {
        let d = self.dials.lock().unwrap();
        d.windows(2)
            .map(|w| (w[1].1 - w[0].1).as_millis() as u64)
            .collect()
    }
}

impl SubscribeDialer for FakeDialer {
    fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RelaySession>, StartError>> {
        self.dials
            .lock()
            .unwrap()
            .push((url.to_owned(), tokio::time::Instant::now()));
        let next = self.script.lock().unwrap().pop().unwrap_or(Err(0));
        Box::pin(async move {
            match next {
                Ok(s) => Ok(Arc::new(s) as Arc<dyn RelaySession>),
                Err(status) => Err(StartError {
                    phase: StartPhase::Connect,
                    status,
                    message: format!("refused {status}"),
                }),
            }
        })
    }
}

#[derive(Default)]
struct Sink {
    states: Mutex<Vec<ViewerState>>,
    events: Mutex<Vec<ViewerEvent>>,
}

impl ViewerSink for Sink {
    fn state(&self, s: ViewerState) {
        self.states.lock().unwrap().push(s);
    }
    fn event(&self, e: ViewerEvent) {
        self.events.lock().unwrap().push(e);
    }
}

fn closes_with(code: Option<u32>, after_ms: u64) -> Result<FakeSession, u16> {
    Ok(FakeSession {
        close: code.map_or(SessionClose::Abrupt("reset".into()), SessionClose::Code),
        live_for: Duration::from_millis(after_ms),
        stream: Mutex::new(None),
    })
}

fn cfg() -> ViewerConfig {
    ViewerConfig {
        relay_url: "https://relay.example:4433".into(),
        broadcast_id: "K7XQ2M".into(),
        preset: PlayoutPreset::Balanced,
    }
}

async fn run_with(
    dialer: Arc<FakeDialer>,
) -> (EndReason, Arc<Sink>, mpsc::UnboundedSender<Command>) {
    let sink = Arc::new(Sink::default());
    let (tx, rx) = mpsc::unbounded_channel();
    let reason = run(cfg(), dialer, sink.clone(), ViewerClock::new(), rx).await;
    (reason, sink, tx)
}

#[test]
fn the_url_carries_owner_labels_and_rejoin() {
    let url = subscribe_url(
        "https://relay.example:4433",
        "K7XQ2M",
        "00ff00ff00ff00ff",
        false,
    )
    .unwrap();
    let os = gawk_engine::relay::CLIENT_OS;
    let app = gawk_engine::defaults::this().app;
    assert_eq!(
        url,
        format!(
            "https://relay.example:4433/subscribe/K7XQ2M?owner=00ff00ff00ff00ff&app={app}&os={os}"
        )
    );
    let again = subscribe_url("https://relay.example:4433", "K7XQ2M", "aa", true).unwrap();
    assert!(again.ends_with("&rejoin=1"));
    assert!(subscribe_url("http://relay.example", "K7XQ2M", "aa", false).is_err());
    assert_eq!(mint_owner().len(), 16);
}

#[tokio::test(start_paused = true)]
async fn a_terminal_close_ends_the_viewer() {
    for code in [
        gawk_wire::CLOSE_CODE_BROADCAST_ENDED,
        gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR,
    ] {
        let dialer = FakeDialer::new(vec![closes_with(Some(code), 100)]);
        let (reason, sink, _tx) = run_with(dialer.clone()).await;
        assert_eq!(reason, EndReason::Closed(code));
        assert_eq!(dialer.dials.lock().unwrap().len(), 1, "never redialled");
        assert_eq!(
            sink.states.lock().unwrap().last(),
            Some(&ViewerState::Ended(EndReason::Closed(code)))
        );
    }
}

#[tokio::test(start_paused = true)]
async fn a_drain_reconnects_at_once_with_rejoin_and_an_abrupt_drop_in_250ms() {
    let dialer = FakeDialer::new(vec![
        closes_with(Some(gawk_wire::CLOSE_CODE_SERVER_DRAINING), 100),
        closes_with(None, 100),
        closes_with(Some(gawk_wire::CLOSE_CODE_BROADCAST_ENDED), 100),
    ]);
    let (reason, _sink, _tx) = run_with(dialer.clone()).await;
    assert_eq!(
        reason,
        EndReason::Closed(gawk_wire::CLOSE_CODE_BROADCAST_ENDED)
    );
    // Each session lived 100 ms; then 0 ms (drain), then 250 ms (abrupt).
    assert_eq!(dialer.gaps_ms(), [100, 350]);
    let dials = dialer.dials.lock().unwrap();
    assert!(!dials[0].0.contains("rejoin"));
    assert!(dials[1].0.contains("rejoin=1"));
    // A fresh owner token per attempt.
    assert_ne!(dials[0].0, dials[1].0);
}

#[tokio::test(start_paused = true)]
async fn failed_dials_climb_the_ladder_and_give_up_after_ten() {
    // One session that drops, then nothing but refusals (429, relay full).
    let mut script = vec![closes_with(None, 0)];
    script.extend((0..20).map(|_| Err(429)));
    let dialer = FakeDialer::new(script);
    let (reason, _sink, _tx) = run_with(dialer.clone()).await;
    assert_eq!(reason, EndReason::GaveUp);
    assert_eq!(dialer.dials.lock().unwrap().len(), 1 + 10);
    assert_eq!(
        dialer.gaps_ms(),
        [
            250, 2000, 4000, 8000, 15_000, 15_000, 15_000, 15_000, 15_000, 15_000
        ]
    );
}

#[tokio::test(start_paused = true)]
async fn an_unknown_broadcast_ends_at_once() {
    let dialer = FakeDialer::new(vec![Err(404)]);
    let (reason, _sink, _tx) = run_with(dialer.clone()).await;
    assert_eq!(reason, EndReason::NotFound);
}

#[tokio::test(start_paused = true)]
async fn a_session_closing_announces_the_code_a_bare_close_lacks() {
    // R57: Chrome-shaped closes carry no code; the in-band 0x17 names it.
    let mut closing = Vec::new();
    gawk_wire::append_session_closing(&mut closing, gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR)
        .unwrap();
    let dialer = FakeDialer::new(vec![Ok(FakeSession {
        close: SessionClose::Abrupt("no code".into()),
        live_for: Duration::from_millis(500),
        stream: Mutex::new(Some(closing)),
    })]);
    let (reason, _sink, _tx) = run_with(dialer).await;
    assert_eq!(
        reason,
        EndReason::Closed(gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR)
    );
}

#[tokio::test(start_paused = true)]
async fn stop_ends_a_live_viewer() {
    let dialer = FakeDialer::new(vec![closes_with(None, 3_600_000)]);
    let sink = Arc::new(Sink::default());
    let (tx, rx) = mpsc::unbounded_channel();
    let task = tokio::spawn(run(cfg(), dialer, sink.clone(), ViewerClock::new(), rx));
    tokio::time::sleep(Duration::from_millis(50)).await;
    tx.send(Command::Stop).unwrap();
    assert_eq!(task.await.unwrap(), EndReason::Stopped);
    let states = sink.states.lock().unwrap();
    assert_eq!(states[..2], [ViewerState::Connecting, ViewerState::Live]);
}

#[tokio::test(start_paused = true)]
async fn a_404_while_reconnecting_is_a_restarted_relay_not_an_unknown_broadcast() {
    // After a relay restart the broadcast is unknown until its publisher
    // reclaims it: the viewer must keep climbing the ladder, not give up.
    let dialer = FakeDialer::new(vec![
        closes_with(Some(gawk_wire::CLOSE_CODE_SERVER_DRAINING), 100),
        Err(404),
        Err(404),
        closes_with(Some(gawk_wire::CLOSE_CODE_BROADCAST_ENDED), 100),
    ]);
    let (reason, _sink, _tx) = run_with(dialer.clone()).await;
    assert_eq!(
        reason,
        EndReason::Closed(gawk_wire::CLOSE_CODE_BROADCAST_ENDED)
    );
    assert_eq!(dialer.dials.lock().unwrap().len(), 4);
}
