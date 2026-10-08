//! D24's viewer integration test (docs/67, IO4): the viewer core subscribed
//! to a `gawk-pubsim` broadcast on the real relay, through induced datagram
//! loss on the relay→viewer leg and a rolling relay restart.
//!
//! Ignored by default like the engine's relay suite: it builds and runs
//! gawk-server and gawk-pubsim (Go). Run with
//! `cargo test -p gawk-viewer --test relay_viewer -- --ignored`; CI sets
//! `GAWK_IT_RELAY_BIN_DIR` and `GAWK_IT_PUBSIM_BIN` to prebuilt binaries.

#[path = "../../../../../gawk-broadcast-desktop/crates/engine/tests/support/relay.rs"]
mod relay;

use gawk_viewer::pipeline::{PipelineStats, ViewerEvent};
use gawk_viewer::playout::PlayoutPreset;
use gawk_viewer::session::{
    EndReason, ViewerClock, ViewerConfig, ViewerSink, ViewerState, WtSubscribeDialer, run,
};
use relay::{Relay, SECRET, repo_root};
use std::collections::HashMap;
use std::io::{BufRead, BufReader};
use std::net::SocketAddr;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};
use tokio::net::UdpSocket;

/// The publisher simulator: the committed H.264 + Opus fixture, looping.
struct Pubsim {
    child: Child,
    id: String,
}

impl Drop for Pubsim {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn pubsim_bin() -> PathBuf {
    if let Ok(bin) = std::env::var("GAWK_IT_PUBSIM_BIN") {
        return PathBuf::from(bin);
    }
    let out = std::env::temp_dir().join(format!("gawk-pubsim-{}", std::process::id()));
    let status = Command::new("go")
        .args(["build", "-o"])
        .arg(&out)
        .arg("./cmd/gawk-pubsim")
        .current_dir(repo_root().join("gawk-server"))
        // As in the relay harness: an iOS deployment target must not reach cgo.
        .env_remove("IPHONEOS_DEPLOYMENT_TARGET")
        .status()
        .expect("go toolchain available");
    assert!(status.success(), "go build ./cmd/gawk-pubsim failed");
    out
}

fn start_pubsim(relay_url: &str) -> Pubsim {
    let mut child = Command::new(pubsim_bin())
        .args(["-url", relay_url, "-insecure", "-secret", SECRET, "-audio"])
        .args(["-stats", "0"])
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .expect("spawn gawk-pubsim");
    let stdout = child.stdout.take().unwrap();
    let mut lines = BufReader::new(stdout).lines();
    let id = loop {
        let line = lines
            .next()
            .expect("gawk-pubsim printed its ID before exiting")
            .unwrap();
        if let Some(id) = line.strip_prefix("GAWK_PUBSIM_ID=") {
            break id.to_owned();
        }
    };
    // Keep draining stdout so the publisher never blocks on a full pipe.
    std::thread::spawn(move || for _ in lines {});
    Pubsim { child, id }
}

/// A userspace UDP forwarder in front of the relay that drops one in
/// `drop_every` packets on the relay→viewer leg while armed (0 = off): the
/// lossy link of gawk-server's resilient_loss_test, in Rust. One upstream
/// socket per viewer address, so a reconnect (a new local port) gets its
/// own path, and a restarted relay on the same port is reached unchanged.
struct LossyLink {
    addr: SocketAddr,
    drop_every: Arc<AtomicU64>,
    dropped: Arc<AtomicU64>,
}

async fn lossy_link(upstream: SocketAddr) -> LossyLink {
    let front = Arc::new(UdpSocket::bind("127.0.0.1:0").await.unwrap());
    let addr = front.local_addr().unwrap();
    let drop_every = Arc::new(AtomicU64::new(0));
    let dropped = Arc::new(AtomicU64::new(0));
    let (de, dr) = (drop_every.clone(), dropped.clone());
    tokio::spawn(async move {
        let mut paths: HashMap<SocketAddr, Arc<UdpSocket>> = HashMap::new();
        let mut buf = vec![0u8; 65_536];
        loop {
            let Ok((n, client)) = front.recv_from(&mut buf).await else {
                return;
            };
            let up = match paths.get(&client) {
                Some(u) => u.clone(),
                None => {
                    let u = Arc::new(UdpSocket::bind("127.0.0.1:0").await.unwrap());
                    u.connect(upstream).await.unwrap();
                    paths.insert(client, u.clone());
                    let (front, u2, de, dr) = (front.clone(), u.clone(), de.clone(), dr.clone());
                    tokio::spawn(async move {
                        let mut buf = vec![0u8; 65_536];
                        let mut seq: u64 = 0;
                        while let Ok(n) = u2.recv(&mut buf).await {
                            seq += 1;
                            let every = de.load(Ordering::Relaxed);
                            if every > 0 && seq.is_multiple_of(every) {
                                dr.fetch_add(1, Ordering::Relaxed);
                                continue;
                            }
                            let _ = front.send_to(&buf[..n], client).await;
                        }
                    });
                    u
                }
            };
            let _ = up.send(&buf[..n]).await;
        }
    });
    LossyLink {
        addr,
        drop_every,
        dropped,
    }
}

#[derive(Default)]
struct Seen {
    video: u64,
    keyframes: u64,
    audio: u64,
    configs: Vec<String>,
    states: Vec<ViewerState>,
    stats: Option<PipelineStats>,
}

#[derive(Default)]
struct Sink(Mutex<Seen>);

impl ViewerSink for Sink {
    fn state(&self, s: ViewerState) {
        self.0.lock().unwrap().states.push(s);
    }
    fn event(&self, e: ViewerEvent) {
        let mut seen = self.0.lock().unwrap();
        match e {
            ViewerEvent::VideoFrame { keyframe, .. } => {
                seen.video += 1;
                seen.keyframes += u64::from(keyframe);
            }
            ViewerEvent::Audio { .. } => seen.audio += 1,
            ViewerEvent::VideoConfig(c) => seen.configs.push(c.codec),
            ViewerEvent::Stats(s) => seen.stats = Some(s),
            _ => {}
        }
    }
}

impl Sink {
    fn video(&self) -> u64 {
        self.0.lock().unwrap().video
    }

