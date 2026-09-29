//! The in-process app-audio control plane against a REAL PipeWire daemon
//! (docs/58 D8/D12, LX4): the docs/39 behaviours restated on the Rust
//! implementation — the list, the tee, churn, re-targeting into the same
//! sink, the mid-session switch, the link count answer — and the kill
//! matrix that proves cleanup-by-construction on the connection boundary
//! (G11): SIGKILL, a panic and a clean exit each leave no
//! `gawk-app-capture-*` node or link behind.
#![cfg(target_os = "linux")]

mod support;

use gawk_audio::pwctl::{Notice, PwCtl};
use gawk_audio::pwgraph::Event;
use std::process::{Command, Stdio};
use std::time::Duration;
use support::pwtest::{daemon, wait_for, wait_until};

fn apps_seen(ctl: &PwCtl, binary: &str) -> bool {
    ctl.drain().iter().any(|n| {
        matches!(n, Notice::Event(Event::Apps(apps)) if apps.iter().any(|a| a.binary == binary))
    })
}

fn wait_apps(ctl: &PwCtl, binary: &str) {
    wait_for(&format!("{binary} in the app list"), || {
        ctl.watch();
        std::thread::sleep(Duration::from_millis(50));
        apps_seen(ctl, binary)
    });
}

/// docs/39 F8's scenario: an application already playing when the control
/// plane connects is in its FIRST list — the opening registry burst is not
/// dropped, and its binary (bound-object properties, F1) is resolved.
#[test]
fn an_app_already_playing_is_listed_from_the_start() {
    let Some((d, _g)) = daemon() else { return };
    let _game = d.emitter("early-game", 440, 2);
    let ctl = PwCtl::start().expect("control plane");
    let first = ctl.drain();
    assert!(
        first.iter().any(|n| matches!(n,
            Notice::Event(Event::Apps(a)) if a.iter().any(|a| a.binary == "early-game"))),
        "first notices: {first:?}"
    );
    ctl.stop();
}

#[test]
fn the_list_updates_live() {
    let Some((d, _g)) = daemon() else { return };
    let ctl = PwCtl::start().expect("control plane");
    let game = d.emitter("late-game", 440, 2);
    wait_apps(&ctl, "late-game");
    game.kill();
    wait_for("late-game to leave the list", || {
        ctl.watch();
        std::thread::sleep(Duration::from_millis(50));
        ctl.drain().iter().any(|n| {
            matches!(n, Notice::Event(Event::Apps(a)) if !a.iter().any(|a| a.binary == "late-game"))
        })
    });
    ctl.stop();
}

/// The tee, measured: the app is heard in OUR sink AND still on the
/// speakers — a copy, never a re-route. The sink is Internal (hidden from
/// device lists).
#[test]
fn capture_is_a_tee_not_a_reroute() {
    let Some((d, _g)) = daemon() else { return };
    let _game = d.emitter("tee-game", 440, 2);
    let ctl = PwCtl::start().expect("control plane");
    wait_apps(&ctl, "tee-game");
    let serial = ctl.capture(Some("tee-game")).expect("capture");
    let ours = d
        .find_node(|n| n.serial == serial)
        .expect("the sink is in the graph");
    assert!(ours.name.starts_with("gawk-app-capture-"), "{ours:?}");
    assert_eq!(ours.media_class, "Audio/Sink/Internal");
    wait_for("the game's ports linked into our sink", || {
        d.links_into(ours.id) >= 2
    });
    let speakers = d.find_node(|n| n.name == "speakers").unwrap();
    assert!(
        d.capture_peak(serial, Duration::from_millis(700)) > 0.05,
        "our sink hears it"
    );
    assert!(
        d.capture_peak(speakers.serial, Duration::from_millis(700)) > 0.05,
        "the speakers still hear it"
    );
    ctl.stop();
}

