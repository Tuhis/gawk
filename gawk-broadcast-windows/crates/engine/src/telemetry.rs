//! The R28 telemetry reporter, mirroring the Linux Go reporter behavior for
//! behavior (docs/38 D15 / gawk-broadcast/internal/telemetry/reporter.go).
//! Everything downstream matches on the browser's field names, so the batch
//! shape here is a contract, not a style choice — the envelope test pins it.
//!
//! Posture rules ported verbatim:
//! - `Enabled == false` in the hello ⇒ collect nothing, send nothing —
//!   indistinguishable from a pre-R28 relay.
//! - No ingest URL ⇒ inert (the pairing rule decides the URL; a blank
//!   telemetry setting reports to the reference collector only when the
//!   relay is also the default one).
//! - Sampling decimates to the hello's cadence (floor 250 ms); flushing is
//!   an independent fixed 10 s.
//! - Bounded everywhere, shedding OLDEST: 64 samples / 256 events pending,
//!   8 batches queued, 4 MiB of JSON per session (then events-only, with an
//!   explicit `telemetry-budget-exhausted` event so the wire says so).
//! - Sends retry twice (2 s, 4 s) on transport errors / 429 / 5xx; other
//!   4xx are dropped immediately.
//! - The session token and broadcast key ride in the BODY, hex-encoded;
//!   there is no auth header and no raw broadcast ID anywhere.

use crate::clock::Clock;
use crate::stats::Stats;
use serde::Serialize;
use std::collections::VecDeque;
use std::sync::{Arc, Condvar, Mutex};
use std::time::Duration;

pub const FLUSH_INTERVAL: Duration = Duration::from_secs(10);
pub const SESSION_BYTE_BUDGET: usize = 4 << 20;
const MIN_REPORT_INTERVAL_MS: u64 = 250;
const MAX_PENDING_SAMPLES: usize = 64;
const MAX_PENDING_EVENTS: usize = 256;
const SEND_QUEUE_DEPTH: usize = 8;
const MAX_SEND_ATTEMPTS: u32 = 3;
const SEND_TIMEOUT: Duration = Duration::from_secs(10);
const KIND_MAX: usize = 64;
const DETAIL_MAX: usize = 256;

/// The identity a TelemetryHello carries (already parsed by dispatch).
#[derive(Debug, Clone)]
pub struct Hello {
    pub enabled: bool,
    pub report_interval_ms: u16,
    pub token: Vec<u8>,
    pub broadcast_key_hex: String,
}

#[derive(Serialize, Clone)]
struct App {
    version: String,
    surface: &'static str,
    browser: &'static str,
    os: &'static str,
}

#[derive(Serialize)]
struct Sample {
    #[serde(rename = "tMs")]
    t_ms: i64,
    stats: Stats,
}

#[derive(Serialize)]
struct Event {
    #[serde(rename = "tMs")]
    t_ms: i64,
    kind: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    detail: String,
}

#[derive(Serialize)]
struct Batch {
    v: u32,
    token: String,
    role: &'static str,
    #[serde(rename = "broadcastKey")]
    broadcast_key: String,
    seq: u64,
    #[serde(rename = "final")]
    is_final: bool,
    app: App,
    #[serde(rename = "startedAtMs")]
    started_at_ms: i64,
    samples: Vec<Sample>,
    events: Vec<Event>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    truncated: bool,
}

struct SessionState {
    token_hex: String,
    broadcast_key_hex: String,
    started_at_ms: i64,
    started_at_us: u64,
    report_interval_ms: u64,
    seq: u64,
    samples: Vec<Sample>,
    events: Vec<Event>,
    bytes_sent: usize,
    truncated: bool,
    last_sample_at_us: Option<u64>,
    last_flush_at_us: u64,
}

struct Inner {
    url: Option<String>,
    session: Option<SessionState>,
}

/// A batch bound for `url`; `None` is the sender-shutdown sentinel.
type Outbound = Option<(String, Vec<u8>)>;

struct SendQueue {
    queue: Mutex<VecDeque<Outbound>>,
    ready: Condvar,
}

/// The reporter. Clone-free: the shell owns one for the app's lifetime and
/// repoints/begins it per broadcast, exactly like the Linux shells.
pub struct Reporter {
    inner: Mutex<Inner>,
    app: App,
    clock: Arc<dyn Clock>,
    send: Arc<SendQueue>,
    sender: Mutex<Option<std::thread::JoinHandle<()>>>,
}

