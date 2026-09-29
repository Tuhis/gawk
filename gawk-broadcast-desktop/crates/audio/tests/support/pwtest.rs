//! A private, headless PipeWire for integration tests (docs/58 D12), ported
//! from the Go `pwtest` with its three environment lessons (docs/39 F6):
//!
//! * WirePlumber needs a session bus — a private `dbus-daemon` is started;
//! * an emitter started before WirePlumber adopted a default sink dies with
//!   "no target node available" — the sink AND the default are awaited;
//! * the stock WirePlumber config cannot run in a container (its Bluetooth
//!   half loads logind and takes the session manager down) — a config with
//!   only the linking half is written.
//!
//! The harness SKIPS when the environment cannot host a sound server —
//! unless `GAWK_REQUIRE_PIPEWIRE=1`, which CI sets, so a skip can never be a
//! silent pass (docs/39 F11).

#![allow(dead_code)]

use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::time::{Duration, Instant};

pub struct Daemon {
    pub dir: PathBuf,
    pub env: Vec<(String, String)>,
    procs: Mutex<Vec<Child>>,
    dbus_pid: String,
}

impl Drop for Daemon {
    fn drop(&mut self) {
        for mut p in self.procs.lock().unwrap().drain(..).rev() {
            let _ = p.kill();
            let _ = p.wait();
        }
        let _ = Command::new("kill").arg(&self.dbus_pid).status();
        let _ = std::fs::remove_dir_all(&self.dir);
    }
}

fn skip(why: &str) -> Option<Daemon> {
    assert!(
        std::env::var("GAWK_REQUIRE_PIPEWIRE").is_err(),
        "GAWK_REQUIRE_PIPEWIRE is set, but: {why}"
    );
    eprintln!("SKIP: {why}");
    None
}

fn which(bin: &str) -> bool {
    Command::new("sh")
        .args(["-c", &format!("command -v {bin}")])
        .stdout(Stdio::null())
        .status()
        .is_ok_and(|s| s.success())
}

/// Every process the harness started, killed at exit: the shared daemon is
/// a static, and statics are never dropped.
static PIDS: Mutex<Vec<String>> = Mutex::new(Vec::new());

extern "C" fn kill_all() {
    if let Ok(pids) = PIDS.lock() {
        for pid in pids.iter() {
            let _ = Command::new("kill").args(["-9", pid]).status();
        }
    }
}

/// One daemon per test binary, shared, and a lock every test holds: the
/// control plane names its sink by pid, so two alive at once in one process
/// would find each other's sink.
pub fn daemon() -> Option<(&'static Daemon, MutexGuard<'static, ()>)> {
    static D: OnceLock<Option<Daemon>> = OnceLock::new();
    static LOCK: Mutex<()> = Mutex::new(());
    let guard = LOCK.lock().unwrap_or_else(|e| e.into_inner());
    let d = D
        .get_or_init(|| {
            // SAFETY: registering a plain extern "C" fn with no captures.
            unsafe { libc::atexit(kill_all) };
            start("FL,FR")
        })
        .as_ref()?;
    Some((d, guard))
}

