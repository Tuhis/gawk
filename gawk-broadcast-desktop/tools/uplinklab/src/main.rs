//! The uplink lab's sender (docs/74). Replays a frame-size trace from a real
//! encoder at the broadcaster's cadence over the engine's transport stack, in
//! one of two delta deliveries:
//!
//! - `datagram` — today's engine: chunks then parity, `send_datagram` each.
//! - `carrier`  — one reliable uni stream per GOP carrying the same
//!   datagrams as `uint16 len ‖ datagram` records (docs/24's format).
//!
//! Keyframes ride their own uni stream in both, with the engine's F-12 rule
//! (an in-flight write younger than 2 s finishes; the newest waits in one
//! pending slot). Every event is logged with the host wall clock in ns so the
//! receiver's log (gawk-server/cmd/gawk-uplinklab) lines up with it.

use std::fs::File;
use std::io::{BufWriter, Write};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use gawk_wire as wire;
use tokio::sync::{Notify, mpsc, watch};
use wtransport::config::QuicTransportConfig;
use wtransport::quinn::congestion;
use wtransport::{ClientConfig, Connection, Endpoint, VarInt};

const KEYFRAME_WRITE_CANCEL_AGE: Duration = Duration::from_secs(2);
const CARRIER_EXPIRED_CODE: u32 = 2;
/// How long a retired carrier has, after its FIN, to be fully acknowledged.
const CARRIER_RETIRE_GRACE: Duration = Duration::from_millis(150);

fn now_ns() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos() as i64
}

struct Args {
    url: String,
    trace: String,
    mode: String,
    seconds: f64,
    fps: f64,
    parity: usize,
    out: String,
    cc: String,
    datagram_buffer: usize,
    id_file: String,
}

fn parse_args() -> Args {
    let mut a = Args {
        url: "https://127.0.0.1:4600/lab".into(),
        trace: String::new(),
        mode: "datagram".into(),
        seconds: 65.0,
        fps: 60.0,
        parity: 2,
        out: "send.csv".into(),
        cc: "cubic".into(),
        datagram_buffer: 1024 * 1024,
        id_file: String::new(),
    };
    let v: Vec<String> = std::env::args().skip(1).collect();
    let mut i = 0;
    while i < v.len() {
        let val = v.get(i + 1).cloned().unwrap_or_default();
        match v[i].as_str() {
            "--url" => a.url = val,
            "--trace" => a.trace = val,
            "--mode" => a.mode = val,
            "--seconds" => a.seconds = val.parse().unwrap(),
            "--fps" => a.fps = val.parse().unwrap(),
            "--parity" => a.parity = val.parse().unwrap(),
            "--out" => a.out = val,
            "--cc" => a.cc = val,
            "--datagram-buffer" => a.datagram_buffer = val.parse().unwrap(),
            "--id-file" => a.id_file = val,
            other => panic!("unknown flag {other}"),
        }
        i += 2;
    }
    assert!(!a.trace.is_empty(), "--trace is required");
    assert!(
        a.mode == "datagram" || a.mode == "carrier",
        "--mode is datagram or carrier"
    );
    a
}

type Log = Arc<Mutex<BufWriter<File>>>;