impl Reporter {
    pub fn new(version: &str, clock: Arc<dyn Clock>) -> Self {
        let send = Arc::new(SendQueue {
            queue: Mutex::new(VecDeque::new()),
            ready: Condvar::new(),
        });
        let sender = {
            let send = send.clone();
            std::thread::Builder::new()
                .name("telemetry-send".into())
                .spawn(move || sender_loop(&send))
                .expect("spawn telemetry sender")
        };
        Self {
            inner: Mutex::new(Inner {
                url: None,
                session: None,
            }),
            app: App {
                version: if version.is_empty() {
                    "dev".into()
                } else {
                    version.into()
                },
                surface: "broadcaster",
                browser: "gawk-broadcast-windows",
                os: "Windows",
            },
            clock,
            send,
            sender: Mutex::new(Some(sender)),
        }
    }

    /// Points the reporter at the resolved ingest URL (or `None` = off).
    /// Resolved per broadcast, like the Linux shells — the pairing rule
    /// lives in `config`, not here.
    pub fn set_url(&self, url: Option<String>) {
        self.inner.lock().unwrap().url = url;
    }

    /// Adopts a session identity. A second hello finishes the previous
    /// session first (final batch, seq restart) — reconnects mint fresh
    /// tokens.
    ///
    /// A hello with no ingest URL yet still adopts the session: the R37
    /// TelemetryEndpoint (0x12) rides its OWN uni stream and can land after
    /// the hello — refusing here would lose that race half the time (the
    /// docs/22 finding 9 lesson). Until a URL arrives nothing is sent and
    /// pending buffers stay bounded; if none ever does (the §4.10
    /// foreign-relay guard), the session ends having sent nothing.
    pub fn begin(&self, hello: &Hello) {
        self.finish();
        let mut inner = self.inner.lock().unwrap();
        if !hello.enabled {
            eprintln!("relay reports fleet telemetry is off; this session will not report");
            return;
        }
        if inner.url.is_none() {
            eprintln!(
                "telemetry hello received with no ingest URL resolved; nothing is sent unless the relay advertises one"
            );
        }
        let now_us = self.clock.now_us();
        inner.session = Some(SessionState {
            token_hex: hex(&hello.token),
            broadcast_key_hex: hello.broadcast_key_hex.clone(),
            started_at_ms: std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_millis() as i64)
                .unwrap_or(0),
            started_at_us: now_us,
            report_interval_ms: u64::from(hello.report_interval_ms).max(MIN_REPORT_INTERVAL_MS),
            seq: 0,
            samples: Vec::new(),
            events: Vec::new(),
            bytes_sent: 0,
            truncated: false,
            last_sample_at_us: None,
            last_flush_at_us: now_us,
        });
    }

    /// Offers a stats sample; decimated to the hello's cadence, refused
    /// after the byte budget (events still flow).
    pub fn report(&self, stats: Stats) {
        let now_us = self.clock.now_us();
        let mut inner = self.inner.lock().unwrap();
        let Some(s) = inner.session.as_mut() else {
            return;
        };
        if s.truncated {
            return;
        }
        if let Some(last) = s.last_sample_at_us
            && now_us.saturating_sub(last) < s.report_interval_ms * 1000
        {
            return;
        }
        s.last_sample_at_us = Some(now_us);
        let t_ms = (now_us.saturating_sub(s.started_at_us) / 1000) as i64;
        s.samples.push(Sample { t_ms, stats });
        while s.samples.len() > MAX_PENDING_SAMPLES {
            s.samples.remove(0);
        }
    }

    /// Records a lifecycle event (`error`, `ended`, `resuming`, `resumed`).
    pub fn event(&self, kind: &str, detail: &str) {
        let now_us = self.clock.now_us();
        let mut inner = self.inner.lock().unwrap();
        let Some(s) = inner.session.as_mut() else {
            return;
        };
        let t_ms = (now_us.saturating_sub(s.started_at_us) / 1000) as i64;
        s.events.push(Event {
            t_ms,
            kind: truncate_str(kind, KIND_MAX),
            detail: truncate_str(detail, DETAIL_MAX),
        });
        while s.events.len() > MAX_PENDING_EVENTS {
            s.events.remove(0);
        }
    }

    /// Called on the shell's cadence (~1 s); flushes when the 10 s interval
    /// has elapsed. Split from wall-clock threads so tests drive it with a
    /// fake clock.
    pub fn tick(&self) {
        let now_us = self.clock.now_us();
        let due = {
            let mut inner = self.inner.lock().unwrap();
            match inner.session.as_mut() {
                Some(s)
                    if now_us.saturating_sub(s.last_flush_at_us)
                        >= FLUSH_INTERVAL.as_micros() as u64 =>
                {
                    s.last_flush_at_us = now_us;
                    true
                }
                _ => false,
            }
        };
        if due {
            self.flush(false);
        }
    }

    /// Ends the session: flushes a final batch (always sent, even empty).
    pub fn finish(&self) {
        self.flush(true);
        self.inner.lock().unwrap().session = None;
    }

    fn flush(&self, is_final: bool) {
        let mut inner = self.inner.lock().unwrap();
        let url = inner.url.clone();
        let app = self.app.clone();
        let Some(s) = inner.session.as_mut() else {
            return;
        };
        let Some(url) = url else {
            return;
        };
        if s.samples.is_empty() && s.events.is_empty() && !is_final {
            return;
        }
        let batch = Batch {
            v: 1,
            token: s.token_hex.clone(),
            role: "broadcaster",
            broadcast_key: s.broadcast_key_hex.clone(),
            seq: s.seq,
            is_final,
            app,
            started_at_ms: s.started_at_ms,
            samples: std::mem::take(&mut s.samples),
            events: std::mem::take(&mut s.events),
            truncated: s.truncated,
        };
        s.seq += 1;
        let Ok(body) = serde_json::to_vec(&batch) else {
            return;
        };

        // The uncompressed-JSON session budget: crossing it flips the
        // reporter to events-only and SAYS SO in-band.
        s.bytes_sent += body.len();
        if !s.truncated && s.bytes_sent > SESSION_BYTE_BUDGET {
            s.truncated = true;
            let t_ms = (self.clock.now_us().saturating_sub(s.started_at_us) / 1000) as i64;
            s.events.push(Event {
                t_ms,
                kind: "telemetry-budget-exhausted".into(),
                detail: "events only from here".into(),
            });
        }
        drop(inner);

        let mut q = self.send.queue.lock().unwrap();
        while q.len() >= SEND_QUEUE_DEPTH {
            q.pop_front();
        }
        q.push_back(Some((url, body)));
        self.send.ready.notify_one();
    }
}

