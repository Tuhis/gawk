//! The uplink lab's engine sender (docs/75 RU6). Where `main.rs` re-implements
//! the carrier rules, this replays the same frame-size trace through the
//! engine as it ships: `gawk_engine::transport::dial_publish` with the
//! `congestion` setting, `gawk_engine::sender::Sender` with the
//! `uplinkDelivery` setting and the relay's capabilities applied as the
//! session applies them, the audio lane, and the shell's 1 Hz
//! `uplink::UplinkMonitor` — the upload-bandwidth warning.
//!
//! Nothing here decides what is sent. The sender reaches the transport through
//! the engine's own seam (`RelaySession`), and the lab wraps that seam only to
//! watch it: every call is passed through unchanged and timestamped, so the
//! log has the lines `analyze.py` reads from the prototype —
//!
//! - `F,id,key,size,n,k,t_send,0` per frame (written at the end: `n` and `k`
//!   are what the engine emitted, read off its datagrams and carrier records;
//!   a frame the engine never emitted gets the default budget's counts),
//! - `K,id,t0,t1,outcome` per keyframe stream, `C,frame,t_fin,t_end,outcome`
//!   per carrier (`acked`, `failed`, `expired` past the deadline, `abandoned`
//!   reset unfinished), `O` on a carrier's prologue, `W` per carrier write
//!   that blocked over 1 ms, `E`/`M` on a datagram the transport refused,
//! - `T` and `S` path stats from quinn (`WtSession::quic_stats`) —
//!
//! and three of its own: `N,t,raised|cleared` when the warning changes, `Y`
//! once a second with the engine's counters, and `U` at the end with them.

use std::collections::HashMap;
use std::fs::File;
use std::io::{BufWriter, Write};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use gawk_engine::carrier::Delivery;
use gawk_engine::clock::MonotonicClock;
use gawk_engine::media::{
    AUDIO_BITRATE_BPS, AUDIO_CHANNELS, AUDIO_CODEC, AUDIO_FRAME_MS, AUDIO_SAMPLE_RATE, AccessUnit,
    AudioFormat, AudioPacket,
};
use gawk_engine::relay::{
    BoxFuture, CancelSignal, CarrierStream, KeyframeOutcome, KeyframeWriter, PathCounters,
    RelaySession, SendDatagramError, ServerStream, SessionClose,
};
use gawk_engine::sender::Sender;
use gawk_engine::stats::Stats;
use gawk_engine::transport::{Congestion, WtSession, dial_publish};
use gawk_engine::uplink::UplinkMonitor;
use gawk_wire as wire;

fn now_ns() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos() as i64
}

type Log = Arc<Mutex<BufWriter<File>>>;

fn log_line(log: &Log, line: String) {
    let mut w = log.lock().unwrap();
    let _ = writeln!(w, "{line}");
}

struct Args {
    url: String,
    trace: String,
    delivery: Delivery,
    congestion: Congestion,
    seconds: f64,
    fps: f64,
    parity: u8,
    carriers: bool,
    out: String,
}

fn parse_args() -> Args {
    let mut a = Args {
        url: "https://127.0.0.1:4600/lab".into(),
        trace: String::new(),
        delivery: Delivery::LowestLatency,
        congestion: Congestion::Cubic,
        seconds: 65.0,
        fps: 60.0,
        parity: 2,
        carriers: true,
        out: "send.csv".into(),
    };
    let v: Vec<String> = std::env::args().skip(1).collect();
    let mut i = 0;
    while i < v.len() {
        let val = v.get(i + 1).cloned().unwrap_or_default();
        match v[i].as_str() {
            "--url" => a.url = val,
            "--trace" => a.trace = val,
            // The prototype's words, so run.sh and fair/run.sh drive both.
            "--mode" => {
                a.delivery = match val.as_str() {
                    "datagram" => Delivery::LowestLatency,
                    "carrier" => Delivery::Reliable,
                    other => panic!("--mode is datagram or carrier, not {other}"),
                }
            }
            "--cc" => {
                a.congestion = Congestion::parse(&val)
                    .unwrap_or_else(|| panic!("--cc is cubic or bbr, not {val}"))
            }
            "--seconds" => a.seconds = val.parse().unwrap(),
            "--fps" => a.fps = val.parse().unwrap(),
            // The relay's advertised parity level (its -parity-default).
            "--parity" => a.parity = val.parse().unwrap(),
            // Whether the "relay" advertises CAP_UPLINK_CARRIERS.
            "--carriers" => a.carriers = val != "0",
            "--out" => a.out = val,
            other => panic!("unknown flag {other}"),
        }
        i += 2;
    }
    assert!(!a.trace.is_empty(), "--trace is required");
    a
}