fn log_line(log: &Log, line: String) {
    let mut w = log.lock().unwrap();
    let _ = writeln!(w, "{line}");
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

async fn connect(
    a: &Args,
) -> (
    Endpoint<wtransport::endpoint::endpoint_side::Client>,
    Connection,
) {
    let mut transport = QuicTransportConfig::default();
    // The engine sets exactly one transport option (transport.rs); the
    // other two are what the lab varies.
    transport.keep_alive_interval(Some(Duration::from_secs(10)));
    transport.datagram_send_buffer_size(a.datagram_buffer);
    match a.cc.as_str() {
        "cubic" => {}
        "bbr" => {
            transport.congestion_controller_factory(Arc::new(congestion::BbrConfig::default()));
        }
        other => panic!("unknown --cc {other}: cubic or bbr"),
    }
    let tls = wtransport::tls::client::build_default_tls_config(
        Arc::new(wtransport::tls::rustls::RootCertStore::empty()),
        Some(Arc::new(
            wtransport::tls::client::NoServerVerification::new(),
        )),
    );
    let config = ClientConfig::builder()
        .with_bind_default()
        .with_custom_tls_and_transport(tls, transport)
        .build();
    let endpoint = Endpoint::client(config).expect("endpoint");
    let opts = wtransport::endpoint::ConnectOptions::builder(&a.url)
        .add_header("origin", "gawk-broadcast://native")
        .build();
    let conn = endpoint.connect(opts).await.expect("connect");
    (endpoint, conn)
}

/// F-12's keyframe writer: one write in flight, the newest waiting.
struct KeyframeSlot {
    pending: Mutex<Option<(u32, Vec<u8>)>>,
    notify: Notify,
}

async fn keyframe_task(conn: Connection, slot: Arc<KeyframeSlot>, log: Log, priority: i32) {
    loop {
        let next = slot.pending.lock().unwrap().take();
        let Some((id, msg)) = next else {
            slot.notify.notified().await;
            continue;
        };
        let t0 = now_ns();
        let started = Instant::now();
        let mut st = match conn.open_uni().await {
            Ok(o) => match o.await {
                Ok(s) => s,
                Err(_) => return,
            },
            Err(_) => return,
        };
        st.set_priority(priority);
        let outcome = {
            let write = async {
                st.write_all(&msg).await?;
                st.finish().await
            };
            tokio::pin!(write);
            loop {
                tokio::select! {
                    r = &mut write => break if r.is_ok() { "sent" } else { "failed" },
                    _ = tokio::time::sleep(Duration::from_millis(50)) => {
                        if started.elapsed() > KEYFRAME_WRITE_CANCEL_AGE
                            && slot.pending.lock().unwrap().is_some()
                        {
                            break "cancelled";
                        }
                    }
                }
            }
        };
        if outcome == "cancelled" {
            let _ = st.reset(VarInt::from_u32(1));
        }
        log_line(&log, format!("K,{id},{t0},{},{outcome}", now_ns()));
    }
}

enum CarrierMsg {
    Rotate(u32),
    Records(u32, Vec<u8>),
}

/// The per-GOP carrier: a writer task draining a queue, so the frame loop
/// never blocks on the stream. On rotation the previous carrier finishes and
/// gets `CARRIER_RETIRE_GRACE` to be fully acknowledged, else it is reset —
/// quinn exposes no per-offset acknowledgement, so "fully acknowledged after
/// FIN" is the only deadline the sender can observe (docs/74 D3). Nothing for
/// a GOP that a newer keyframe has superseded (`latest`, the newest keyframe's
/// frameId) survives: queued records are shed unwritten, a carrier is never
/// opened for it, and a write still blocked on its carrier is abandoned and
/// the carrier reset. Without the last, a starved connection keeps one GOP's
/// stream alive — and trickling — for as long as the write blocks.
async fn carrier_task(
    conn: Connection,
    mut rx: mpsc::UnboundedReceiver<CarrierMsg>,
    log: Log,
    mut latest: watch::Receiver<u32>,
) {
    let mut current: Option<(u32, wtransport::SendStream)> = None;
    while let Some(msg) = rx.recv().await {
        let newest = *latest.borrow();
        match msg {
            CarrierMsg::Rotate(gop) if gop < newest => {
                // Superseded before it opened: retire the old one, open nothing.
                if let Some((old_gop, mut st)) = current.take() {
                    let _ = st.reset(VarInt::from_u32(CARRIER_EXPIRED_CODE));
                    log_line(&log, format!("C,{old_gop},{},{},shed", now_ns(), now_ns()));
                }
                log_line(&log, format!("G,{gop}"));
            }
            CarrierMsg::Records(frame, _) if frame < newest => {
                log_line(&log, format!("D,{frame}"));
            }
            CarrierMsg::Rotate(gop) => {
                if let Some((old_gop, mut st)) = current.take() {
                    let log = log.clone();
                    tokio::spawn(async move {
                        let t_fin = now_ns();
                        let r = tokio::time::timeout(CARRIER_RETIRE_GRACE, st.finish()).await;
                        let outcome = match r {
                            Ok(Ok(())) => "acked",
                            Ok(Err(_)) => "failed",
                            Err(_) => {
                                let _ = st.reset(VarInt::from_u32(CARRIER_EXPIRED_CODE));
                                "expired"
                            }
                        };
                        log_line(&log, format!("C,{old_gop},{t_fin},{},{outcome}", now_ns()));
                    });
                }
                let t0 = now_ns();
                let mut st = match conn.open_uni().await {
                    Ok(o) => match o.await {
                        Ok(s) => s,
                        Err(_) => return,
                    },
                    Err(_) => return,
                };
                // Below the keyframe stream: a GOP's deltas are useless
                // until its keyframe has landed.
                st.set_priority(0);
                let mut pro = Vec::with_capacity(2);
                wire::append_carrier_prologue(&mut pro);
                if st.write_all(&pro).await.is_err() {
                    return;
                }
                log_line(&log, format!("O,{gop},{t0},{}", now_ns()));
                current = Some((gop, st));
            }
            CarrierMsg::Records(frame, buf) => {
                if let Some((gop, st)) = current.as_mut() {
                    let gop = *gop;
                    let t0 = now_ns();
                    // write_all is not cancel-safe; an abandoned write's
                    // stream is reset, so the partial record never matters.
                    let superseded = tokio::select! {
                        r = st.write_all(&buf) => {
                            if r.is_err() {
                                current = None;
                            }
                            false
                        }
                        _ = latest.wait_for(|&k| k > gop) => true,
                    };
                    let t1 = now_ns();
                    // Only a write that actually waited is worth a line.
                    if t1 - t0 > 1_000_000 {
                        log_line(&log, format!("W,{frame},{t0},{t1}"));
                    }
                    if superseded && let Some((old_gop, mut st)) = current.take() {
                        let _ = st.reset(VarInt::from_u32(CARRIER_EXPIRED_CODE));
                        log_line(&log, format!("C,{old_gop},{t1},{t1},abandoned"));
                    }
                }
            }
        }
    }
}

fn chunk_frame(
    frame_id: u32,
    data: &[u8],
    ts_us: u64,
    parity: usize,
    budget: &wire::ChunkBudget,
) -> (Vec<Vec<u8>>, Vec<Vec<u8>>) {
    let payloads = wire::split_frame(data, budget).expect("split");
    let count = payloads.len() as u16;
    let mut dgrams = Vec::with_capacity(payloads.len());
    for (i, p) in payloads.iter().enumerate() {
        let mut d = Vec::with_capacity(wire::VIDEO_CHUNK_HEADER_SIZE + p.len());
        wire::append_video_chunk(
            &mut d,
            &wire::VideoChunkHeader {
                keyframe: false,
                frame_id,
                chunk_index: i as u16,
                chunk_count: count,
                timestamp_us: ts_us,
            },
            p,
        )
        .expect("chunk");
        dgrams.push(d);
    }
    let mut par = Vec::new();
    if parity > 0 && payloads.len() <= wire::MAX_PARITY_DATA_CHUNKS {
        let symbols = wire::compute_parity(&payloads, parity).expect("parity");
        for (i, sym) in symbols.iter().enumerate() {
            let mut d = Vec::with_capacity(wire::PARITY_CHUNK_HEADER_SIZE + sym.len());
            wire::append_parity_chunk(
                &mut d,
                &wire::ParityChunkHeader {
                    frame_id,
                    parity_index: i as u8,
                    chunk_count: count,
                    frame_bytes: data.len() as u32,
                },
                sym,
            )
            .expect("parity chunk");
            par.push(d);
        }
    }
    (dgrams, par)
}

#[tokio::main(flavor = "multi_thread", worker_threads = 4)]
async fn main() {
    let a = parse_args();
    let trace = load_trace(&a.trace);
    let log: Log = Arc::new(Mutex::new(BufWriter::new(File::create(&a.out).unwrap())));
    let (_endpoint, conn) = connect(&a).await;
    eprintln!("connected, mode={} cc={}", a.mode, a.cc);

    // Against a real relay's /publish: write the minted broadcast ID where
    // gawk-loadgen can find it.
    if !a.id_file.is_empty() {
        let c = conn.clone();
        let path = a.id_file.clone();
        tokio::spawn(async move {
            while let Ok(opening) = c.accept_uni().await {
                let mut st = opening;
                let mut msg = Vec::new();
                let mut buf = [0u8; 4096];
                while let Ok(Some(n)) = st.read(&mut buf).await {
                    msg.extend_from_slice(&buf[..n]);
                }
                if let Ok(id) = wire::parse_broadcast_announce(&msg) {
                    let _ = std::fs::write(&path, id);
                }
            }
        });
    }

    let slot = Arc::new(KeyframeSlot {
        pending: Mutex::new(None),
        notify: Notify::new(),
    });
    // The engine sets no stream priority; the carrier puts keyframes first.
    let kf_priority = if a.mode == "carrier" { 1 } else { 0 };
    tokio::spawn(keyframe_task(
        conn.clone(),
        slot.clone(),
        log.clone(),
        kf_priority,
    ));

    let (latest_tx, latest_rx) = watch::channel(0u32);
    let carrier_tx = if a.mode == "carrier" {
        let (tx, rx) = mpsc::unbounded_channel();
        tokio::spawn(carrier_task(conn.clone(), rx, log.clone(), latest_rx));
        Some(tx)
    } else {
        None
    };

    {
        let c = conn.clone();
        tokio::spawn(async move {
            // A 20 ms Opus frame at ~64 kbps: the audio lane's datagram.
            let mut pkt = vec![0u8; 180];
            pkt[0] = wire::VERSION;
            pkt[1] = wire::TYPE_AUDIO_FRAME;
            let mut iv = tokio::time::interval(Duration::from_millis(20));
            loop {
                iv.tick().await;
                if c.send_datagram(&pkt).is_err() {
                    return;
                }
            }
        });
    }

    // Path stats once a second.
    {
        let c = conn.clone();
        let log = log.clone();
        tokio::spawn(async move {
            let mut iv = tokio::time::interval(Duration::from_secs(1));
            loop {
                iv.tick().await;
                let q = c.quic_connection();
                let s = q.stats();
                log_line(
                    &log,
                    format!(
                        "T,{},{},{},{},{},{},{},{}",
                        now_ns(),
                        s.path.rtt.as_micros(),
                        s.path.cwnd,
                        s.path.sent_packets,
                        s.path.lost_packets,
                        s.path.lost_bytes,
                        s.path.congestion_events,
                        s.frame_tx.datagram
                    ),
                );
            }
        });
    }

    let interval = Duration::from_secs_f64(1.0 / a.fps);
    let n = (a.seconds * a.fps) as usize;
    let payload = vec![0x5Au8; trace.iter().map(|t| t.0).max().unwrap()];
    let start = tokio::time::Instant::now() + Duration::from_millis(100);
    let mut datagrams_offered: u64 = 0;
    // The engine's chunk budget (sender.rs): shrunk once when quinn says a
    // datagram no longer fits — its MTU black-hole detector can drop the
    // path MTU to 1200 under loss — and kept for every later frame.
    let mut budget = wire::ChunkBudget::new();
    for i in 0..n {
        tokio::time::sleep_until(start + interval.mul_f64(i as f64)).await;
        let (size, key) = trace[i % trace.len()];
        let frame_id = i as u32;
        let ts_us = (i as f64 * 1e6 / a.fps) as u64;
        let data = &payload[..size];
        let t = now_ns();
        if key {
            let mut msg = Vec::with_capacity(wire::STREAM_FRAME_HEADER_SIZE + size);
            wire::append_stream_frame_header(
                &mut msg,
                &wire::StreamFrameHeader {
                    keyframe: true,
                    frame_id,
                    timestamp_us: ts_us,
                    config_len: 0,
                    payload_len: size as u32,
                },
            )
            .unwrap();
            msg.extend_from_slice(data);
            let superseded = slot
                .pending
                .lock()
                .unwrap()
                .replace((frame_id, msg))
                .is_some();
            slot.notify.notify_one();
            if let Some(tx) = &carrier_tx {
                latest_tx.send_replace(frame_id);
                let _ = tx.send(CarrierMsg::Rotate(frame_id));
            }
            log_line(
                &log,
                format!("F,{frame_id},1,{size},0,0,{t},{}", superseded as u8),
            );
            continue;
        }
        let (mut dgrams, mut par) = chunk_frame(frame_id, data, ts_us, a.parity, &budget);
        match &carrier_tx {
            None => {
                // sender.rs's send_delta: on TooLarge, shrink and re-chunk the
                // frame once; any other failure drops the rest of the frame.
                'attempt: for _ in 0..2 {
                    for d in dgrams.iter() {
                        datagrams_offered += 1;
                        match conn.send_datagram(d) {
                            Ok(()) => {}
                            Err(wtransport::error::SendDatagramError::TooLarge) => {
                                let max = conn.max_datagram_size().unwrap_or(0);
                                if budget.shrink_for_datagram_size(max) {
                                    log_line(&log, format!("M,{frame_id},{max},{}", now_ns()));
                                    (dgrams, par) =
                                        chunk_frame(frame_id, data, ts_us, a.parity, &budget);
                                    continue 'attempt;
                                }
                                log_line(&log, format!("E,{frame_id},toolarge"));
                                break 'attempt;
                            }
                            Err(_) => {
                                log_line(&log, format!("E,{frame_id},failed"));
                                break 'attempt;
                            }
                        }
                    }
                    for d in par.iter() {
                        datagrams_offered += 1;
                        if conn.send_datagram(d).is_err() {
                            break;
                        }
                    }
                    break;
                }
            }
            Some(tx) => {
                let mut buf = Vec::new();
                for d in dgrams.iter().chain(par.iter()) {
                    buf.extend_from_slice(&(d.len() as u16).to_be_bytes());
                    buf.extend_from_slice(d);
                }
                let _ = tx.send(CarrierMsg::Records(frame_id, buf));
            }
        }
        log_line(
            &log,
            format!("F,{frame_id},0,{size},{},{},{t},0", dgrams.len(), par.len()),
        );
    }
    // Let the tail drain, then record the final path state.
    tokio::time::sleep(Duration::from_millis(1500)).await;
    let s = conn.quic_connection().stats();
    log_line(
        &log,
        format!(
            "S,{},{},{},{},{},{},{},{},{}",
            s.path.rtt.as_micros(),
            s.path.cwnd,
            s.path.sent_packets,
            s.path.lost_packets,
            s.path.lost_bytes,
            s.path.congestion_events,
            s.frame_tx.datagram,
            datagrams_offered,
            s.udp_tx.bytes
        ),
    );
    log.lock().unwrap().flush().unwrap();
    conn.close(VarInt::from_u32(0), b"done");
    tokio::time::sleep(Duration::from_millis(200)).await;
}