    /// Waits until `cond` holds, failing with the viewer's state if not.
    async fn wait_for(&self, what: &str, within: Duration, cond: impl Fn(&Seen) -> bool) {
        let deadline = Instant::now() + within;
        loop {
            if cond(&self.0.lock().unwrap()) {
                return;
            }
            if Instant::now() > deadline {
                let seen = self.0.lock().unwrap();
                panic!(
                    "timed out waiting for {what}: video {} audio {} states {:?} stats {:?}",
                    seen.video, seen.audio, seen.states, seen.stats
                );
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "builds and runs the Go relay and publisher"]
async fn the_viewer_plays_through_datagram_loss_and_a_relay_restart() {
    let relay = Relay::start(&["-publish-secret", SECRET]);
    let pubsim = start_pubsim(&relay.url);
    let relay_addr: SocketAddr = relay.url.trim_start_matches("https://").parse().unwrap();
    let link = lossy_link(relay_addr).await;

    let sink = Arc::new(Sink::default());
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    let cfg = ViewerConfig {
        relay_url: format!("https://{}", link.addr),
        broadcast_id: pubsim.id.clone(),
        preset: PlayoutPreset::Balanced,
    };
    let dialer = Arc::new(WtSubscribeDialer {
        origin: "gawk://ios".into(),
        insecure: true,
    });
    let viewer = tokio::spawn(run(cfg, dialer, sink.clone(), ViewerClock::new(), rx));

    // Clean link: the join prime and a couple of GOPs, with audio.
    sink.wait_for("first frames", Duration::from_secs(15), |s| {
        s.video >= 30 && s.audio >= 20 && s.keyframes >= 1
    })
    .await;
    assert!(
        sink.0.lock().unwrap().configs[0].starts_with("avc1"),
        "the fixture is H.264"
    );

    // Lossy link: one relay→viewer packet in 12 dropped. Parity (the relay's
    // default, 2 symbols) repairs what it can and the strict delta-loss rule
    // freezes until the next keyframe on the rest; playback must go on.
    link.drop_every.store(12, Ordering::Relaxed);
    let before = sink.video();
    tokio::time::sleep(Duration::from_secs(6)).await;
    link.drop_every.store(0, Ordering::Relaxed);
    let dropped = link.dropped.load(Ordering::Relaxed);
    assert!(
        dropped > 50,
        "the link actually dropped packets ({dropped})"
    );
    let during = sink.video() - before;
    assert!(
        during >= 60,
        "frames kept playing through the loss ({during} in 6 s)"
    );
    sink.wait_for("a parity repair", Duration::from_secs(5), |s| {
        s.stats
            .is_some_and(|st| st.reassembly.frames_recovered_by_parity > 0)
    })
    .await;

    // A rolling relay restart: SIGTERM drains every session with 4002, and
    // the same relay comes back on the same port. The publisher reclaims its
    // broadcast; the viewer reconnects (a 404 until then is part of the
    // ladder) and plays again on the same ID.
    let mut relay = relay;
    relay.drain_and_stop();
    let _relay = relay.restart();
    let before = sink.video();
    sink.wait_for("a reconnect", Duration::from_secs(30), |s| {
        s.states
            .iter()
            .any(|st| matches!(st, ViewerState::Reconnecting { .. }))
    })
    .await;
    sink.wait_for("frames after the restart", Duration::from_secs(60), |s| {
        s.video >= before + 30
    })
    .await;
    {
        let seen = sink.0.lock().unwrap();
        assert!(
            !seen
                .states
                .iter()
                .any(|st| matches!(st, ViewerState::Ended(_))),
            "the viewer never ended: {:?}",
            seen.states
        );
    }

    tx.send(gawk_viewer::session::Command::Stop).unwrap();
    assert_eq!(viewer.await.unwrap(), EndReason::Stopped);
    drop(pubsim);
}
