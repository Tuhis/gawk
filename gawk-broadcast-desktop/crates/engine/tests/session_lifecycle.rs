//! Session lifecycle against a scripted relay, through the
//! `start_with_session` seam — the failure orderings a real relay will not
//! produce on demand.

use gawk_engine::clock::MonotonicClock;
use gawk_engine::relay::{
    BoxFuture, CancelSignal, KeyframeOutcome, KeyframeWriter, PublishDialer, RelaySession,
    SendDatagramError, ServerStream, SessionClose, StartError, StartPhase,
};
use gawk_engine::resume;
use gawk_engine::room::{RoomConn, RoomDialer};
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

/// A relay where nothing ever happens: the session just serves.
struct IdleRelay;

struct InstantWriter;

impl KeyframeWriter for InstantWriter {
    fn write(
        self: Box<Self>,
        _msg: Vec<u8>,
        _cancel: CancelSignal,
    ) -> BoxFuture<'static, KeyframeOutcome> {
        Box::pin(async { KeyframeOutcome::Sent })
    }
    fn abort(self: Box<Self>, _code: u32) {}
}

impl RelaySession for IdleRelay {
    fn send_datagram(&self, _dgram: &[u8]) -> Result<(), SendDatagramError> {
        Ok(())
    }
    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async { Ok(Box::new(InstantWriter) as Box<dyn KeyframeWriter>) })
    }
    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        Box::pin(std::future::pending())
    }
    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(std::future::pending())
    }
    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        Box::pin(std::future::pending())
    }
}

fn config() -> SessionConfig {
    SessionConfig {
        relay_url: "https://localhost:4433".into(),
        broadcast_id: String::new(),
        resume_token_hex: String::new(),
        publish_secret: String::new(),
        origin: "https://localhost".into(),
        insecure: true,
        ..SessionConfig::default()
    }
}

// R73 (docs/75 D4, D5, D8): the session reports the Delivery it was started
// with and the controller its connection runs; a live Delivery change is a
// call, not a restart. The engine's defaults — datagrams on BBR — are what
// the iOS app, which has no setting (OD3), runs.
#[tokio::test]
async fn the_session_reports_its_delivery_and_controller_and_takes_a_live_change() {
    use gawk_engine::carrier::Delivery;
    use gawk_engine::transport::Congestion;

    let (session, _rx) = Session::start_with_session(
        config(),
        Arc::new(IdleRelay),
        Arc::new(MonotonicClock::new()),
    );
    let st = session.stats();
    assert_eq!(st.uplink_mode_requested, "lowest-latency");
    assert_eq!(st.uplink_congestion, "bbr");
    assert_eq!(st.uplink_mode, "datagram");
    session.stop().await;

    let cfg = SessionConfig {
        uplink_delivery: Delivery::Reliable,
        congestion: Congestion::Cubic,
        ..config()
    };
    let (session, _rx) =
        Session::start_with_session(cfg, Arc::new(IdleRelay), Arc::new(MonotonicClock::new()));
    let st = session.stats();
    assert_eq!(st.uplink_mode_requested, "reliable");
    assert_eq!(st.uplink_congestion, "cubic");
    session.set_uplink_delivery(Delivery::LowestLatency);
    assert_eq!(session.stats().uplink_mode_requested, "lowest-latency");
    session.stop().await;
}

// The shell can drop its `Arc<Session>` without a completed `stop()` (the
// quit path's bounded stop timeout does exactly that). The dropped stop
// watch must END the run loop — reading it as "no stop yet" busy-loops
// `changed()` at 100% of a core forever.
#[tokio::test]
async fn dropping_the_session_ends_the_run_loop_instead_of_spinning() {
    let (session, mut rx) = Session::start_with_session(
        config(),
        Arc::new(IdleRelay),
        Arc::new(MonotonicClock::new()),
    );

    drop(session);

    let ev = tokio::time::timeout(Duration::from_secs(5), rx.recv())
        .await
        .expect("the run loop must notice its shell is gone")
        .expect("the run loop owns the channel until it emits Ended");
    assert_eq!(ev, EngineEvent::Ended { error: None });
}

