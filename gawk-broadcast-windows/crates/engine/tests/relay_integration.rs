//! Integration against the REAL gawk-server binary (docs/38 D18): "a
//! hand-written fake would only assert the engine against our belief about
//! the relay, which is the belief most worth doubting in a second
//! implementation of a protocol" — and this is the third.
//!
//! Ignored by default (they build and run the Go relay); CI and developers
//! run them with `cargo test -p gawk-engine --test relay_integration --
//! --ignored`. The harness mirrors the Go engine's
//! relay_integration_test.go: build gawk-server + gawk-devcert, self-signed
//! certs, free ports, poll /healthz.

use gawk_engine::clock::MonotonicClock;
use gawk_engine::media::AccessUnit;
use gawk_engine::relay::StartPhase;
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use std::io::{Read, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::mpsc::UnboundedReceiver;

const SECRET: &str = "it-s3cret";

fn server_dir() -> PathBuf {
    // crates/engine → gawk-broadcast-windows → repo root → gawk-server.
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../../gawk-server")
        .canonicalize()
        .unwrap()
}

fn build_tool(name: &str, out: &PathBuf) {
    let status = Command::new("go")
        .args(["build", "-o"])
        .arg(out)
        .arg(format!("./cmd/{name}"))
        .current_dir(server_dir())
        .status()
        .expect("go toolchain available");
    assert!(status.success(), "go build ./cmd/{name} failed");
}

fn free_udp_port() -> u16 {
    std::net::UdpSocket::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn free_tcp_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

/// Minimal HTTP/1.1 GET over std TcpStream — keeps reqwest out of the tree.
fn http_get(addr: &str, path: &str) -> std::io::Result<String> {
    let mut stream = std::net::TcpStream::connect(addr)?;
    stream.set_read_timeout(Some(Duration::from_secs(2)))?;
    write!(
        stream,
        "GET {path} HTTP/1.1\r\nHost: {addr}\r\nConnection: close\r\n\r\n"
    )?;
    let mut out = String::new();
    stream.read_to_string(&mut out)?;
    Ok(out)
}

// The restart machinery (fields beyond child/url/ops_port) only runs on
// unix — the drain test is cfg(unix) — so Windows sees it as dead code.
#[cfg_attr(not(unix), allow(dead_code))]
struct Relay {
    child: Child,
    url: String,
    udp_port: u16,
    ops_port: u16,
    bin: PathBuf,
    cert_dir: PathBuf,
    extra_args: Vec<String>,
}

static TEST_SEQ: std::sync::atomic::AtomicU32 = std::sync::atomic::AtomicU32::new(0);

impl Relay {
    /// Builds (cached by Go's build cache) and starts the real relay. Each
    /// test gets its own temp dir so parallel `go build -o` calls don't race.
    fn start(extra_args: &[&str]) -> Relay {
        let seq = TEST_SEQ.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let tmp = std::env::temp_dir().join(format!("gawk-it-{}-{seq}", std::process::id()));
        std::fs::create_dir_all(&tmp).unwrap();
        let bin = tmp.join("gawk-server");
        let devcert = tmp.join("gawk-devcert");
        build_tool("gawk-server", &bin);
        build_tool("gawk-devcert", &devcert);
        let cert_dir = tmp.join("cert");
        if !cert_dir.join("cert.pem").exists() {
            let out = Command::new(&devcert)
                .arg("-out")
                .arg(&cert_dir)
                .output()
                .unwrap();
            assert!(out.status.success(), "gawk-devcert: {:?}", out);
        }
        let extra: Vec<String> = extra_args.iter().map(|s| s.to_string()).collect();
        Self::spawn(bin, cert_dir, free_udp_port(), free_tcp_port(), extra)
    }

    fn spawn(
        bin: PathBuf,
        cert_dir: PathBuf,
        udp_port: u16,
        ops_port: u16,
        extra_args: Vec<String>,
    ) -> Relay {
        let mut cmd = Command::new(&bin);
        cmd.args(["-addr", &format!("127.0.0.1:{udp_port}")])
            .args(["-metrics-addr", &format!("127.0.0.1:{ops_port}")])
            .args(["-cert-file", cert_dir.join("cert.pem").to_str().unwrap()])
            .args(["-key-file", cert_dir.join("key.pem").to_str().unwrap()])
            .args(&extra_args)
            .stdout(Stdio::null())
            .stderr(Stdio::inherit());
        let child = cmd.spawn().expect("spawn gawk-server");
        let relay = Relay {
            child,
            url: format!("https://127.0.0.1:{udp_port}"),
            udp_port,
            ops_port,
            bin,
            cert_dir,
            extra_args,
        };
        relay.wait_healthy();
        relay
    }

    fn wait_healthy(&self) {
        let ops = format!("127.0.0.1:{}", self.ops_port);
        let deadline = Instant::now() + Duration::from_secs(15);
        while Instant::now() < deadline {
            if let Ok(resp) = http_get(&ops, "/healthz")
                && resp.starts_with("HTTP/1.1 200")
            {
                return;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        panic!("relay did not become healthy");
    }

    /// Graceful drain: SIGTERM makes the relay close every session with
    /// 4002 (server draining) while still Ready — the planned-rollout blip
    /// auto-resume exists for. Unix-only: Windows has no SIGTERM analogue a
    /// console Go process handles, so the restart test is cfg(unix) and its
    /// Windows-side behavior stays on the on-hardware register (docs/38 §10).
    #[cfg(unix)]
    fn drain_and_stop(&mut self) {
        unsafe {
            libc_kill(self.child.id() as i32, 15);
        }
        let _ = self.child.wait();
    }

    /// Restarts the SAME relay identity (same port, same flags) — a rolling
    /// restart as the client sees it. Only the cfg(unix) drain test uses it.
    #[cfg(unix)]
    fn restart(self) -> Relay {
        Relay::spawn(
            self.bin.clone(),
            self.cert_dir.clone(),
            self.udp_port,
            free_tcp_port(),
            self.extra_args.clone(),
        )
    }
}

impl Drop for Relay {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

#[cfg(unix)]
unsafe extern "C" {
    #[link_name = "kill"]
    fn libc_kill(pid: i32, sig: i32) -> i32;
}

fn config(relay: &Relay, id: &str, token_hex: &str) -> SessionConfig {
    SessionConfig {
        relay_url: relay.url.clone(),
        broadcast_id: id.into(),
        resume_token_hex: token_hex.into(),
        publish_secret: SECRET.into(),
        origin: gawk_engine::defaults::ORIGIN.into(),
        insecure: true,
    }
}

async fn next_event(rx: &mut UnboundedReceiver<EngineEvent>, what: &str, secs: u64) -> EngineEvent {
    tokio::time::timeout(Duration::from_secs(secs), rx.recv())
        .await
        .unwrap_or_else(|_| panic!("timed out waiting for {what}"))
        .unwrap_or_else(|| panic!("event channel closed waiting for {what}"))
}

/// Collects the session identity (announce + resume token), which arrive on
/// separate streams in NO guaranteed order.
async fn collect_identity(rx: &mut UnboundedReceiver<EngineEvent>) -> (String, String) {
    let (mut id, mut token) = (None, None);
    while id.is_none() || token.is_none() {
        match next_event(rx, "announce/token", 10).await {
            EngineEvent::Announce { broadcast_id } => id = Some(broadcast_id),
            EngineEvent::ResumeToken { token_hex } => token = Some(token_hex),
            _ => {}
        }
    }
    (id.unwrap(), token.unwrap())
}

fn keyframe(n: usize, ts: u64) -> AccessUnit {
    AccessUnit {
        data: vec![0xBB; n],
        timestamp_us: ts,
        keyframe: true,
    }
}

fn delta(n: usize, ts: u64) -> AccessUnit {
    AccessUnit {
        data: vec![0xAA; n],
        timestamp_us: ts,
        keyframe: false,
    }
}

// The whole publisher protocol against the real relay: dial with the Origin
// header, announce + token + capabilities by type, keyframes on reliable
// streams, deltas on datagrams, clean stop.
#[tokio::test]
#[ignore = "builds and runs the real gawk-server (cargo test -- --ignored)"]
async fn publishes_to_the_real_relay() {
    let relay = Relay::start(&["-publish-secret", SECRET]);
    let (session, mut rx) = Session::start(config(&relay, "", ""), Arc::new(MonotonicClock::new()))
        .await
        .unwrap();

    let (id, token) = collect_identity(&mut rx).await;
    assert_eq!(id.len(), 6, "announced code: {id}");
    assert!(
        id.chars()
            .all(|c| gawk_wire::BROADCAST_ID_ALPHABET.contains(c))
    );
    assert_eq!(token.len(), 32, "16-byte token, hex-encoded: {token}");

    let sender = session.sender();
    sender.set_codec("avc1.42E02A");
    sender.send_video(keyframe(50_000, 1_000)).await;
    for i in 0..10u64 {
        sender.send_video(delta(5_000, 2_000 + i)).await;
    }
    // Give the reliable keyframe stream a moment to complete.
    tokio::time::sleep(Duration::from_millis(300)).await;

    let st = session.stats();
    assert!(
        st.sent_frames >= 10,
        "frames should reach the relay: {st:?}"
    );
    assert!(
        st.keyframe_streams_sent >= 1,
        "the keyframe rides a reliable stream: {st:?}"
    );
    // R29: the relay advertises parity (fleet default 2), so deltas carried
    // trailing parity symbols — capabilities arrived and were applied.
    assert!(
        st.parity_level > 0,
        "capabilities should have arrived: {st:?}"
    );
    assert!(st.parity_chunks_sent > 0, "{st:?}");

    session.stop().await;
    let ended = next_event(&mut rx, "ended", 5).await;
    assert_eq!(ended, EngineEvent::Ended { error: None });
}

// Gate 2a (docs/38 D2), proven end to end through the vendored patch: the
// HTTP status of a refused CONNECT reaches StartError. These are the
// relay's real statuses, not a fake's.
#[tokio::test]
#[ignore = "builds and runs the real gawk-server (cargo test -- --ignored)"]
async fn rejection_statuses_surface_through_the_patched_transport() {
    let relay = Relay::start(&["-publish-secret", SECRET]);

    // Wrong secret → 401.
    let mut cfg = config(&relay, "", "");
    cfg.publish_secret = "wrong".into();
    let err = Session::start(cfg, Arc::new(MonotonicClock::new()))
        .await
        .unwrap_err();
    assert_eq!((err.phase, err.status), (StartPhase::Connect, 401), "{err}");

    // A well-formed but unknown ID with a fabricated token → 403 (the token
    // cannot verify).
    let cfg = config(&relay, "ABCDEF", &"00".repeat(16));
    let err = Session::start(cfg, Arc::new(MonotonicClock::new()))
        .await
        .unwrap_err();
    assert_eq!((err.phase, err.status), (StartPhase::Connect, 403), "{err}");

    // An invalid ID → 404.
    let cfg = config(&relay, "IL0", &"00".repeat(16));
    let err = Session::start(cfg, Arc::new(MonotonicClock::new()))
        .await
        .unwrap_err();
    assert_eq!((err.phase, err.status), (StartPhase::Connect, 404), "{err}");
}

// G9: a rolling relay restart. The drain's 4002 is read from the
// CLOSE_WEBTRANSPORT_SESSION capsule (gate 2b), classified non-terminal,
// and the session reclaims its code on the restarted relay with the same
// frameId space.
#[cfg(unix)]
#[tokio::test]
#[ignore = "builds and runs the real gawk-server (cargo test -- --ignored)"]
async fn resume_survives_a_relay_restart() {
    let mut relay = Relay::start(&["-publish-secret", SECRET]);
    let (session, mut rx) = Session::start(config(&relay, "", ""), Arc::new(MonotonicClock::new()))
        .await
        .unwrap();
    let (id, _token) = collect_identity(&mut rx).await;

    let sender = session.sender();
    sender.set_codec("avc1.42E02A");
    sender.send_video(keyframe(10_000, 1_000)).await;
    sender.send_video(delta(5_000, 2_000)).await;
    let frames_before = session.stats().encoded_frames;

    // Rolling restart: drain (4002 to every session), then the replacement
    // comes up on the same address. The resume key is derived from the
    // publish secret, so the restarted relay verifies the old token.
    relay.drain_and_stop();
    let relay = relay.restart();

    // The session must come back on its own — resuming, then resumed.
    let mut resumed = false;
    for _ in 0..20 {
        match next_event(&mut rx, "resuming/resumed", 30).await {
            EngineEvent::Resumed => {
                resumed = true;
                break;
            }
            EngineEvent::Resuming { .. } => continue,
            EngineEvent::Ended { error } => panic!("session ended instead of resuming: {error:?}"),
            _ => continue,
        }
    }
    assert!(resumed);
    assert_eq!(
        session.broadcast_id(),
        id,
        "the SAME code, reclaimed — never a mint"
    );

    // The frameId space carried over (continuity is the resume signal) and
    // frames flow to the restarted relay.
    sender.send_video(delta(5_000, 3_000)).await;
    let st = session.stats();
    assert_eq!(st.encoded_frames, frames_before + 1);
    assert!(st.resumes >= 1);

    session.stop().await;
    drop(relay);
}

// Gate 2b + the relay invariant: a newer publisher with a verified token
// deposes the incumbent with 4004, and the deposed session must NOT
// auto-resume back (two auto-resuming engines would depose each other
// forever).
#[tokio::test]
#[ignore = "builds and runs the real gawk-server (cargo test -- --ignored)"]
async fn a_superseded_publisher_stays_down() {
    let relay = Relay::start(&["-publish-secret", SECRET]);
    let (session_a, mut rx_a) =
        Session::start(config(&relay, "", ""), Arc::new(MonotonicClock::new()))
            .await
            .unwrap();
    let (id, token) = collect_identity(&mut rx_a).await;

    // A newer session claims the same code with the verified token.
    let (session_b, rx_b) =
        Session::start(config(&relay, &id, &token), Arc::new(MonotonicClock::new()))
            .await
            .unwrap();

    // The incumbent is deposed with 4004 — terminal for resume, surfaced
    // with the curated sentence, and no Resuming/Resumed ever follows.
    loop {
        match next_event(&mut rx_a, "the 4004 ending", 15).await {
            EngineEvent::Ended { error } => {
                let msg = error.expect("superseded is an error ending");
                assert!(msg.contains("superseded"), "{msg}");
                break;
            }
            EngineEvent::Resumed => panic!("a deposed publisher must never resume back"),
            _ => continue,
        }
    }

    session_b.stop().await;
    let _ = rx_b;
    session_a.stop().await;
}
