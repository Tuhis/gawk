//! The uplink carrier lane (R73 RU3, docs/75 D1, D3, D4): in *Reliable*
//! delivery, each GOP's deltas leave on one publisher-opened uni stream — a
//! carrier — instead of as datagrams, so QUIC repairs a lost packet rather
//! than every viewer losing the frame and, with it, the rest of the GOP.
//!
//! A carrier starts with the R19 prologue `0x01 0x0A` and carries records of
//! `u16 len ‖ datagram`, each byte for byte the `VideoChunk` or
//! `ParityChunk` datagram *Lowest latency* would have sent (D1). The relay
//! ingests a record exactly as that datagram.
//!
//! What this module enforces is D3's "nothing outlives its GOP at the
//! sender", because nothing downstream can:
//!
//! - **The encoder never waits.** Records go through a queue drained by one
//!   writer task; enqueueing is a lock and a push.
//! - **Shedding.** When a newer keyframe is produced, records still queued
//!   for an older GOP are dropped unwritten, a carrier for a GOP superseded
//!   before it opened is never opened, and a write still blocked on an older
//!   GOP's carrier is abandoned and that carrier reset at once (F10: without
//!   these a starved uplink queued 16–70 s of video, and one GOP's stream
//!   trickled on for 25–36 s after it was superseded).
//! - **Retiring.** At rotation the previous carrier is finished (FIN) and
//!   reset with [`UPLINK_CARRIER_EXPIRED_CODE`] if it is not fully
//!   acknowledged [`CARRIER_RETIRE_DEADLINE`] later — quinn reports no
//!   per-offset acknowledgement, so the GOP boundary is where a deadline can
//!   be enforced at all.
//! - **At most [`MAX_OPEN_CARRIERS`]** carriers are live (D2): the current
//!   GOP's and one draining. Before a third opens, the oldest draining one is
//!   reset, and the open waits until that reset has been handed to the
//!   transport. The relay accepts one more than this, because a reset can
//!   still be in flight when the next carrier's first frame arrives (review
//!   of #541).

use crate::memory;
use crate::relay::{CarrierStream, RelaySession};
use gawk_wire as wire;
use std::collections::VecDeque;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::{Notify, oneshot, watch};
use tokio::task::JoinHandle;

/// Stream error code for a carrier reset because its GOP is over and it was
/// not fully acknowledged in time (docs/75 D3). Application-defined, like
/// [`crate::sender::KEYFRAME_SUPERSEDED_CODE`]; the relay treats any reset
/// the same way (it ends the carrier).
pub const UPLINK_CARRIER_EXPIRED_CODE: u32 = 2;

/// How long a retired carrier may take to be fully acknowledged before it is
/// reset (docs/75 D3; the relay sizes its receive windows on the same
/// figure, D10).
pub const CARRIER_RETIRE_DEADLINE: Duration = Duration::from_millis(150);

/// The most carriers the sender keeps live at once (docs/75 D2): the current
/// GOP's and one draining. The relay's cap is one higher, as slack for a
/// reset that is still in flight.
pub const MAX_OPEN_CARRIERS: usize = 2;

/// Send priority of a keyframe stream: ahead of a carrier, because a GOP's
/// deltas are useless until its keyframe has landed (docs/75 D3) — and the
/// relay drops records behind the newest keyframe it has ingested.
pub const KEYFRAME_STREAM_PRIORITY: i32 = 1;

/// Send priority of a carrier (quinn's default).
pub const CARRIER_STREAM_PRIORITY: i32 = 0;

/// The Quality page's label for the setting below (docs/75 §6).
pub const DELIVERY_LABEL: &str = "Delivery";

/// The broadcaster's **Delivery** setting (docs/75 D4, §6): how deltas cross
/// the uplink when the relay supports carriers. Persisted as
/// `uplinkDelivery` and reported as `uplinkModeRequested`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Delivery {
    /// Deltas on a carrier per GOP: a lost packet is repaired, viewers wait
    /// a moment for it instead of losing the picture.
    Reliable,
    /// Deltas as datagrams with parity — today's path, unchanged (D6).
    LowestLatency,
}

/// The engine's default delivery: *Lowest latency* until RU7's hardware
/// pass holds, then *Reliable* (docs/75 D4, §8 step 3). This constant is the
/// one place that flip happens; a blank `uplinkDelivery` follows it.
pub const DEFAULT_DELIVERY: Delivery = Delivery::LowestLatency;

impl Default for Delivery {
    fn default() -> Self {
        DEFAULT_DELIVERY
    }
}

impl Delivery {
    /// Both choices, in the Quality page's order (§6).
    pub const ALL: [Delivery; 2] = [Delivery::Reliable, Delivery::LowestLatency];

    /// The `broadcast.json` value and the telemetry/diagnostics word.
    pub fn as_str(self) -> &'static str {
        match self {
            Delivery::Reliable => "reliable",
            Delivery::LowestLatency => "lowest-latency",
        }
    }

    /// Parses a stored value; anything else (blank included) is `None`, which
    /// the config layer resolves to [`DEFAULT_DELIVERY`].
    pub fn parse(s: &str) -> Option<Delivery> {
        match s.trim() {
            "reliable" => Some(Delivery::Reliable),
            "lowest-latency" => Some(Delivery::LowestLatency),
            _ => None,
        }
    }

    /// The segmented control's label (docs/75 §6, normative).
    pub fn label(self) -> &'static str {
        match self {
            Delivery::Reliable => "Reliable",
            Delivery::LowestLatency => "Lowest latency",
        }
    }

    /// The hint under the selected choice (docs/75 §6, normative). Main-flow
    /// copy: no principle-3 jargon (`lossnotice`'s string test).
    pub fn hint(self) -> &'static str {
        match self {
            Delivery::Reliable => {
                "If your connection drops some video, viewers wait a moment for it instead of losing the picture."
            }
            Delivery::LowestLatency => {
                "Viewers never wait for dropped video. If your connection drops some, they see brief freezes."
            }
        }
    }
}

