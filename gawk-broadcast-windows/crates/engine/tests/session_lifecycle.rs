//! Session lifecycle against a scripted relay, through the
//! `start_with_session` seam — the failure orderings a real relay will not
//! produce on demand.

use gawk_engine::clock::MonotonicClock;
use gawk_engine::relay::{
    BoxFuture, CancelSignal, KeyframeOutcome, KeyframeWriter, RelaySession, SendDatagramError,
    ServerStream, SessionClose,
};
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use std::sync::Arc;
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