impl Drop for Reporter {
    fn drop(&mut self) {
        // Stop the sender thread; queued batches ahead of the sentinel
        // still go out.
        self.send.queue.lock().unwrap().push_back(None);
        self.send.ready.notify_one();
        if let Some(h) = self.sender.lock().unwrap().take() {
            let _ = h.join();
        }
    }
}

fn sender_loop(send: &SendQueue) {
    loop {
        let item = {
            let mut q = send.queue.lock().unwrap();
            loop {
                if let Some(item) = q.pop_front() {
                    break item;
                }
                q = send.ready.wait(q).unwrap();
            }
        };
        let Some((url, body)) = item else { return };
        deliver(&url, &body);
    }
}

/// POSTs one batch: up to 3 attempts with 2 s / 4 s backoff. Retryable:
/// transport errors, 429, 5xx. Other 4xx drop immediately (the request is
/// wrong; repeating it can't help).
fn deliver(url: &str, body: &[u8]) {
    for attempt in 1..=MAX_SEND_ATTEMPTS {
        match post(url, body) {
            Ok(status) if status < 300 => return,
            Ok(status) if status != 429 && (400..500).contains(&status) => return,
            _ => {}
        }
        if attempt < MAX_SEND_ATTEMPTS {
            std::thread::sleep(Duration::from_secs(2 * u64::from(attempt)));
        }
    }
}