// --- R62 (docs/64 D8–D9): pause, and a republish on the same code ---------------

/// A publish leg: serves until the session closes it, and records that and
/// whatever it was sent.
#[derive(Default)]
struct Leg {
    closed: AtomicBool,
    datagrams: Mutex<Vec<Vec<u8>>>,
}

impl RelaySession for Leg {
    fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError> {
        self.datagrams.lock().unwrap().push(dgram.to_vec());
        Ok(())
    }
    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async { Ok(Box::new(InstantWriter) as Box<dyn KeyframeWriter>) })
    }
    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        Box::pin(std::future::pending())
    }
    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(std::future::pending())
    }
    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        Box::pin(std::future::pending())
    }
    fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
    }
}

/// The reclaim dialer: records every URL and hands out the scripted legs.
#[derive(Default)]
struct Reclaims {
    urls: Mutex<Vec<String>>,
    legs: Mutex<VecDeque<Arc<Leg>>>,
}

impl PublishDialer for Reclaims {
    fn dial(&self, url: &str) -> BoxFuture<'_, Result<Arc<dyn RelaySession>, StartError>> {
        self.urls.lock().unwrap().push(url.to_owned());
        let leg = self.legs.lock().unwrap().pop_front();
        Box::pin(async move {
            leg.map(|l| l as Arc<dyn RelaySession>).ok_or(StartError {
                phase: StartPhase::Connect,
                status: 0,
                message: "no scripted leg".into(),
            })
        })
    }
}

struct NoRooms;

impl RoomDialer for NoRooms {
    fn dial(&self, _url: &str) -> BoxFuture<'_, Result<Arc<dyn RoomConn>, StartError>> {
        Box::pin(async {
            Err(StartError {
                phase: StartPhase::Connect,
                status: 0,
                message: "no rooms here".into(),
            })
        })
    }
}

const TOKEN: &str = "00112233445566778899aabbccddeeff";

/// Whether `leg` was sent exactly this datagram (time-sync pings ride the
/// leg too, so a count would test the ping cadence).
fn sent(leg: &Leg, dgram: &[u8]) -> bool {
    leg.datagrams.lock().unwrap().iter().any(|d| d == dgram)
}

/// A session on a known identity (a resumed start), its first leg, and the
/// scripted reclaims.
fn publishing(
    next: Vec<Arc<Leg>>,
) -> (
    Arc<Session>,
    tokio::sync::mpsc::UnboundedReceiver<EngineEvent>,
    Arc<Leg>,
    Arc<Reclaims>,
) {
    let first = Arc::new(Leg::default());
    let reclaims = Arc::new(Reclaims::default());
    *reclaims.legs.lock().unwrap() = next.into();
    let cfg = SessionConfig {
        broadcast_id: "ABC234".into(),
        resume_token_hex: TOKEN.into(),
        ..config()
    };
    let (session, rx) = Session::start_with_seams(
        cfg,
        first.clone(),
        Arc::new(MonotonicClock::new()),
        Arc::new(NoRooms),
        reclaims.clone(),
    );
    (session, rx, first, reclaims)
}

async fn expect(rx: &mut tokio::sync::mpsc::UnboundedReceiver<EngineEvent>, want: EngineEvent) {
    loop {
        let ev = tokio::time::timeout(Duration::from_secs(10), rx.recv())
            .await
            .unwrap_or_else(|_| panic!("timed out waiting for {want:?}"))
            .expect("the event channel stays open until Ended");
        if ev == want {
            return;
        }
        assert!(
            !matches!(ev, EngineEvent::Ended { .. }),
            "ended while waiting for {want:?}: {ev:?}"
        );
    }
}