/// `size,flags` per frame, as `ffprobe -show_entries packet=size,flags`
/// prints them; `K` in the flags marks a keyframe.
fn load_trace(path: &str) -> Vec<(usize, bool)> {
    let text = std::fs::read_to_string(path).expect("trace");
    text.lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| {
            let mut it = l.split(',');
            let size: usize = it.next().unwrap().trim().parse().unwrap();
            let flags = it.next().unwrap_or("");
            (size.max(1), flags.contains('K'))
        })
        .collect()
}

/// What the engine emitted for each delta: its data chunk count and parity
/// symbols, from the datagrams and records themselves.
#[derive(Default)]
struct Emitted {
    by_frame: HashMap<u32, (u16, u8)>,
}

impl Emitted {
    /// Notes one video or parity datagram; returns its frame ID.
    fn note(&mut self, d: &[u8]) -> Option<u32> {
        match wire::peek_type(d).ok()?.1 {
            wire::TYPE_VIDEO_CHUNK => {
                let (h, _) = wire::parse_video_chunk(d).ok()?;
                let e = self.by_frame.entry(h.frame_id).or_default();
                e.0 = h.chunk_count;
                Some(h.frame_id)
            }
            wire::TYPE_PARITY_CHUNK => {
                let (h, _) = wire::parse_parity_chunk(d).ok()?;
                let e = self.by_frame.entry(h.frame_id).or_default();
                e.0 = h.chunk_count;
                e.1 = e.1.max(h.parity_index + 1);
                Some(h.frame_id)
            }
            _ => None,
        }
    }
}

/// The engine's transport, watched: every call goes to the `WtSession`
/// unchanged.
struct LabRelay {
    inner: Arc<WtSession>,
    log: Log,
    emitted: Arc<Mutex<Emitted>>,
}

impl RelaySession for LabRelay {
    fn send_datagram(&self, dgram: &[u8]) -> Result<(), SendDatagramError> {
        let fid = self.emitted.lock().unwrap().note(dgram);
        let r = self.inner.send_datagram(dgram);
        if let (Err(e), Some(fid)) = (&r, fid) {
            match e {
                SendDatagramError::TooLarge { max_datagram_size } => log_line(
                    &self.log,
                    format!("M,{fid},{},{}", max_datagram_size.unwrap_or(0), now_ns()),
                ),
                SendDatagramError::Failed(_) => log_line(&self.log, format!("E,{fid},failed")),
            }
        }
        r
    }

