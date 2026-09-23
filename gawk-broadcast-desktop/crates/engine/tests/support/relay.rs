//! The real-relay harness (docs/38 D18): build gawk-server + gawk-devcert,
//! self-signed certs, free ports, poll /healthz. Shared by the engine's
//! integration suite and the macOS encode-to-relay test (R52 MB3), which
//! include it with `#[path]` — a test-only file, so no crate for it.
#![allow(dead_code)]

use gawk_engine::media::AccessUnit;
use gawk_engine::session::{EngineEvent, SessionConfig};
use std::io::{Read, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};
use tokio::sync::mpsc::UnboundedReceiver;

pub const SECRET: &str = "it-s3cret";

pub fn server_dir() -> PathBuf {
    // crates/<crate> → gawk-broadcast-desktop → repo root → gawk-server.
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../../gawk-server")
        .canonicalize()
        .unwrap()
}

pub fn build_tool(name: &str, out: &PathBuf) {
    let status = Command::new("go")
        .args(["build", "-o"])
        .arg(out)
        .arg(format!("./cmd/{name}"))
        .current_dir(server_dir())
        .status()
        .expect("go toolchain available");
    assert!(status.success(), "go build ./cmd/{name} failed");
}

pub fn free_udp_port() -> u16 {
    std::net::UdpSocket::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

pub fn free_tcp_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

/// Minimal HTTP/1.1 GET over std TcpStream — keeps reqwest out of the tree.
pub fn http_get(addr: &str, path: &str) -> std::io::Result<String> {
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
pub struct Relay {
    child: Child,
    pub url: String,
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
    pub fn start(extra_args: &[&str]) -> Relay {
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
    pub fn drain_and_stop(&mut self) {
        unsafe {
            libc_kill(self.child.id() as i32, 15);
        }
        let _ = self.child.wait();
    }

    /// Restarts the SAME relay identity (same port, same flags) — a rolling
    /// restart as the client sees it. Only the cfg(unix) drain test uses it.
    #[cfg(unix)]
    pub fn restart(self) -> Relay {
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

pub fn config(relay: &Relay, id: &str, token_hex: &str) -> SessionConfig {
    SessionConfig {
        relay_url: relay.url.clone(),
        broadcast_id: id.into(),
        resume_token_hex: token_hex.into(),
        publish_secret: SECRET.into(),
        origin: gawk_engine::defaults::ORIGIN.into(),
        insecure: true,
        ..SessionConfig::default()
    }
}

pub async fn next_event(
    rx: &mut UnboundedReceiver<EngineEvent>,
    what: &str,
    secs: u64,
) -> EngineEvent {
    tokio::time::timeout(Duration::from_secs(secs), rx.recv())
        .await
        .unwrap_or_else(|_| panic!("timed out waiting for {what}"))
        .unwrap_or_else(|| panic!("event channel closed waiting for {what}"))
}

/// Collects the session identity (announce + resume token), which arrive on
/// separate streams in NO guaranteed order.
pub async fn collect_identity(rx: &mut UnboundedReceiver<EngineEvent>) -> (String, String) {
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

pub fn keyframe(n: usize, ts: u64) -> AccessUnit {
    AccessUnit {
        data: vec![0xBB; n],
        timestamp_us: ts,
        keyframe: true,
    }
}

pub fn delta(n: usize, ts: u64) -> AccessUnit {
    AccessUnit {
        data: vec![0xAA; n],
        timestamp_us: ts,
        keyframe: false,
    }
}