/// Churn (docs/39 AG4): the stream dies (a menu), links drop to zero and
/// the count says so; it comes back, and is re-linked by binary.
#[test]
fn links_follow_stream_churn_and_zero_is_reported() {
    let Some((d, _g)) = daemon() else { return };
    let game = d.emitter("churn-game", 330, 2);
    let ctl = PwCtl::start().expect("control plane");
    wait_apps(&ctl, "churn-game");
    let serial = ctl.capture(Some("churn-game")).unwrap();
    let sink = d.find_node(|n| n.serial == serial).unwrap();
    wait_for("links", || d.links_into(sink.id) >= 2);
    game.kill();
    wait_for("a zero link count", || {
        ctl.drain()
            .iter()
            .any(|n| matches!(n, Notice::Event(Event::Links { links: 0, .. })))
    });
    let _again = d.emitter("churn-game", 330, 2);
    wait_for("re-linked after the restart", || d.links_into(sink.id) >= 2);
    ctl.stop();
}

/// docs/39 F2 and F4: another app, then whole-system audio, re-link into
/// the SAME sink — the serial the audio pipeline reads never changes.
#[test]
fn retargeting_and_the_system_switch_keep_the_same_sink() {
    let Some((d, _g)) = daemon() else { return };
    let _a = d.emitter("retarget-a", 440, 2);
    let _b = d.emitter("retarget-b", 660, 2);
    let ctl = PwCtl::start().expect("control plane");
    wait_apps(&ctl, "retarget-b");
    let s1 = ctl.capture(Some("retarget-a")).unwrap();
    let s2 = ctl.capture(Some("retarget-b")).unwrap();
    let s3 = ctl.capture(None).unwrap();
    assert_eq!((s1, s2), (s2, s3), "one sink for the whole broadcast");
    assert_eq!(d.gawk_objects().len(), 1);
    let sink = d.find_node(|n| n.serial == s3).unwrap();
    wait_for("the speakers' monitors linked in", || {
        d.links_into(sink.id) >= 2
    });
    assert!(d.capture_peak(s3, Duration::from_millis(700)) > 0.05);
    ctl.stop();
}

/// docs/39 F5: capturing a silent application answers at once, with no
/// links and no error.
#[test]
fn capturing_a_silent_application_yields_no_links_and_no_error() {
    let Some((d, _g)) = daemon() else { return };
    let ctl = PwCtl::start().expect("control plane");
    let serial = ctl
        .capture(Some("not-playing"))
        .expect("a sink, not an error");
    let sink = d.find_node(|n| n.serial == serial).unwrap();
    assert_eq!(d.links_into(sink.id), 0);
    ctl.stop();
    wait_for("the sink gone after a clean stop", || {
        d.gawk_objects().is_empty()
    });
}

// ----- the kill matrix (G11) -----

/// Runs in a CHILD process (the parent re-executes this test binary): starts
/// the control plane, captures, reports the serial, then ends the way
/// `GAWK_PWCTL_CHILD` says.
#[test]
#[ignore = "child process of the kill matrix; run by kill_matrix_* below"]
fn kill_matrix_child() {
    let Ok(mode) = std::env::var("GAWK_PWCTL_CHILD") else {
        return;
    };
    let ctl = PwCtl::start().expect("control plane");
    let serial = ctl.capture(Some("kill-game")).expect("capture");
    println!("SERIAL {serial}");
    use std::io::Write;
    std::io::stdout().flush().unwrap();
    match mode.as_str() {
        "clean" => {
            std::thread::sleep(Duration::from_millis(500));
            ctl.stop();
        }
        "panic" => {
            std::thread::sleep(Duration::from_millis(500));
            panic!("injected panic with the control plane live");
        }
        _ => loop {
            std::thread::sleep(Duration::from_secs(1));
        },
    }
}