    fn open_keyframe_stream(&self) -> BoxFuture<'_, Result<Box<dyn KeyframeWriter>, String>> {
        Box::pin(async move {
            let w = self.inner.open_keyframe_stream().await?;
            Ok(Box::new(LabKeyframe {
                inner: w,
                log: self.log.clone(),
            }) as Box<dyn KeyframeWriter>)
        })
    }

    fn accept_uni(&self) -> BoxFuture<'_, Result<Box<dyn ServerStream>, String>> {
        self.inner.accept_uni()
    }

    fn receive_datagram(&self) -> BoxFuture<'_, Result<Vec<u8>, String>> {
        self.inner.receive_datagram()
    }

    fn closed(&self) -> BoxFuture<'_, SessionClose> {
        self.inner.closed()
    }

    fn close(&self) {
        self.inner.close()
    }

    fn path_counters(&self) -> Option<PathCounters> {
        self.inner.path_counters()
    }

    fn remote_address(&self) -> Option<std::net::SocketAddr> {
        self.inner.remote_address()
    }

    fn open_uplink_carrier(&self) -> BoxFuture<'_, Result<Box<dyn CarrierStream>, String>> {
        Box::pin(async move {
            let c = self.inner.open_uplink_carrier().await?;
            Ok(Box::new(LabCarrier {
                inner: c,
                log: self.log.clone(),
                emitted: self.emitted.clone(),
                first_frame: None,
                fin_at: None,
                settled: false,
            }) as Box<dyn CarrierStream>)
        })
    }
}

struct LabKeyframe {
    inner: Box<dyn KeyframeWriter>,
    log: Log,
}

impl KeyframeWriter for LabKeyframe {
    fn write(
        self: Box<Self>,
        msg: Vec<u8>,
        cancel: CancelSignal,
    ) -> BoxFuture<'static, KeyframeOutcome> {
        let id = wire::parse_stream_frame_header(&msg)
            .map(|h| h.frame_id)
            .unwrap_or(u32::MAX);
        let log = self.log.clone();
        let t0 = now_ns();
        let write = self.inner.write(msg, cancel);
        Box::pin(async move {
            let outcome = write.await;
            let word = match &outcome {
                KeyframeOutcome::Sent => "sent",
                KeyframeOutcome::Cancelled => "cancelled",
                KeyframeOutcome::Failed(_) => "failed",
            };
            log_line(&log, format!("K,{id},{t0},{},{word}", now_ns()));
            outcome
        })
    }

    fn abort(self: Box<Self>, code: u32) {
        self.inner.abort(code)
    }

    fn set_priority(&self, priority: i32) {
        self.inner.set_priority(priority)
    }
}

struct LabCarrier {
    inner: Box<dyn CarrierStream>,
    log: Log,
    emitted: Arc<Mutex<Emitted>>,
    /// The first delta it carried: names the carrier in `C` lines.
    first_frame: Option<u32>,
    /// When its FIN was asked for.
    fin_at: Option<i64>,
    /// Its outcome is logged.
    settled: bool,
}

impl LabCarrier {
    fn name(&self) -> String {
        self.first_frame.map_or("-".into(), |f| f.to_string())
    }
}

impl CarrierStream for LabCarrier {
    fn set_priority(&mut self, priority: i32) {
        self.inner.set_priority(priority)
    }

    fn write<'a>(&'a mut self, buf: &'a [u8]) -> BoxFuture<'a, Result<(), String>> {
        Box::pin(async move {
            let t0 = now_ns();
            let mut frame = None;
            if wire::parse_carrier_prologue(buf).is_ok() && buf.len() == wire::CARRIER_PROLOGUE_SIZE
            {
                log_line(&self.log, format!("O,-,{t0},{t0}"));
            } else {
                // The lane writes one frame's records per call.
                let mut rest = buf;
                let mut em = self.emitted.lock().unwrap();
                while let Ok((rec, more)) = wire::parse_carrier_record(rest) {
                    if let Some(fid) = em.note(rec) {
                        frame.get_or_insert(fid);
                    }
                    rest = more;
                }
                drop(em);
                if self.first_frame.is_none() {
                    self.first_frame = frame;
                }
            }
            let r = self.inner.write(buf).await;
            let t1 = now_ns();
            if t1 - t0 > 1_000_000
                && let Some(fid) = frame
            {
                log_line(&self.log, format!("W,{fid},{t0},{t1}"));
            }
            r
        })
    }