/// How the current GOP's deltas leave (`uplinkMode`, docs/75 D8).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum UplinkMode {
    /// Datagrams with parity: *Lowest latency*, or a relay without
    /// `CAP_UPLINK_CARRIERS`, or before its capabilities arrived.
    #[default]
    Datagram,
    /// One carrier per GOP.
    Carrier,
}

impl UplinkMode {
    pub fn as_str(self) -> &'static str {
        match self {
            UplinkMode::Datagram => "datagram",
            UplinkMode::Carrier => "carrier",
        }
    }
}

/// D4's engage rule, the whole of it: the relay advertised carriers on this
/// session and the broadcaster chose *Reliable*. No automatic switching.
pub fn engage(relay_offers_carriers: bool, delivery: Delivery) -> UplinkMode {
    if relay_offers_carriers && delivery == Delivery::Reliable {
        UplinkMode::Carrier
    } else {
        UplinkMode::Datagram
    }
}

/// The lane's counters, folded into [`crate::stats::Stats`] by the sender.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LaneCounters {
    /// Carriers opened (their prologue written or being written).
    pub carriers_opened: u64,
    /// Carriers reset before they were fully acknowledged: retired past the
    /// deadline, abandoned on a blocked write, or reset to make room.
    pub carriers_expired: u64,
    /// Records that never reached a carrier: shed with a superseded GOP,
    /// abandoned mid-write, or with no carrier to take them.
    pub records_shed: u64,
    /// Frames whose records were all written to a carrier.
    pub frames_sent: u64,
    /// Data-chunk datagram bytes written — what datagram mode counts in
    /// `bytesSent`: no parity (that is `parity_bytes_sent`), no record
    /// prefixes, so the counter means the same in both delivery modes.
    pub bytes_sent: u64,
    /// Parity records written, and their datagram bytes.
    pub parity_chunks_sent: u64,
    pub parity_bytes_sent: u64,
}

/// One delta frame's records, ready to write: its chunks then its parity,
/// each `u16 len ‖ datagram` (D1).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub(crate) struct FrameRecords {
    pub bytes: Vec<u8>,
    pub records: u64,
    /// The data chunks' datagram bytes, prefixes excluded.
    pub data_bytes: u64,
    pub parity_records: u64,
    pub parity_bytes: u64,
}

impl FrameRecords {
    /// Frames the datagrams of one delta as records, verbatim.
    pub fn new(chunks: &[Vec<u8>], parity: &[Vec<u8>]) -> Result<Self, wire::WireError> {
        let len: usize = chunks.iter().chain(parity).map(|d| d.len() + 2).sum();
        let mut f = FrameRecords {
            bytes: Vec::with_capacity(len),
            ..Default::default()
        };
        for d in chunks {
            wire::append_carrier_record(&mut f.bytes, d)?;
            f.records += 1;
            f.data_bytes += d.len() as u64;
        }
        for d in parity {
            wire::append_carrier_record(&mut f.bytes, d)?;
            f.records += 1;
            f.parity_records += 1;
            f.parity_bytes += d.len() as u64;
        }
        Ok(f)
    }
}

enum Item {
    /// A keyframe was produced: GOP `gop` begins, on a carrier or not.
    Gop { gop: u64, carrier: bool },
    Frame {
        gop: u64,
        frame: FrameRecords,
        /// The frame's bytes in the `send` memory gauge until it is written,
        /// shed or cleared (docs/77 D4): the queue is unbounded within a GOP.
        _held: memory::Held,
    },
}

struct LaneState {
    queue: VecDeque<Item>,
    /// The GOP the next enqueued frame belongs to.
    gop: u64,
    closed: bool,
    task: Option<JoinHandle<()>>,
    counters: LaneCounters,
}

struct Shared {
    st: Mutex<LaneState>,
    wake: Notify,
    /// The newest GOP: a write or an open for an older one is superseded.
    newest: watch::Sender<u64>,
}

impl Shared {
    fn count(&self, f: impl FnOnce(&mut LaneCounters)) {
        f(&mut self.st.lock().unwrap().counters);
    }

    /// Resolves once a GOP newer than `gop` exists (or the lane closed,
    /// which bumps the GOP too).
    async fn superseded(&self, gop: u64) {
        let mut rx = self.newest.subscribe();
        let _ = rx.wait_for(|newest| *newest > gop).await;
    }

    async fn next(&self) -> Option<Item> {
        loop {
            {
                let mut st = self.st.lock().unwrap();
                if let Some(item) = st.queue.pop_front() {
                    return Some(item);
                }
                if st.closed {
                    return None;
                }
            }
            self.wake.notified().await;
        }
    }
}

/// The carrier lane: the queue, the writer task, the retiring carriers.
pub(crate) struct CarrierLane {
    shared: Arc<Shared>,
    relay: Arc<Mutex<Arc<dyn RelaySession>>>,
}

impl CarrierLane {
    /// `relay` is the sender's current-session slot: a carrier opens on
    /// whatever session is current when its GOP begins.
    pub fn new(relay: Arc<Mutex<Arc<dyn RelaySession>>>) -> Self {
        Self {
            shared: Arc::new(Shared {
                st: Mutex::new(LaneState {
                    queue: VecDeque::new(),
                    gop: 0,
                    closed: false,
                    task: None,
                    counters: LaneCounters::default(),
                }),
                wake: Notify::new(),
                newest: watch::Sender::new(0),
            }),
            relay,
        }
    }