fn kill_matrix(mode: &str) {
    let Some((d, _g)) = daemon() else { return };
    let _game = d.emitter("kill-game", 440, 2);
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "kill_matrix_child",
            "--ignored",
            "--nocapture",
            "--test-threads=1",
        ])
        .env("GAWK_PWCTL_CHILD", mode)
        .envs(d.env.iter().map(|(k, v)| (k, v)))
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let serial = {
        use std::io::BufRead;
        let out = child.stdout.take().unwrap();
        let mut lines = std::io::BufReader::new(out).lines();
        loop {
            let line = lines
                .next()
                .expect("child exited before capturing")
                .unwrap();
            // libtest prints "test kill_matrix_child ... " on the same line.
            if let Some(s) = line.split("SERIAL ").nth(1) {
                break s.split_whitespace().next().unwrap().parse::<u32>().unwrap();
            }
        }
    };
    let sink = d
        .find_node(|n| n.serial == serial)
        .expect("the child's sink exists");
    wait_for("the child's links", || d.links_into(sink.id) >= 2);
    if mode == "sigkill" {
        child.kill().unwrap();
    }
    let _ = child.wait();
    assert!(
        wait_until(Duration::from_secs(10), || d.gawk_objects().is_empty()
            && d.links_into(sink.id) == 0),
        "{mode}: gawk objects survived: {:?}",
        d.gawk_objects()
    );
}

#[test]
fn kill_matrix_sigkill_leaves_nothing() {
    kill_matrix("sigkill");
}

#[test]
fn kill_matrix_panic_leaves_nothing() {
    kill_matrix("panic");
}

#[test]
fn kill_matrix_clean_exit_leaves_nothing() {
    kill_matrix("clean");
}

// ----- the GStreamer side (docs/58 D7) against the same daemon -----

use gawk_audio::gstsrc::{self, Source};

#[test]
fn the_system_cascade_picks_the_pipewire_monitor() {
    let Some((_d, _g)) = daemon() else { return };
    let order = gstsrc::order("", "");
    let chosen = gstsrc::select(&order).expect("a system-audio source");
    assert_eq!(chosen, Source::PipewireMonitor);
}

/// The app-sink monitor, end to end: pwctl creates and links the sink,
/// gst captures its monitor by serial, and the samples are the app's tone
/// as F32 stereo on the monotonic clock.
#[test]
fn the_app_sink_monitor_delivers_the_apps_samples() {
    let Some((d, _g)) = daemon() else { return };
    let _game = d.emitter("gst-game", 440, 2);
    let ctl = PwCtl::start().expect("control plane");
    wait_apps(&ctl, "gst-game");
    let serial = ctl.capture(Some("gst-game")).unwrap();
    let src = Source::AppSinkMonitor(serial);
    gstsrc::trial(&src, std::time::Instant::now() + gstsrc::PROBE_BUDGET).expect("trial");

    let peak = std::sync::Arc::new(std::sync::Mutex::new((0f32, 0usize, None::<u64>)));
    let errors = std::sync::Arc::new(std::sync::Mutex::new(Vec::<String>::new()));
    let cap = {
        let peak = peak.clone();
        let errors = errors.clone();
        gstsrc::Capture::start(
            &src,
            move |pcm| {
                let mut p = peak.lock().unwrap();
                for s in pcm.bytes.chunks_exact(4) {
                    p.0 = p.0.max(f32::from_le_bytes([s[0], s[1], s[2], s[3]]).abs());
                }
                p.1 += pcm.frames;
                p.2 = pcm.capture_ns.or(p.2);
            },
            move |e| errors.lock().unwrap().push(e),
        )
        .expect("capture")
    };
    wait_for("a second of samples", || peak.lock().unwrap().1 >= 48_000);
    cap.stop();
    let (p, _, ns) = *peak.lock().unwrap();
    assert!(p > 0.05, "peak {p}: the app's tone reached the monitor");
    assert!(ns.is_some(), "buffers carry a capture time");
    assert!(
        errors.lock().unwrap().is_empty(),
        "{:?}",
        errors.lock().unwrap()
    );
    ctl.stop();
}