    fn finish(&mut self) -> BoxFuture<'_, Result<(), String>> {
        Box::pin(async move {
            let t_fin = now_ns();
            self.fin_at = Some(t_fin);
            let r = self.inner.finish().await;
            self.settled = true;
            let word = if r.is_ok() { "acked" } else { "failed" };
            log_line(
                &self.log,
                format!("C,{},{t_fin},{},{word}", self.name(), now_ns()),
            );
            r
        })
    }

    fn reset(&mut self, code: u32) {
        self.inner.reset(code);
        if !self.settled {
            self.settled = true;
            let now = now_ns();
            let (t_fin, word) = match self.fin_at {
                Some(t) => (t, "expired"),
                None => (now, "abandoned"),
            };
            log_line(&self.log, format!("C,{},{t_fin},{now},{word}", self.name()));
        }
    }
}

fn engine_line(kind: char, st: &Stats, warned: bool) -> String {
    format!(
        "{kind},{},{},{:.1},{:.1},{},{},{},{},{},{},{},{},{}",
        now_ns(),
        warned as u8,
        st.encoder_fps,
        st.sent_fps,
        st.keyframe_streams_superseded,
        st.keyframe_streams_failed,
        st.frames_dropped_at_send,
        st.uplink_packets_sent,
        st.uplink_packets_lost,
        st.uplink_carriers_opened,
        st.uplink_carriers_expired,
        st.uplink_records_shed,
        st.uplink_datagrams_dropped,
    )
}