    /// A keyframe was produced: a new GOP begins, on a carrier when
    /// `carrier`. Everything queued for an older GOP is shed now, and a
    /// write blocked on an older GOP's carrier is abandoned. Never waits.
    pub fn new_gop(&self, carrier: bool) {
        let mut st = self.shared.st.lock().unwrap();
        if st.closed {
            return;
        }
        st.gop += 1;
        let gop = st.gop;
        let mut shed = 0;
        // Older GOP markers go too: a carrier superseded before it opened is
        // never opened. The newest marker retires whatever is current.
        st.queue.retain(|item| match item {
            Item::Frame { frame, .. } => {
                shed += frame.records;
                false
            }
            Item::Gop { .. } => false,
        });
        st.counters.records_shed += shed;
        if !carrier && st.task.is_none() {
            // No writer has ever run: there is no carrier to retire.
            drop(st);
            self.shared.newest.send_replace(gop);
            return;
        }
        st.queue.push_back(Item::Gop { gop, carrier });
        if st.task.is_none() {
            st.task = Some(tokio::spawn(run(self.shared.clone(), self.relay.clone())));
        }
        drop(st);
        self.shared.newest.send_replace(gop);
        self.shared.wake.notify_one();
    }

    /// Queues one delta frame's records for the current GOP's carrier.
    /// Never waits.
    pub fn enqueue(&self, frame: FrameRecords) {
        let mut st = self.shared.st.lock().unwrap();
        if st.closed {
            return;
        }
        let gop = st.gop;
        let _held = memory::Held::new(memory::Component::Send, frame.bytes.len() as u64);
        st.queue.push_back(Item::Frame { gop, frame, _held });
        drop(st);
        self.shared.wake.notify_one();
    }

    pub fn counters(&self) -> LaneCounters {
        self.shared.st.lock().unwrap().counters
    }

    /// Teardown: nothing more is queued or written, the current carrier is
    /// reset, and the writer task is returned to be awaited.
    pub fn close(&self) -> Option<JoinHandle<()>> {
        let mut st = self.shared.st.lock().unwrap();
        st.closed = true;
        st.queue.clear();
        st.gop += 1;
        let gop = st.gop;
        let task = st.task.take();
        drop(st);
        self.shared.newest.send_replace(gop);
        self.shared.wake.notify_one();
        task
    }
}

/// A carrier finishing in the background, and the means to cut it short.
struct Retiring {
    reset_now: Option<oneshot::Sender<()>>,
    task: JoinHandle<()>,
}

impl Retiring {
    /// Resets the carrier now and returns once the reset has been handed to
    /// the transport: the caller opens the next carrier only after it, so
    /// RESET_STREAM is queued ahead of the new stream (review of #541). The
    /// relay keeps one slot of slack for a reset still in flight (D2).
    async fn force_reset(mut self) {
        if let Some(tx) = self.reset_now.take() {
            let _ = tx.send(());
        }
        let _ = self.task.await;
    }
}

/// Retires a carrier: FIN, then a reset if it is not fully acknowledged by
/// the deadline (or if the lane needs its slot sooner).
fn retire(shared: Arc<Shared>, mut carrier: Box<dyn CarrierStream>) -> Retiring {
    let (tx, rx) = oneshot::channel::<()>();
    let task = tokio::spawn(async move {
        let expired = {
            let finish = carrier.finish();
            tokio::pin!(finish);
            let forced = async {
                if rx.await.is_err() {
                    std::future::pending::<()>().await;
                }
            };
            tokio::select! {
                r = tokio::time::timeout(CARRIER_RETIRE_DEADLINE, &mut finish) => r.is_err(),
                _ = forced => true,
            }
        };
        if expired {
            carrier.reset(UPLINK_CARRIER_EXPIRED_CODE);
            shared.count(|c| c.carriers_expired += 1);
        }
    });
    Retiring {
        reset_now: Some(tx),
        task,
    }
}

/// Opens a carrier for `gop` and writes its prologue, unless the GOP is
/// superseded first.
async fn open(
    shared: &Shared,
    relay: &Mutex<Arc<dyn RelaySession>>,
    gop: u64,
) -> Option<Box<dyn CarrierStream>> {
    let session = relay.lock().unwrap().clone();
    let opened = tokio::select! {
        biased;
        r = session.open_uplink_carrier() => r.ok(),
        _ = shared.superseded(gop) => None,
    };
    let mut carrier = opened?;
    carrier.set_priority(CARRIER_STREAM_PRIORITY);
    shared.count(|c| c.carriers_opened += 1);
    let mut prologue = Vec::with_capacity(wire::CARRIER_PROLOGUE_SIZE);
    wire::append_carrier_prologue(&mut prologue);
    let written = tokio::select! {
        biased;
        r = carrier.write(&prologue) => r.is_ok(),
        _ = shared.superseded(gop) => false,
    };
    if !written {
        carrier.reset(UPLINK_CARRIER_EXPIRED_CODE);
        shared.count(|c| c.carriers_expired += 1);
        return None;
    }
    Some(carrier)
}