#[tokio::test(start_paused = true)]
async fn a_pause_closes_the_leg_and_a_republish_reclaims_the_code_at_once() {
    let second = Arc::new(Leg::default());
    let (session, mut rx, first, reclaims) = publishing(vec![second.clone()]);

    session.pause();
    expect(&mut rx, EngineEvent::Paused).await;
    assert!(
        first.closed.load(Ordering::SeqCst),
        "the leg closes cleanly"
    );
    // Paused means nothing is dialed, however long it lasts.
    tokio::time::sleep(Duration::from_secs(60)).await;
    assert!(reclaims.urls.lock().unwrap().is_empty());

    let asked = tokio::time::Instant::now();
    session.republish();
    expect(&mut rx, EngineEvent::Resuming { attempt: 1 }).await;
    expect(&mut rx, EngineEvent::Resumed).await;
    assert!(
        asked.elapsed() < resume::RESUME_INITIAL_DELAY,
        "a deliberate reclaim does not wait out the loss delay"
    );
    let urls = reclaims.urls.lock().unwrap().clone();
    assert_eq!(urls.len(), 1);
    assert!(
        urls[0].contains("/publish/ABC234?") && urls[0].contains(&format!("resume={TOKEN}")),
        "the same code, reclaimed with its token: {}",
        urls[0]
    );

    // The sender publishes on the new leg.
    session.sender().send_datagram_best_effort(b"after");
    assert!(sent(&second, b"after"));
    assert!(!sent(&first, b"after"));

    session.stop().await;
    expect(&mut rx, EngineEvent::Ended { error: None }).await;
}

#[tokio::test(start_paused = true)]
async fn a_republish_while_live_swaps_the_leg_without_a_pause() {
    let second = Arc::new(Leg::default());
    let (session, mut rx, first, reclaims) = publishing(vec![second.clone()]);

    session.republish();
    loop {
        match tokio::time::timeout(Duration::from_secs(10), rx.recv())
            .await
            .unwrap()
            .unwrap()
        {
            EngineEvent::Resumed => break,
            EngineEvent::Paused => panic!("a restart is not a pause"),
            EngineEvent::Ended { error } => panic!("ended: {error:?}"),
            _ => {}
        }
    }
    assert!(first.closed.load(Ordering::SeqCst));
    assert_eq!(reclaims.urls.lock().unwrap().len(), 1);
    session.sender().send_datagram_best_effort(b"after");
    assert!(sent(&second, b"after"));
    assert!(!sent(&first, b"after"));

    session.stop().await;
    expect(&mut rx, EngineEvent::Ended { error: None }).await;
}

#[tokio::test(start_paused = true)]
async fn stopping_while_paused_ends_cleanly_and_dials_nothing() {
    let (session, mut rx, _first, reclaims) = publishing(vec![]);
    session.pause();
    expect(&mut rx, EngineEvent::Paused).await;
    session.stop().await;
    expect(&mut rx, EngineEvent::Ended { error: None }).await;
    assert!(reclaims.urls.lock().unwrap().is_empty());
}

/// A dialer whose every reclaim is refused with `status`.
struct Refusing(u16);

impl PublishDialer for Refusing {
    fn dial(&self, _url: &str) -> BoxFuture<'_, Result<Arc<dyn RelaySession>, StartError>> {
        let status = self.0;
        Box::pin(async move {
            Err(StartError {
                phase: StartPhase::Connect,
                status,
                message: format!("the relay refused the publish request (status {status})"),
            })
        })
    }
}

// Review of #423: a refused reclaim says which refusal it was, before the
// Ended it causes — only a 404 means the relay let the code go.
#[tokio::test(start_paused = true)]
async fn a_refused_reclaim_says_its_status_before_the_end() {
    for status in [404u16, 401] {
        let cfg = SessionConfig {
            broadcast_id: "ABC234".into(),
            resume_token_hex: TOKEN.into(),
            ..config()
        };
        let (session, mut rx) = Session::start_with_seams(
            cfg,
            Arc::new(Leg::default()),
            Arc::new(MonotonicClock::new()),
            Arc::new(NoRooms),
            Arc::new(Refusing(status)),
        );
        session.pause();
        expect(&mut rx, EngineEvent::Paused).await;
        session.republish();
        let mut refused = None;
        loop {
            let ev = tokio::time::timeout(Duration::from_secs(30), rx.recv())
                .await
                .unwrap()
                .unwrap();
            match ev {
                EngineEvent::ReclaimRefused { status } => refused = Some(status),
                EngineEvent::Ended { error } => {
                    assert!(error.is_some(), "a refused reclaim is an error ending");
                    break;
                }
                _ => {}
            }
        }
        assert_eq!(refused, Some(status), "said before the end");
    }
}

