//! The engine's memory gauges against the real queues (R76, docs/77 D4).
//! Its own test binary with a single test, because the gauges are
//! process-wide statics: any other test touching the same queues in
//! parallel would move the numbers under these assertions.

use gawk_engine::carrier::Delivery;
use gawk_engine::clock::testing::FakeClock;
use gawk_engine::gate::FrameGate;
use gawk_engine::media::AccessUnit;
use gawk_engine::memory::{self, Component};
use gawk_engine::relay::{
    BoxFuture, CancelSignal, CarrierStream, KeyframeOutcome, KeyframeWriter, RelaySession,
    SendDatagramError, ServerStream, SessionClose,
};
use gawk_engine::sender::Sender;
use gawk_wire as wire;
use std::sync::Arc;

/// A keyframe stream that lands at once.
struct InstantKeyframe;

impl KeyframeWriter for InstantKeyframe {
    fn write(
        self: Box<Self>,
        _msg: Vec<u8>,
        _c: CancelSignal,
    ) -> BoxFuture<'static, KeyframeOutcome> {
        Box::pin(async { KeyframeOutcome::Sent })
    }
    fn abort(self: Box<Self>, _code: u32) {}
}

/// An uplink that never drains: every carrier write parks forever, so the
/// lane's queue holds everything after the first frame.
struct StalledCarrier;

impl CarrierStream for StalledCarrier {
    fn set_priority(&mut self, _priority: i32) {}
    fn write<'a>(&'a mut self, _buf: &'a [u8]) -> BoxFuture<'a, Result<(), String>> {
        Box::pin(std::future::pending())
    }
    fn finish(&mut self) -> BoxFuture<'_, Result<(), String>> {
        Box::pin(std::future::pending())
    }
    fn reset(&mut self, _code: u32) {}
}

struct StalledRelay;

impl RelaySession for StalledRelay {
    fn send_datagram(&self, _d: &[u8]) -> Result<(), SendDatagramError> {
        Ok(())
    }
    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async { Ok(Box::new(InstantKeyframe) as Box<dyn KeyframeWriter>) })
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
    fn open_uplink_carrier(&self) -> BoxFuture<'_, Result<Box<dyn CarrierStream>, String>> {
        Box::pin(async { Ok(Box::new(StalledCarrier) as Box<dyn CarrierStream>) })
    }
}

fn au(keyframe: bool, len: usize, ts: u64) -> AccessUnit {
    AccessUnit {
        data: vec![0xAB; len],
        timestamp_us: ts,
        keyframe,
    }
}

async fn settle() {
    for _ in 0..100 {
        tokio::task::yield_now().await;
    }
}

#[tokio::test]
async fn each_engine_queue_reports_what_it_holds_and_gives_it_back() {
    // Nothing reports before its queue exists.
    for c in [Component::Encode, Component::Send, Component::Telemetry] {
        assert_eq!(memory::component(c), None, "{c:?}");
    }

    // encode: the FrameGate, measured at zero once built, AU bytes while
    // queued, zero again once popped or dropped.
    {
        let mut gate = FrameGate::new();
        assert_eq!(memory::component(Component::Encode), Some(0));
        gate.offer(au(true, 5000, 1));
        gate.offer(au(false, 700, 2));
        assert_eq!(memory::component(Component::Encode), Some(5700));
        gate.pop();
        assert_eq!(memory::component(Component::Encode), Some(700));
    }
    assert_eq!(
        memory::component(Component::Encode),
        Some(0),
        "a dropped gate gives its backlog back"
    );

    // send: a stalled carrier keeps the GOP's deltas queued, and the next
    // keyframe sheds them (docs/75 D3) — the gauge follows both ways.
    let sender = Sender::new(Arc::new(StalledRelay), Arc::new(FakeClock::default()));
    sender.set_uplink_delivery(Delivery::Reliable);
    sender.apply_capabilities(wire::RelayCapabilities {
        flags: wire::CAP_UPLINK_CARRIERS,
        parity_level: 0,
    });
    sender.set_codec("avc1.640028");
    sender.send_video(au(true, 4000, 1)).await;
    settle().await;
    for ts in 2..12 {
        sender.send_video(au(false, 10_000, ts)).await;
        settle().await;
    }
    let backed_up = memory::component(Component::Send).expect("the lane reports");
    // The first delta is stuck in the parked write; the other nine queue,
    // each a little over its AU size once chunked into records.
    assert!(backed_up >= 9 * 10_000, "{backed_up}");
    sender.send_video(au(true, 4000, 12)).await;
    settle().await;
    let after = memory::component(Component::Send).unwrap();
    assert!(
        after < 2 * 10_000,
        "the new GOP shed the old one's queue: {backed_up} → {after}"
    );

    // telemetry: declared by the reporter, zero with nothing queued.
    let _reporter = gawk_engine::telemetry::Reporter::new("test", Arc::new(FakeClock::default()));
    assert_eq!(memory::component(Component::Telemetry), Some(0));
}