fn post(url: &str, body: &[u8]) -> Result<u16, String> {
    // `http_status_as_error(false)` keeps a non-2xx response on the Ok arm so
    // deliver()'s status policy stays in one place; ureq 3 would otherwise
    // report it as Error::StatusCode.
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(SEND_TIMEOUT))
        .http_status_as_error(false)
        .build()
        .into();
    match agent
        .post(url)
        .header("Content-Type", "application/json")
        .send(body)
    {
        Ok(resp) => Ok(resp.status().as_u16()),
        Err(e) => Err(e.to_string()),
    }
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// Clips to `max` BYTES on a char boundary (the Go reporter clips on rune
/// boundaries; same result for the same input).
fn truncate_str(s: &str, max: usize) -> String {
    if s.len() <= max {
        return s.to_string();
    }
    let mut end = max;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    s[..end].to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::clock::testing::FakeClock;
    use std::io::{Read, Write};
    use std::sync::mpsc;

    fn hello() -> Hello {
        Hello {
            enabled: true,
            report_interval_ms: 250,
            token: vec![0u8; 24],
            broadcast_key_hex: "1a2b3c4d5e6f".into(),
        }
    }

    /// One-shot HTTP server: accepts connections, returns `status`, sends
    /// each body to the channel.
    fn serve(status: u16, n: usize) -> (String, mpsc::Receiver<String>) {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let url = format!("http://{}/ingest", listener.local_addr().unwrap());
        let (tx, rx) = mpsc::channel();
        std::thread::spawn(move || {
            for _ in 0..n {
                let Ok((mut conn, _)) = listener.accept() else {
                    return;
                };
                let mut buf = Vec::new();
                let mut tmp = [0u8; 4096];
                let body = loop {
                    let Ok(k) = conn.read(&mut tmp) else { return };
                    buf.extend_from_slice(&tmp[..k]);
                    if let Some(pos) = find_body(&buf) {
                        let need = content_length(&buf).unwrap_or(0);
                        if buf.len() - pos >= need {
                            break String::from_utf8_lossy(&buf[pos..pos + need]).to_string();
                        }
                    }
                    if k == 0 {
                        return;
                    }
                };
                let _ = tx.send(body);
                let _ = conn.write_all(
                    format!(
                        "HTTP/1.1 {status} X\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
                    )
                    .as_bytes(),
                );
            }
        });
        (url, rx)
    }

    fn find_body(buf: &[u8]) -> Option<usize> {
        buf.windows(4).position(|w| w == b"\r\n\r\n").map(|p| p + 4)
    }
    fn content_length(buf: &[u8]) -> Option<usize> {
        let head = std::str::from_utf8(&buf[..find_body(buf)?]).ok()?;
        head.lines().find_map(|l| {
            let (k, v) = l.split_once(':')?;
            k.eq_ignore_ascii_case("content-length")
                .then(|| v.trim().parse().ok())?
        })
    }

    // The envelope pin, mirroring TestReporterEnvelope: field names and
    // values downstream readers match on, and no raw broadcast ID.
    #[test]
    fn envelope_has_the_contract_shape() {
        let clock = Arc::new(FakeClock::default());
        let (url, rx) = serve(204, 1);
        let r = Reporter::new("1.2.3", clock.clone());
        r.set_url(Some(url));
        r.begin(&hello());
        r.report(Stats::default());
        r.event(
            "resuming",
            "K7XQ2M must not appear — wait, it must: detail is caller-chosen",
        );
        // ^ deliberate: the reporter never ADDS an ID; callers own detail.
        r.finish();

        let body = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["v"], 1);
        assert_eq!(v["role"], "broadcaster");
        assert_eq!(v["broadcastKey"], "1a2b3c4d5e6f");
        assert_eq!(v["token"], "0".repeat(48));
        assert_eq!(v["seq"], 0);
        assert_eq!(v["final"], true);
        assert_eq!(v["app"]["version"], "1.2.3");
        assert_eq!(v["app"]["surface"], "broadcaster");
        assert_eq!(v["app"]["browser"], "gawk-broadcast-windows");
        assert_eq!(v["app"]["os"], "Windows");
        assert!(v["samples"].as_array().is_some());
        assert!(v["events"].as_array().is_some());
        assert!(v.get("truncated").is_none(), "truncated omitted when false");
        // The sample's stats carry the browser's names.
        assert!(v["samples"][0]["stats"].get("encoderFps").is_some());
        assert!(v["samples"][0]["tMs"].is_number());
    }

    #[test]
    fn inert_without_url_and_when_fleet_disabled() {
        let clock = Arc::new(FakeClock::default());
        let r = Reporter::new("", clock.clone());
        // No URL: the session is adopted (a 0x12 may still arrive) but
        // nothing is ever queued for send — the §4.10 guard's reporter half.
        r.begin(&hello());
        r.report(Stats::default());
        r.event("resuming", "");
        r.finish(); // must not panic or send (no server exists to hit)
        assert!(
            r.send.queue.lock().unwrap().is_empty(),
            "no URL ever resolved: zero batches queued"
        );

        // URL set but fleet disabled.
        let r = Reporter::new("", clock);
        r.set_url(Some("http://127.0.0.1:1/ingest".into()));
        r.begin(&Hello {
            enabled: false,
            ..hello()
        });
        assert!(r.inner.lock().unwrap().session.is_none());
    }

    // The R37 settle-order race (docs/40 §4.10): the 0x12 endpoint rides its
    // own uni stream and can arrive AFTER the 0x0D hello. The hello must not
    // refuse the session just because no URL has resolved yet.
    #[test]
    fn an_advertised_url_arriving_after_the_hello_still_reports() {
        let clock = Arc::new(FakeClock::default());
        let (url, rx) = serve(204, 1);
        let r = Reporter::new("", clock.clone());
        r.begin(&hello()); // hello first, no URL resolved yet
        r.report(Stats::default());
        r.set_url(Some(url)); // the 0x12 lands
        r.finish();
        let body = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["final"], true);
        assert_eq!(v["samples"].as_array().unwrap().len(), 1);
    }

    #[test]
    fn samples_decimate_to_the_hello_cadence() {
        let clock = Arc::new(FakeClock::default());
        let r = Reporter::new("", clock.clone());
        r.set_url(Some("http://127.0.0.1:1/ingest".into()));
        r.begin(&Hello {
            report_interval_ms: 2000,
            ..hello()
        });
        // 10 s of 100 ms-spaced offers → ~5 accepted at 2 s cadence.
        for _ in 0..100 {
            r.report(Stats::default());
            clock.advance_ms(100);
        }
        let n = r
            .inner
            .lock()
            .unwrap()
            .session
            .as_ref()
            .unwrap()
            .samples
            .len();
        assert!((5..=6).contains(&n), "kept {n} samples");
    }

    #[test]
    fn pending_buffers_shed_oldest() {
        let clock = Arc::new(FakeClock::default());
        let r = Reporter::new("", clock.clone());
        r.set_url(Some("http://127.0.0.1:1/ingest".into()));
        r.begin(&hello());
        for i in 0..(MAX_PENDING_EVENTS + 10) {
            r.event(&format!("e{i}"), "");
        }
        let inner = r.inner.lock().unwrap();
        let events = &inner.session.as_ref().unwrap().events;
        assert_eq!(events.len(), MAX_PENDING_EVENTS);
        assert_eq!(
            events.last().unwrap().kind,
            format!("e{}", MAX_PENDING_EVENTS + 9)
        );
        assert_eq!(events[0].kind, "e10"); // oldest gone
    }

    #[test]
    fn second_begin_finishes_the_previous_session_and_restarts_seq() {
        let clock = Arc::new(FakeClock::default());
        let (url, rx) = serve(204, 2);
        let r = Reporter::new("", clock.clone());
        r.set_url(Some(url));
        r.begin(&hello());
        r.event("resuming", "");
        r.begin(&hello()); // finishes session 1 with a final batch
        let body = rx.recv_timeout(Duration::from_secs(5)).unwrap();
        let v: serde_json::Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["final"], true);
        assert_eq!(v["seq"], 0);
        // Session 2 restarts seq at 0 too.
        r.event("resumed", "");
        r.finish();
        let v: serde_json::Value =
            serde_json::from_str(&rx.recv_timeout(Duration::from_secs(5)).unwrap()).unwrap();
        assert_eq!(v["seq"], 0);
        assert_eq!(v["events"][0]["kind"], "resumed");
    }

    #[test]
    fn non_retryable_4xx_drops_without_retry() {
        // A 400 server that would panic the test if hit 3 times: serve(400, 1)
        // only answers once; deliver() returning promptly proves no retry.
        let (url, _rx) = serve(400, 1);
        let start = std::time::Instant::now();
        deliver(&url, b"{}");
        assert!(
            start.elapsed() < Duration::from_secs(1),
            "400 must not retry"
        );
    }

    #[test]
    fn truncate_respects_char_boundaries() {
        assert_eq!(truncate_str("abc", 2), "ab");
        // '€' is 3 bytes; clipping at 4 must not split it.
        assert_eq!(truncate_str("a€€", 4), "a€");
        assert_eq!(truncate_str("short", 64), "short");
    }
}