#[tokio::test(start_paused = true)]
async fn a_second_pause_is_a_no_op() {
    let (session, mut rx, _first, _reclaims) = publishing(vec![]);
    session.pause();
    expect(&mut rx, EngineEvent::Paused).await;
    session.pause();
    tokio::time::sleep(Duration::from_secs(5)).await;
    assert!(
        rx.try_recv().is_err(),
        "no second Paused, nothing else either"
    );
    session.stop().await;
}

// --- R40 (docs/71 D13, CF1): a screening relay's RelayIdentity --------------

/// One server uni stream, read to its end.
struct OneStream(Option<Vec<u8>>);

impl ServerStream for OneStream {
    fn read_to_end(&mut self, _limit: usize) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        let msg = self.0.take().unwrap_or_default();
        Box::pin(async move { Ok(msg) })
    }
}

/// A publish leg whose relay opens one server stream carrying `msg` at
/// session start — what a relay with -screening-notice does — and then just
/// serves.
#[derive(Default)]
struct NoticeLeg {
    stream: Mutex<Option<Vec<u8>>>,
    datagrams: Mutex<Vec<Vec<u8>>>,
}

impl RelaySession for NoticeLeg {
    fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError> {
        self.datagrams.lock().unwrap().push(dgram.to_vec());
        Ok(())
    }
    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async { Ok(Box::new(InstantWriter) as Box<dyn KeyframeWriter>) })
    }
    // Taken when the future is POLLED, not when it is built: the session's
    // select! loop builds a fresh accept_uni() future every turn and drops it
    // whenever another branch wins (the first ping tick does, at once), so an
    // eager take loses the stream on whichever turn select! happens not to
    // poll it — a real QUIC accept is cancel-safe, and the fake must be too.
    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        Box::pin(async move {
            let msg = self.stream.lock().unwrap().take();
            match msg {
                Some(msg) => Ok(Box::new(OneStream(Some(msg))) as Box<dyn ServerStream>),
                None => std::future::pending().await,
            }
        })
    }
    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        Box::pin(std::future::pending())
    }
    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        Box::pin(std::future::pending())
    }
}

// CF1: the stream a screening relay adds to every publish session must not
// disturb a broadcaster. This test was run first against the released
// dispatch (2.6.0), which classifies 0x11 as Unknown and drops it, with the
// RelayNotices assertion absent: publishing carried on. This build reads the
// notice and still carries on.
#[tokio::test(start_paused = true)]
async fn a_relay_identity_stream_does_not_disturb_publishing() {
    let mut msg = Vec::new();
    gawk_wire::append_relay_identity(
        &mut msg,
        &gawk_wire::RelayIdentity {
            server_version: "1.42.0",
            name: "gawk home",
            notices: gawk_wire::RELAY_NOTICE_SCREENING,
        },
    )
    .unwrap();
    let leg = Arc::new(NoticeLeg {
        stream: Mutex::new(Some(msg)),
        ..NoticeLeg::default()
    });
    let (session, mut rx) =
        Session::start_with_session(config(), leg.clone(), Arc::new(MonotonicClock::new()));

    tokio::time::sleep(Duration::from_secs(5)).await;
    let mut notices = Vec::new();
    while let Ok(ev) = rx.try_recv() {
        assert!(
            !matches!(ev, EngineEvent::Ended { .. }),
            "the session ended: {ev:?}"
        );
        if let EngineEvent::RelayNotices { screening } = ev {
            notices.push(screening);
        }
    }
    assert_eq!(
        notices,
        vec![true],
        "one notice, read off the publish session"
    );
    session
        .sender()
        .send_datagram_best_effort(b"still publishing");
    assert!(
        leg.datagrams
            .lock()
            .unwrap()
            .iter()
            .any(|d| d == b"still publishing")
    );

    session.stop().await;
    expect(&mut rx, EngineEvent::Ended { error: None }).await;
}