/// The writer task: drains the queue onto the current GOP's carrier.
async fn run(shared: Arc<Shared>, relay: Arc<Mutex<Arc<dyn RelaySession>>>) {
    let mut current: Option<(u64, Box<dyn CarrierStream>)> = None;
    let mut retiring: VecDeque<Retiring> = VecDeque::new();
    while let Some(item) = shared.next().await {
        match item {
            Item::Gop { gop, carrier } => {
                if let Some((_, old)) = current.take() {
                    retiring.push_back(retire(shared.clone(), old));
                }
                if !carrier || *shared.newest.borrow() > gop {
                    continue;
                }
                // Room for the new carrier. The reset is handed to the
                // transport before the open, so RESET_STREAM is queued first.
                retiring.retain(|r| !r.task.is_finished());
                while retiring.len() >= MAX_OPEN_CARRIERS {
                    if let Some(r) = retiring.pop_front() {
                        r.force_reset().await;
                    }
                }
                current = open(&shared, &relay, gop).await.map(|c| (gop, c));
            }
            Item::Frame { gop, frame, .. } => {
                let Some((_, carrier)) = current.as_mut().filter(|(g, _)| *g == gop) else {
                    // No carrier for this GOP: it failed to open, or died.
                    shared.count(|c| c.records_shed += frame.records);
                    continue;
                };
                let written = tokio::select! {
                    biased;
                    r = carrier.write(&frame.bytes) => Some(r.is_ok()),
                    _ = shared.superseded(gop) => None,
                };
                match written {
                    Some(true) => shared.count(|c| {
                        c.frames_sent += 1;
                        c.bytes_sent += frame.data_bytes;
                        c.parity_chunks_sent += frame.parity_records;
                        c.parity_bytes_sent += frame.parity_bytes;
                    }),
                    Some(false) => {
                        // The stream failed (stopped by the relay, or the
                        // connection died): the rest of this GOP has nowhere
                        // to go.
                        shared.count(|c| c.records_shed += frame.records);
                        current = None;
                    }
                    None => {
                        // Blocked on a superseded GOP's carrier: abandon the
                        // write and reset the carrier at once (D3).
                        if let Some((_, mut old)) = current.take() {
                            old.reset(UPLINK_CARRIER_EXPIRED_CODE);
                        }
                        shared.count(|c| {
                            c.carriers_expired += 1;
                            c.records_shed += frame.records;
                        });
                    }
                }
            }
        }
    }
    // Closed: teardown. The current carrier's GOP will never be completed;
    // retiring ones keep their deadline (bounded, and the session closing
    // ends them sooner).
    if let Some((_, mut c)) = current.take() {
        c.reset(UPLINK_CARRIER_EXPIRED_CODE);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::clock::testing::FakeClock;
    use crate::media::AccessUnit;
    use crate::relay::{
        BoxFuture, CancelSignal, KeyframeOutcome, KeyframeWriter, SendDatagramError, ServerStream,
        SessionClose,
    };
    use crate::sender::Sender;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// What one fake carrier saw.
    #[derive(Default, Debug)]
    struct CarrierLog {
        priority: Option<i32>,
        bytes: Vec<u8>,
        finished: bool,
        reset: Option<u32>,
    }

    /// A transport whose carrier writes, acknowledgements and opens the
    /// test scripts — the uplink, which is what varies.
    struct Inner {
        datagrams: Mutex<Vec<Vec<u8>>>,
        keyframes: Mutex<Vec<(Option<i32>, Vec<u8>)>>,
        carriers: Mutex<Vec<Arc<Mutex<CarrierLog>>>>,
        opens: AtomicUsize,
        /// Opens and resets, in the order the transport saw them.
        events: Arc<Mutex<Vec<String>>>,
        /// Carrier writes park while true.
        writes_block: watch::Sender<bool>,
        /// A finished carrier is fully acknowledged once this is true.
        acked: watch::Sender<bool>,
        /// Carrier opens park while true (no stream credit).
        opens_stall: watch::Sender<bool>,
    }

    #[derive(Clone)]
    struct FakeRelay(Arc<Inner>);

    impl FakeRelay {
        fn new() -> Self {
            FakeRelay(Arc::new(Inner {
                datagrams: Mutex::default(),
                keyframes: Mutex::default(),
                carriers: Mutex::default(),
                opens: AtomicUsize::new(0),
                events: Arc::default(),
                writes_block: watch::Sender::new(false),
                acked: watch::Sender::new(true),
                opens_stall: watch::Sender::new(false),
            }))
        }
        fn carriers(&self) -> Vec<Arc<Mutex<CarrierLog>>> {
            self.0.carriers.lock().unwrap().clone()
        }
        fn opens(&self) -> usize {
            self.0.opens.load(Ordering::SeqCst)
        }
        fn finished(&self, i: usize) -> bool {
            self.carriers()[i].lock().unwrap().finished
        }
        fn reset(&self, i: usize) -> Option<u32> {
            self.carriers()[i].lock().unwrap().reset
        }
        fn bytes(&self, i: usize) -> Vec<u8> {
            self.carriers()[i].lock().unwrap().bytes.clone()
        }
        /// Every video/parity datagram, in order.
        fn media_datagrams(&self) -> Vec<Vec<u8>> {
            self.0
                .datagrams
                .lock()
                .unwrap()
                .iter()
                .filter(|d| d[1] == wire::TYPE_VIDEO_CHUNK || d[1] == wire::TYPE_PARITY_CHUNK)
                .cloned()
                .collect()
        }
        fn keyframe_msgs(&self) -> Vec<Vec<u8>> {
            let k = self.0.keyframes.lock().unwrap();
            k.iter().map(|k| k.1.clone()).collect()
        }
        /// One carrier's records, after checking its prologue.
        fn records(&self, i: usize) -> Vec<Vec<u8>> {
            let bytes = self.bytes(i);
            wire::parse_carrier_prologue(&bytes).expect("prologue first");
            let mut rest = &bytes[wire::CARRIER_PROLOGUE_SIZE..];
            let mut out = Vec::new();
            while !rest.is_empty() {
                let (rec, more) = wire::parse_carrier_record(rest).unwrap();
                out.push(rec.to_vec());
                rest = more;
            }
            out
        }
    }

    struct FakeKeyframe {
        relay: FakeRelay,
        priority: Mutex<Option<i32>>,
    }

    impl KeyframeWriter for FakeKeyframe {
        fn write(
            self: Box<Self>,
            msg: Vec<u8>,
            _cancel: CancelSignal,
        ) -> BoxFuture<'static, KeyframeOutcome> {
            let prio = *self.priority.lock().unwrap();
            self.relay.0.keyframes.lock().unwrap().push((prio, msg));
            Box::pin(async { KeyframeOutcome::Sent })
        }
        fn abort(self: Box<Self>, _code: u32) {}
        fn set_priority(&self, priority: i32) {
            *self.priority.lock().unwrap() = Some(priority);
        }
    }

    struct FakeCarrier {
        index: usize,
        events: Arc<Mutex<Vec<String>>>,
        log: Arc<Mutex<CarrierLog>>,
        writes_block: watch::Receiver<bool>,
        acked: watch::Receiver<bool>,
    }

    impl CarrierStream for FakeCarrier {
        fn set_priority(&mut self, priority: i32) {
            self.log.lock().unwrap().priority = Some(priority);
        }
        fn write<'a>(&'a mut self, buf: &'a [u8]) -> BoxFuture<'a, Result<(), String>> {
            Box::pin(async move {
                let _ = self.writes_block.wait_for(|b| !*b).await;
                self.log.lock().unwrap().bytes.extend_from_slice(buf);
                Ok(())
            })
        }
        fn finish(&mut self) -> BoxFuture<'_, Result<(), String>> {
            self.log.lock().unwrap().finished = true;
            Box::pin(async move {
                let _ = self.acked.wait_for(|a| *a).await;
                Ok(())
            })
        }
        fn reset(&mut self, code: u32) {
            self.events
                .lock()
                .unwrap()
                .push(format!("reset {}", self.index));
            self.log.lock().unwrap().reset = Some(code);
        }
    }

    impl RelaySession for FakeRelay {
        fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError> {
            self.0.datagrams.lock().unwrap().push(dgram.to_vec());
            Ok(())
        }
        fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
            let w = FakeKeyframe {
                relay: self.clone(),
                priority: Mutex::new(None),
            };
            Box::pin(async move { Ok(Box::new(w) as Box<dyn KeyframeWriter>) })
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
            let n = self.0.opens.fetch_add(1, Ordering::SeqCst);
            self.0.events.lock().unwrap().push(format!("open {n}"));
            let mut stall = self.0.opens_stall.subscribe();
            Box::pin(async move {
                let _ = stall.wait_for(|s| !*s).await;
                let log = Arc::new(Mutex::new(CarrierLog::default()));
                let index = {
                    let mut c = self.0.carriers.lock().unwrap();
                    c.push(log.clone());
                    c.len() - 1
                };
                Ok(Box::new(FakeCarrier {
                    index,
                    events: self.0.events.clone(),
                    log,
                    writes_block: self.0.writes_block.subscribe(),
                    acked: self.0.acked.subscribe(),
                }) as Box<dyn CarrierStream>)
            })
        }
    }

    fn sender(relay: &FakeRelay) -> Sender {
        Sender::new(Arc::new(relay.clone()), Arc::new(FakeClock::default()))
    }

    fn offer(s: &Sender, carriers: bool, parity_level: u8) {
        let carrier_flag = if carriers {
            wire::CAP_UPLINK_CARRIERS
        } else {
            0
        };
        s.apply_capabilities(wire::RelayCapabilities {
            flags: wire::CAP_PARITY_CHUNKS | carrier_flag,
            parity_level,
        });
    }

    fn kf(ts: u64) -> AccessUnit {
        AccessUnit {
            data: vec![0xBB; 4000],
            timestamp_us: ts,
            keyframe: true,
        }
    }

    fn delta(len: usize, ts: u64) -> AccessUnit {
        AccessUnit {
            data: (0..len).map(|i| (i % 251) as u8).collect(),
            timestamp_us: ts,
            keyframe: false,
        }
    }

    /// Two GOPs with deltas of one, several, and many chunks.
    fn two_gops() -> Vec<AccessUnit> {
        vec![
            kf(1),
            delta(3000, 2),
            delta(100, 3),
            delta(9000, 4),
            kf(5),
            delta(2500, 6),
            delta(5000, 7),
        ]
    }

    async fn settle() {
        for _ in 0..100 {
            tokio::task::yield_now().await;
        }
    }

    /// Sends frames at an encoder's pace: the writer gets to run between
    /// them, as it does between two real frames.
    async fn send_all(s: &Sender, frames: Vec<AccessUnit>) {
        for f in frames {
            s.send_video(f).await;
            settle().await;
        }
    }

    fn reliable(relay: &FakeRelay, parity_level: u8) -> Sender {
        let s = sender(relay);
        offer(&s, true, parity_level);
        s.set_uplink_delivery(Delivery::Reliable);
        s
    }

    // RU3: one carrier per keyframe, and its records are byte for byte the
    // datagrams Lowest latency sends for the same frames, parity included
    // (D1) — the relay's ingest of a record is its ingest of a datagram.
    #[tokio::test(start_paused = true)]
    async fn a_carrier_per_gop_carries_the_datagrams_byte_for_byte() {
        let dg_relay = FakeRelay::new();
        let dg = sender(&dg_relay);
        offer(&dg, true, 2);
        dg.set_uplink_delivery(Delivery::LowestLatency);
        send_all(&dg, two_gops()).await;

        let cr_relay = FakeRelay::new();
        let cr = reliable(&cr_relay, 2);
        send_all(&cr, two_gops()).await;

        assert_eq!(cr_relay.carriers().len(), 2, "one carrier per keyframe");
        assert!(
            cr_relay.media_datagrams().is_empty(),
            "no delta as datagram"
        );
        let want = dg_relay.media_datagrams();
        let (first, second) = (cr_relay.records(0), cr_relay.records(1));
        assert!(
            first.iter().any(|r| r[1] == wire::TYPE_PARITY_CHUNK),
            "parity stays in the records"
        );
        assert_eq!(first, want[..first.len()], "GOP 1");
        assert_eq!(second, want[first.len()..], "GOP 2");
        assert_eq!(cr_relay.keyframe_msgs(), dg_relay.keyframe_msgs());

        let st = cr.stats();
        let dst = dg.stats();
        assert_eq!(st.uplink_mode, "carrier");
        assert_eq!(st.uplink_mode_requested, "reliable");
        assert_eq!(st.uplink_carriers_opened, 2);
        assert_eq!((st.uplink_carriers_expired, st.uplink_records_shed), (0, 0));
        assert_eq!(st.sent_frames, dst.sent_frames, "every frame sent");
        assert_eq!(st.parity_chunks_sent, dst.parity_chunks_sent);
        // Review of #541: the byte counters mean the same in both modes —
        // data chunks in bytesSent, parity only in parityBytesSent, no
        // record prefixes — or a Delivery switch skews every rate built on
        // them and bytesSent + parityBytesSent counts parity twice.
        assert_eq!(st.bytes_sent, dst.bytes_sent, "bytesSent");
        assert_eq!(
            st.parity_bytes_sent, dst.parity_bytes_sent,
            "parityBytesSent"
        );
        assert_eq!(st.datagrams_sent, 0);
    }

    // RU3: the keyframe stream goes ahead of the carrier (D3).
    #[tokio::test(start_paused = true)]
    async fn the_keyframe_stream_has_priority_1_and_the_carrier_0() {
        let relay = FakeRelay::new();
        let s = reliable(&relay, 2);
        send_all(&s, two_gops()).await;

        let prios: Vec<_> = relay
            .0
            .keyframes
            .lock()
            .unwrap()
            .iter()
            .map(|k| k.0)
            .collect();
        assert_eq!(prios, vec![Some(1), Some(1)]);
        assert_eq!(KEYFRAME_STREAM_PRIORITY, 1);
        for c in relay.carriers() {
            assert_eq!(c.lock().unwrap().priority, Some(0));
        }
        assert_eq!(CARRIER_STREAM_PRIORITY, 0);
    }

    // G9: with Lowest latency the wire output is today's, byte for byte,
    // whether or not the relay offers carriers.
    #[tokio::test(start_paused = true)]
    async fn lowest_latency_sends_todays_wire_byte_for_byte() {
        let today = FakeRelay::new();
        let t = sender(&today);
        offer(&t, false, 2); // a relay before R73
        send_all(&t, two_gops()).await;

        let now = FakeRelay::new();
        let n = sender(&now);
        offer(&n, true, 2);
        n.set_uplink_delivery(Delivery::LowestLatency);
        send_all(&n, two_gops()).await;

        assert_eq!(
            *now.0.datagrams.lock().unwrap(),
            *today.0.datagrams.lock().unwrap()
        );
        assert_eq!(now.keyframe_msgs(), today.keyframe_msgs());
        assert_eq!(now.opens(), 0);
        assert_eq!(n.stats().uplink_mode, "datagram");
    }

    // D4: one rule, no automatic switching.
    #[test]
    fn the_engage_rule_is_capability_and_reliable() {
        use Delivery::*;
        use UplinkMode::*;
        for (offers, delivery, want) in [
            (true, Reliable, Carrier),
            (false, Reliable, Datagram),
            (true, LowestLatency, Datagram),
            (false, LowestLatency, Datagram),
        ] {
            assert_eq!(engage(offers, delivery), want, "{offers} {delivery:?}");
        }
    }

    // D4: Reliable on a relay without the capability sends datagrams — an
    // old relay resets a carrier as bad input — and never opens one.
    #[tokio::test(start_paused = true)]
    async fn reliable_on_a_relay_without_carriers_sends_datagrams() {
        let relay = FakeRelay::new();
        let s = sender(&relay);
        offer(&s, false, 2);
        s.set_uplink_delivery(Delivery::Reliable);
        send_all(&s, two_gops()).await;
        assert_eq!(relay.opens(), 0);
        assert!(!relay.media_datagrams().is_empty());
        let st = s.stats();
        assert_eq!(
            (st.uplink_mode.as_str(), st.uplink_mode_requested.as_str()),
            ("datagram", "reliable")
        );
    }

    // D4: a Delivery change applies at the next keyframe, both ways, with
    // no restart.
    #[tokio::test(start_paused = true)]
    async fn a_delivery_change_applies_at_the_next_keyframe() {
        let relay = FakeRelay::new();
        let s = sender(&relay);
        offer(&s, true, 0);
        send_all(&s, vec![kf(1), delta(100, 2)]).await;
        assert_eq!(relay.media_datagrams().len(), 1);

        s.set_uplink_delivery(Delivery::Reliable);
        send_all(&s, vec![delta(100, 3)]).await;
        assert_eq!(relay.media_datagrams().len(), 2, "mid-GOP: still datagrams");
        assert!(relay.carriers().is_empty());

        send_all(&s, vec![kf(4), delta(100, 5)]).await;
        assert_eq!(relay.carriers().len(), 1, "the next keyframe opened one");
        assert_eq!(relay.records(0).len(), 1);
        assert_eq!(relay.media_datagrams().len(), 2);

        s.set_uplink_delivery(Delivery::LowestLatency);
        send_all(&s, vec![delta(100, 6)]).await;
        assert_eq!(relay.records(0).len(), 2, "mid-GOP: still the carrier");

        send_all(&s, vec![kf(7), delta(100, 8)]).await;
        assert_eq!(relay.media_datagrams().len(), 3, "back on datagrams");
        assert!(relay.finished(0), "the carrier was retired");
        assert_eq!(relay.carriers().len(), 1);
    }

    // D3, the queue half: a starved uplink sheds a superseded GOP instead
    // of queueing it, and the write blocked on its carrier is abandoned and
    // the carrier reset the moment the newer keyframe is produced — while
    // the encoder never waits.
    #[tokio::test(start_paused = true)]
    async fn a_superseded_gop_is_shed_and_its_blocked_carrier_reset_at_once() {
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0); // one record per small delta
        send_all(&s, vec![kf(1)]).await;
        assert_eq!(relay.carriers().len(), 1);

        relay.0.writes_block.send_replace(true);
        for ts in 2..5 {
            let sent = tokio::time::timeout(Duration::from_millis(1), s.send_video(delta(100, ts)));
            assert!(sent.await.is_ok(), "the encoder never waits");
        }
        settle().await;
        assert_eq!(relay.bytes(0).len(), wire::CARRIER_PROLOGUE_SIZE);
        assert_eq!(relay.reset(0), None, "blocked, not yet superseded");

        let sent = tokio::time::timeout(Duration::from_millis(1), s.send_video(kf(5)));
        assert!(sent.await.is_ok(), "the encoder never waits");
        settle().await;
        assert_eq!(
            relay.reset(0),
            Some(UPLINK_CARRIER_EXPIRED_CODE),
            "reset at once, not after a deadline"
        );
        assert!(!relay.finished(0), "abandoned, not finished");
        let st = s.stats();
        assert_eq!(
            st.uplink_records_shed, 3,
            "one abandoned, two never written"
        );
        assert_eq!(st.uplink_carriers_expired, 1);

        // The new GOP's carrier carries on once the uplink does.
        relay.0.writes_block.send_replace(false);
        send_all(&s, vec![delta(100, 6)]).await;
        assert_eq!(relay.carriers().len(), 2);
        assert_eq!(relay.records(1).len(), 1);
        assert_eq!(s.stats().sent_frames, 3, "two keyframes and one delta");
    }

    // D3: a carrier for a GOP superseded before it opened is never opened.
    #[tokio::test(start_paused = true)]
    async fn a_carrier_superseded_before_it_opened_is_never_opened() {
        // The writer is stuck on GOP 1's carrier while GOPs 2 and 3 arrive.
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0);
        send_all(&s, vec![kf(1)]).await;
        relay.0.writes_block.send_replace(true);
        s.send_video(delta(100, 2)).await;
        settle().await;
        s.send_video(kf(3)).await;
        s.send_video(delta(100, 4)).await;
        s.send_video(kf(5)).await;
        settle().await;
        relay.0.writes_block.send_replace(false);
        settle().await;
        assert_eq!(relay.opens(), 2, "GOPs 1 and 3 only");
        assert_eq!(s.stats().uplink_records_shed, 2);
        assert_eq!(relay.reset(0), Some(UPLINK_CARRIER_EXPIRED_CODE));

        // An open still waiting for stream credit is abandoned too.
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0);
        relay.0.opens_stall.send_replace(true);
        send_all(&s, vec![kf(1), delta(100, 2), kf(3)]).await;
        relay.0.opens_stall.send_replace(false);
        send_all(&s, vec![delta(100, 4)]).await;
        assert_eq!(relay.carriers().len(), 1, "GOP 1's carrier never opened");
        assert_eq!(relay.records(0).len(), 1, "GOP 2's delta");
        let st = s.stats();
        assert_eq!((st.uplink_carriers_opened, st.uplink_records_shed), (1, 1));
    }

    // D3, the retire half: FIN at rotation; a carrier still unacknowledged
    // 150 ms later is reset and counted, one acknowledged in time is not.
    #[tokio::test(start_paused = true)]
    async fn a_retired_carrier_is_reset_if_not_acknowledged_in_150_ms() {
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0);
        relay.0.acked.send_replace(false);
        send_all(&s, vec![kf(1), delta(100, 2), kf(3)]).await;
        assert!(relay.finished(0), "FIN at rotation");
        assert_eq!(relay.reset(0), None);

        tokio::time::sleep(CARRIER_RETIRE_DEADLINE - Duration::from_millis(1)).await;
        settle().await;
        assert_eq!(relay.reset(0), None, "inside the deadline");
        tokio::time::sleep(Duration::from_millis(2)).await;
        settle().await;
        assert_eq!(relay.reset(0), Some(UPLINK_CARRIER_EXPIRED_CODE));
        assert_eq!(s.stats().uplink_carriers_expired, 1);
        assert_eq!(s.stats().uplink_records_shed, 0, "its records were written");

        // Acknowledged inside the deadline: never reset.
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0);
        relay.0.acked.send_replace(false);
        send_all(&s, vec![kf(1), delta(100, 2), kf(3)]).await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        relay.0.acked.send_replace(true);
        settle().await;
        tokio::time::sleep(Duration::from_millis(200)).await;
        settle().await;
        assert_eq!(relay.reset(0), None);
        assert_eq!(s.stats().uplink_carriers_expired, 0);
    }

    // D2: the relay refuses a third concurrent carrier, so before a third
    // opens the oldest still draining is reset.
    #[tokio::test(start_paused = true)]
    async fn never_more_than_two_carriers_are_open() {
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0);
        relay.0.acked.send_replace(false);
        send_all(&s, vec![kf(1), delta(100, 2), kf(3), delta(100, 4)]).await;
        tokio::time::sleep(Duration::from_millis(50)).await;
        send_all(&s, vec![kf(5)]).await;
        assert_eq!(relay.carriers().len(), 3);
        let open = (0..3).filter(|&i| relay.reset(i).is_none()).count();
        assert!(open <= MAX_OPEN_CARRIERS, "{open} open");
        assert_eq!(relay.reset(0), Some(UPLINK_CARRIER_EXPIRED_CODE));
        assert_eq!(relay.reset(1), None, "still inside its deadline");
    }

    // Review of #541: the relay frees a carrier slot only when it sees the
    // reset, so the reset that makes room must reach the transport BEFORE
    // the next carrier is opened — not whenever the retiring task next runs.
    #[tokio::test(start_paused = true)]
    async fn the_reset_that_makes_room_is_sent_before_the_next_open() {
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0);
        relay.0.acked.send_replace(false);
        send_all(&s, vec![kf(1), delta(100, 2), kf(3), delta(100, 4)]).await;
        send_all(&s, vec![kf(5)]).await;
        let events = relay.0.events.lock().unwrap().clone();
        assert_eq!(events, ["open 0", "open 1", "reset 0", "open 2"]);
    }

    // D3: a new publisher session starts with no carrier, and opens one only
    // once ITS relay has advertised them, at the next keyframe.
    #[tokio::test(start_paused = true)]
    async fn a_resumed_session_starts_with_no_carrier() {
        let a = FakeRelay::new();
        let s = reliable(&a, 0);
        send_all(&s, vec![kf(1), delta(100, 2)]).await;
        assert_eq!(a.carriers().len(), 1);

        let b = FakeRelay::new();
        s.set_relay(Arc::new(b.clone()));
        settle().await;
        assert!(a.finished(0), "the old session's carrier retired");
        send_all(&s, vec![kf(3), delta(100, 4)]).await;
        assert_eq!(b.opens(), 0, "no capability seen yet");
        assert_eq!(b.media_datagrams().len(), 1);

        offer(&s, true, 0);
        send_all(&s, vec![delta(100, 5)]).await;
        assert_eq!(b.media_datagrams().len(), 2, "applies at the next keyframe");
        send_all(&s, vec![kf(6), delta(100, 7)]).await;
        assert_eq!(b.carriers().len(), 1);
        assert_eq!(b.records(0).len(), 1);
        assert_eq!(a.carriers().len(), 1, "nothing more on the old session");
    }

    // Teardown never waits on a starved uplink: the blocked carrier is
    // reset and the writer ends.
    #[tokio::test(start_paused = true)]
    async fn wait_resets_a_blocked_carrier_and_returns() {
        let relay = FakeRelay::new();
        let s = reliable(&relay, 0);
        send_all(&s, vec![kf(1)]).await;
        relay.0.writes_block.send_replace(true);
        send_all(&s, vec![delta(100, 2)]).await;
        let waited = tokio::time::timeout(Duration::from_millis(10), s.wait()).await;
        assert!(waited.is_ok(), "wait() must not hang on a blocked write");
        assert_eq!(relay.reset(0), Some(UPLINK_CARRIER_EXPIRED_CODE));
    }

    // docs/67: the iOS engine runs on a current-thread runtime. The lane
    // must make progress there — opening, writing, shedding, retiring —
    // with no second worker to lean on.
    #[test]
    fn the_lane_runs_on_a_current_thread_runtime() {
        let rt = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .start_paused(true)
            .build()
            .unwrap();
        rt.block_on(async {
            let relay = FakeRelay::new();
            let s = reliable(&relay, 2);
            relay.0.acked.send_replace(false);
            send_all(&s, two_gops()).await;
            assert_eq!(relay.carriers().len(), 2);
            assert!(!relay.records(0).is_empty());
            tokio::time::sleep(CARRIER_RETIRE_DEADLINE * 2).await;
            settle().await;
            assert_eq!(relay.reset(0), Some(UPLINK_CARRIER_EXPIRED_CODE));
            relay.0.writes_block.send_replace(true);
            send_all(&s, vec![delta(100, 9), kf(10)]).await;
            assert!(s.stats().uplink_records_shed > 0);
            s.wait().await;
        });
    }

    #[test]
    fn delivery_words_round_trip_and_blank_is_the_default() {
        for d in Delivery::ALL {
            assert_eq!(Delivery::parse(d.as_str()), Some(d));
        }
        assert_eq!(Delivery::parse(""), None);
        assert_eq!(Delivery::parse("auto"), None);
        assert_eq!(Delivery::default(), DEFAULT_DELIVERY);
        // docs/75 D4, §8 step 2: today's behaviour stays the default until
        // RU7's hardware pass flips it.
        assert_eq!(DEFAULT_DELIVERY, Delivery::LowestLatency);
    }

    // §6's copy, exactly.
    #[test]
    fn the_quality_page_copy_is_section_6s() {
        assert_eq!(
            Delivery::ALL.map(Delivery::label),
            ["Reliable", "Lowest latency"]
        );
        assert_eq!(
            Delivery::Reliable.hint(),
            "If your connection drops some video, viewers wait a moment for it instead of losing the picture."
        );
        assert_eq!(
            Delivery::LowestLatency.hint(),
            "Viewers never wait for dropped video. If your connection drops some, they see brief freezes."
        );
    }
}