// The desktop shell's runtime: multi-threaded, two workers (ui/src/shell.rs).
#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    let a = parse_args();
    let trace = load_trace(&a.trace);
    let log: Log = Arc::new(Mutex::new(BufWriter::new(File::create(&a.out).unwrap())));

    // The publish dial every shell makes (session.rs → dial_publish): the
    // controller from `congestion`; `insecure` for the lab's self-signed
    // certificate.
    let session = Arc::new(
        dial_publish(&a.url, "gawk-broadcast://linux", true, a.congestion)
            .await
            .unwrap_or_else(|e| panic!("dial: {}", e.message)),
    );
    eprintln!(
        "connected, delivery={} cc={}",
        a.delivery.as_str(),
        a.congestion.as_str()
    );
    let emitted = Arc::new(Mutex::new(Emitted::default()));
    let relay = Arc::new(LabRelay {
        inner: session.clone(),
        log: log.clone(),
        emitted: emitted.clone(),
    });
    let sender = Arc::new(Sender::new(relay, Arc::new(MonotonicClock::new())));
    // As Session::start_with_seams does, then as its server-stream loop does
    // with the relay's RelayCapabilities (the lab receiver sends none).
    sender.set_uplink_delivery(a.delivery);
    let mut flags = 0;
    if a.parity > 0 {
        flags |= wire::CAP_PARITY_CHUNKS;
    }
    if a.carriers {
        flags |= wire::CAP_UPLINK_CARRIERS;
    }
    sender.apply_capabilities(wire::RelayCapabilities {
        flags,
        parity_level: a.parity,
    });
    // A real H.264 codec string: every keyframe embeds its DecoderConfig.
    sender.set_codec("avc1.640028");
    sender.set_audio_format(AudioFormat {
        codec: AUDIO_CODEC.into(),
        sample_rate: AUDIO_SAMPLE_RATE,
        channels: AUDIO_CHANNELS,
        bitrate_bps: AUDIO_BITRATE_BPS,
        source: "uplinklab".into(),
    });

    let start = tokio::time::Instant::now() + Duration::from_millis(100);

    // The audio lane: one Opus packet every 20 ms at the shipped bitrate.
    let audio = {
        let s = sender.clone();
        tokio::spawn(async move {
            let bytes = (AUDIO_BITRATE_BPS as u64 * AUDIO_FRAME_MS / 8000) as usize;
            let mut iv = tokio::time::interval_at(start, Duration::from_millis(AUDIO_FRAME_MS));
            let mut ts = 0u64;
            loop {
                iv.tick().await;
                s.send_audio(AudioPacket {
                    data: vec![0xFC; bytes],
                    timestamp_us: ts,
                });
                ts += AUDIO_FRAME_MS * 1000;
            }
        })
    };

    // The shell's 1 Hz tick: the stats snapshot, the upload-bandwidth
    // monitor on it, and quinn's path state.
    let ticker = {
        let s = sender.clone();
        let log = log.clone();
        let session = session.clone();
        let cc = a.congestion;
        tokio::spawn(async move {
            let mut monitor = UplinkMonitor::new();
            let mut warned = false;
            let mut iv = tokio::time::interval_at(start, Duration::from_secs(1));
            loop {
                iv.tick().await;
                let mut st = s.stats();
                st.uplink_congestion = cc.as_str().to_owned();
                let w = monitor.observe(&st);
                if w != warned {
                    warned = w;
                    let word = if w { "raised" } else { "cleared" };
                    log_line(&log, format!("N,{},{word}", now_ns()));
                    eprintln!("uplink warning {word}");
                }
                log_line(&log, engine_line('Y', &st, warned));
                let q = session.quic_stats();
                log_line(
                    &log,
                    format!(
                        "T,{},{},{},{},{},{},{},{}",
                        now_ns(),
                        q.path.rtt.as_micros(),
                        q.path.cwnd,
                        q.path.sent_packets,
                        q.path.lost_packets,
                        q.path.lost_bytes,
                        q.path.congestion_events,
                        q.frame_tx.datagram
                    ),
                );
            }
        })
    };

    let interval = Duration::from_secs_f64(1.0 / a.fps);
    let n = (a.seconds * a.fps) as usize;
    let payload = vec![0x5Au8; trace.iter().map(|t| t.0).max().unwrap()];
    let mut sent = Vec::with_capacity(n);
    for i in 0..n {
        tokio::time::sleep_until(start + interval.mul_f64(i as f64)).await;
        let (size, key) = trace[i % trace.len()];
        let t = now_ns();
        sender
            .send_video(AccessUnit {
                data: payload[..size].to_vec(),
                timestamp_us: (i as f64 * 1e6 / a.fps) as u64,
                keyframe: key,
            })
            .await;
        sent.push((i as u32, key, size, t));
    }
    // Let the tail drain, then record the final path state.
    tokio::time::sleep(Duration::from_millis(1500)).await;
    ticker.abort();
    audio.abort();
    let q = session.quic_stats();
    let offered = session.path_counters().map_or(0, |c| c.datagrams_offered);
    let st = sender.stats();
    {
        let em = emitted.lock().unwrap();
        let budget = wire::ChunkBudget::new();
        for (id, key, size, t) in sent {
            let (n, k) = if key {
                (0, 0)
            } else {
                em.by_frame.get(&id).map_or_else(
                    || {
                        // Never emitted (shed before a write): what the
                        // engine would have cut it into.
                        let n = size.div_ceil(budget.payload());
                        let k = if a.parity > 0 && n <= wire::MAX_PARITY_DATA_CHUNKS {
                            a.parity
                        } else {
                            0
                        };
                        (n as u16, k)
                    },
                    |&(n, k)| (n, k),
                )
            };
            log_line(&log, format!("F,{id},{},{size},{n},{k},{t},0", key as u8));
        }
    }
    log_line(
        &log,
        format!(
            "S,{},{},{},{},{},{},{},{},{}",
            q.path.rtt.as_micros(),
            q.path.cwnd,
            q.path.sent_packets,
            q.path.lost_packets,
            q.path.lost_bytes,
            q.path.congestion_events,
            q.frame_tx.datagram,
            offered,
            q.udp_tx.bytes
        ),
    );
    log_line(&log, engine_line('U', &st, false));
    sender.wait().await;
    log.lock().unwrap().flush().unwrap();
    session.close();
    tokio::time::sleep(Duration::from_millis(200)).await;
}