fn start(speakers: &str) -> Option<Daemon> {
    for bin in [
        "pipewire",
        "wireplumber",
        "pw-cli",
        "pw-dump",
        "pw-record",
        "pw-metadata",
        "dbus-daemon",
        "gst-launch-1.0",
    ] {
        if !which(bin) {
            return skip(&format!("{bin} is not installed"));
        }
    }
    let dir = std::env::temp_dir().join(format!("pwt-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).ok()?;
    std::fs::set_permissions(&dir, std::os::unix::fs::PermissionsExt::from_mode(0o700)).ok()?;

    let out = Command::new("dbus-daemon")
        .args(["--session", "--print-address=1", "--print-pid=1", "--fork"])
        .output()
        .ok()?;
    let text = String::from_utf8_lossy(&out.stdout).to_string();
    let mut words = text.split_whitespace();
    let (Some(addr), Some(pid)) = (words.next(), words.next()) else {
        return skip(&format!("cannot start a session bus: {text}"));
    };
    PIDS.lock().unwrap().push(pid.to_string());
    let d = Daemon {
        env: vec![
            ("XDG_RUNTIME_DIR".into(), dir.display().to_string()),
            ("DBUS_SESSION_BUS_ADDRESS".into(), addr.into()),
            ("PIPEWIRE_RUNTIME_DIR".into(), dir.display().to_string()),
            ("PIPEWIRE_DEBUG".into(), "0".into()),
        ],
        dir,
        procs: Mutex::new(Vec::new()),
        dbus_pid: pid.into(),
    };
    // The in-process control plane under test reads these too.
    for (k, v) in &d.env {
        // SAFETY: set once, under the harness's OnceLock, before any test
        // in this binary connects to PipeWire.
        unsafe { std::env::set_var(k, v) };
    }
    d.spawn("pipewire", &[]);
    if !wait_until(Duration::from_secs(10), || {
        d.dir.join("pipewire-0").exists()
            && d.cmd("pw-cli", &["info", "0"])
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .status()
                .is_ok_and(|s| s.success())
    }) {
        return skip("the PipeWire daemon never came up");
    }
    let mut env_extra = Vec::new();
    if let Some(cfg) = minimal_wireplumber(&d.dir) {
        env_extra.push((
            "WIREPLUMBER_CONFIG_DIR".to_string(),
            cfg.display().to_string(),
        ));
    }
    let mut d = d;
    d.env.extend(env_extra);
    d.spawn("wireplumber", &[]);
    d.null_sink("speakers", "Audio/Sink", speakers);
    if !wait_until(Duration::from_secs(10), || {
        d.find_node(|n| n.name == "speakers").is_some()
    }) {
        return skip("the speakers sink never appeared");
    }
    if !wait_until(Duration::from_secs(10), || {
        d.cmd("pw-metadata", &["-n", "default"])
            .output()
            .is_ok_and(|o| String::from_utf8_lossy(&o.stdout).contains("default.audio.sink"))
    }) {
        return skip("wireplumber never published a default sink (no routing here)");
    }
    Some(d)
}

/// The stock config with only the session manager's linking half enabled
/// (docs/39 F6); `None` keeps the stock config on an unexpected layout.
fn minimal_wireplumber(runtime: &Path) -> Option<PathBuf> {
    let stock = Path::new("/usr/share/wireplumber");
    let src = stock.join("main.lua.d");
    let dir = runtime.join("wireplumber");
    let lua = dir.join("main.lua.d");
    std::fs::create_dir_all(&lua).ok()?;
    std::fs::copy(stock.join("main.conf"), dir.join("main.conf")).ok()?;
    let mut saw_functions = false;
    for e in std::fs::read_dir(&src).ok()?.flatten() {
        let name = e.file_name().to_string_lossy().to_string();
        if !name.ends_with(".lua") || name == "90-enable-all.lua" {
            continue;
        }
        saw_functions |= name == "00-functions.lua";
        std::fs::copy(e.path(), lua.join(&name)).ok()?;
    }
    if !saw_functions {
        return None;
    }
    std::fs::write(
        lua.join("90-enable-linking.lua"),
        "-- Written by the gawk test harness: the session manager's linking half only.\n\
         load_module(\"metadata\")\n\
         default_access.enable()\n\
         device_defaults.enable()\n\
         stream_defaults.enable()\n\
         load_script(\"suspend-node.lua\")\n",
    )
    .ok()?;
    std::fs::copy(stock.join("policy.conf"), dir.join("policy.conf")).ok()?;
    let pol = dir.join("policy.lua.d");
    std::fs::create_dir_all(&pol).ok()?;
    for e in std::fs::read_dir(stock.join("policy.lua.d"))
        .ok()?
        .flatten()
    {
        std::fs::copy(e.path(), pol.join(e.file_name())).ok()?;
    }
    let conf = std::fs::read_to_string(stock.join("wireplumber.conf")).ok()?;
    let kept: Vec<&str> = conf
        .lines()
        .filter(|l| !(l.contains("bluetooth.lua") && l.contains("config/lua")))
        .collect();
    if kept.len() == conf.lines().count() {
        return None;
    }
    std::fs::write(dir.join("wireplumber.conf"), kept.join("\n")).ok()?;
    Some(dir)
}

pub fn wait_until(limit: Duration, mut pred: impl FnMut() -> bool) -> bool {
    let t = Instant::now();
    while t.elapsed() < limit {
        if pred() {
            return true;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    false
}

#[track_caller]
pub fn wait_for(what: &str, pred: impl FnMut() -> bool) {
    assert!(
        wait_until(Duration::from_secs(10), pred),
        "timed out waiting for {what}"
    );
}

#[derive(Debug, Clone)]
pub struct Node {
    pub id: u32,
    pub serial: u32,
    pub name: String,
    pub media_class: String,
}

#[derive(Debug, Clone)]
pub struct DumpLink {
    pub out_node: u32,
    pub in_node: u32,
}

impl Daemon {
    pub fn cmd(&self, name: &str, args: &[&str]) -> Command {
        let mut c = Command::new(name);
        c.args(args).envs(self.env.iter().map(|(k, v)| (k, v)));
        c
    }

    fn spawn(&self, name: &str, args: &[&str]) -> u32 {
        let child = self
            .cmd(name, args)
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap_or_else(|e| panic!("starting {name}: {e}"));
        let pid = child.id();
        PIDS.lock().unwrap().push(pid.to_string());
        self.procs.lock().unwrap().push(child);
        pid
    }

    pub fn null_sink(&self, name: &str, class: &str, positions: &str) {
        let spec = format!(
            "{{ factory.name=support.null-audio-sink node.name={name} node.description={name} \
             media.class={class} object.linger=true audio.position=[{positions}] \
             monitor.channel-volumes=true }}"
        );
        self.spawn("pw-cli", &["-m", "create-node", "adapter", &spec]);
    }

    fn dump(&self) -> Vec<serde_json::Value> {
        let out = self.cmd("pw-dump", &[]).output().expect("pw-dump");
        let mut objs: Vec<serde_json::Value> = Vec::new();
        let de =
            serde_json::Deserializer::from_slice(&out.stdout).into_iter::<Vec<serde_json::Value>>();
        for batch in de.flatten() {
            for o in batch {
                let id = o["id"].as_u64();
                objs.retain(|x| x["id"].as_u64() != id);
                objs.push(o);
            }
        }
        objs
    }

    pub fn nodes(&self) -> Vec<Node> {
        self.dump()
            .into_iter()
            .filter(|o| o["type"].as_str().is_some_and(|t| t.ends_with("Node")))
            .map(|o| {
                let p = &o["info"]["props"];
                Node {
                    id: o["id"].as_u64().unwrap_or(0) as u32,
                    serial: p["object.serial"]
                        .as_u64()
                        .or_else(|| p["object.serial"].as_str().and_then(|s| s.parse().ok()))
                        .unwrap_or(0) as u32,
                    name: p["node.name"].as_str().unwrap_or("").into(),
                    media_class: p["media.class"].as_str().unwrap_or("").into(),
                }
            })
            .collect()
    }

    pub fn links(&self) -> Vec<DumpLink> {
        self.dump()
            .into_iter()
            .filter(|o| o["type"].as_str().is_some_and(|t| t.ends_with("Link")))
            .map(|o| DumpLink {
                out_node: o["info"]["output-node-id"].as_u64().unwrap_or(0) as u32,
                in_node: o["info"]["input-node-id"].as_u64().unwrap_or(0) as u32,
            })
            .collect()
    }

    pub fn find_node(&self, pred: impl Fn(&Node) -> bool) -> Option<Node> {
        self.nodes().into_iter().find(|n| pred(n))
    }

    pub fn links_into(&self, node: u32) -> usize {
        self.links().iter().filter(|l| l.in_node == node).count()
    }

    pub fn gawk_objects(&self) -> Vec<Node> {
        self.nodes()
            .into_iter()
            .filter(|n| n.name.starts_with("gawk-app-capture"))
            .collect()
    }

    fn streams_of(&self, binary: &str) -> usize {
        self.nodes()
            .iter()
            .filter(|n| n.media_class == "Stream/Output/Audio" && n.name == binary)
            .count()
    }

    /// A synthetic "game": gst-launch-1.0 copied to a file named `binary`, so
    /// `application.process.binary` is that name, playing a tone to the
    /// default sink.
    pub fn emitter(&self, binary: &str, freq: u32, channels: u32) -> Emitter {
        let fake = self.dir.join(binary);
        if !fake.exists() {
            let src = String::from_utf8(
                Command::new("sh")
                    .args(["-c", "command -v gst-launch-1.0"])
                    .output()
                    .unwrap()
                    .stdout,
            )
            .unwrap();
            std::fs::copy(src.trim(), &fake).unwrap();
        }
        let before = self.streams_of(binary);
        let child = self
            .cmd(
                fake.to_str().unwrap(),
                &[
                    "-q",
                    "audiotestsrc",
                    "is-live=true",
                    &format!("freq={freq}"),
                    "!",
                    "audioconvert",
                    "!",
                    &format!("audio/x-raw,channels={channels}"),
                    "!",
                    "pipewiresink",
                ],
            )
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let e = Emitter { child };
        wait_for(&format!("emitter {binary} in the graph"), || {
            self.streams_of(binary) > before
        });
        e
    }

    /// Peak |sample| of a capture of `serial`'s monitor over `dur`, 0..1.
    pub fn capture_peak(&self, serial: u32, dur: Duration) -> f64 {
        let path = self.dir.join(format!(
            "cap-{serial}-{}.wav",
            Instant::now().elapsed().as_nanos()
        ));
        let mut child = self
            .cmd(
                "pw-record",
                &[
                    "--target",
                    &serial.to_string(),
                    "-P",
                    "{ stream.capture.sink=true }",
                    "--format",
                    "s16",
                    "--rate",
                    "48000",
                    "--channels",
                    "2",
                    path.to_str().unwrap(),
                ],
            )
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        std::thread::sleep(dur);
        let _ = child.kill();
        let _ = child.wait();
        let data = std::fs::read(&path).unwrap_or_default();
        let _ = std::fs::remove_file(&path);
        let body = wav_samples(&data);
        let peak = body
            .chunks_exact(2)
            .map(|b| i16::from_le_bytes([b[0], b[1]]).unsigned_abs())
            .max()
            .unwrap_or(0);
        f64::from(peak) / 32768.0
    }
}

fn wav_samples(b: &[u8]) -> &[u8] {
    if b.len() < 12 || &b[0..4] != b"RIFF" || &b[8..12] != b"WAVE" {
        return b;
    }
    let mut off = 12;
    while off + 8 <= b.len() {
        let size = u32::from_le_bytes([b[off + 4], b[off + 5], b[off + 6], b[off + 7]]) as usize;
        let body = off + 8;
        if &b[off..off + 4] == b"data" {
            let end = if size == 0 || body + size > b.len() {
                b.len()
            } else {
                body + size
            };
            return &b[body..end];
        }
        off = body + size + (size & 1);
    }
    &[]
}

pub struct Emitter {
    child: Child,
}

impl Emitter {
    pub fn kill(mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Drop for Emitter {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
