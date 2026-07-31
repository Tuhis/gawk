//! The gawk-broadcast Windows shell (WB6, docs/38 D12): one window, the
//! Linux GUI's card architecture plus the capture picker; window-is-the-app
//! (closing ends the broadcast; no tray, no background presence).
//!
//! Threading model: Slint owns the UI thread; the engine runs on a tokio
//! runtime; media pumps run on their own threads. Everything flows back to
//! the UI through one std mpsc channel drained by a UI timer — no shared
//! state crosses the boundary.
#![cfg_attr(windows, windows_subsystem = "windows")]

#[cfg_attr(not(windows), allow(dead_code))]
mod base64util;
mod debuglog;
mod diagnostics;
#[cfg(windows)]
mod dpapi;
mod messages;
#[cfg(windows)]
mod pipeline;
#[cfg(windows)]
mod toast;

use gawk_engine::clock::MonotonicClock;
use gawk_engine::config::{self, Config};
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use gawk_engine::telemetry::{Hello, Reporter};
use messages::{StartFailure, can_mint, first_line, message};
use slint::{ComponentHandle, ModelRc, SharedString, VecModel};
use std::cell::RefCell;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::mpsc;

slint::include_modules!();

#[derive(Clone, Copy, PartialEq, Eq)]
enum UiState {
    Idle,
    Starting,
    Live,
}

/// Everything background threads report back to the UI.
#[cfg_attr(not(windows), allow(dead_code))] // Started carries no pipeline off-Windows
enum ShellMsg {
    Started {
        session: Arc<Session>,
        #[cfg(windows)]
        pipeline: pipeline::Pipeline,
    },
    StartFailed(StartFailure),
    Engine(EngineEvent),
}

/// Sequences resume-token persistence against the announce (docs/22 finding
/// 9: the relay's token stream can arrive BEFORE the announce). On a mint,
/// `cfg.last_broadcast_id` still holds the previous broadcast's id until the
/// announce lands — persisting a token the moment it arrives would pair the
/// new session's token with the old id, and a crash in that window leaves a
/// config whose Resume can only ever get the R17 gate's 403.
struct IdentityLatch {
    /// True once the running session's broadcast id is the persisted one —
    /// from its announce, or from the start on a resume (a reclaim's id is
    /// already the one on disk).
    announced: bool,
    pending_token: Option<String>,
}

impl IdentityLatch {
    fn new() -> Self {
        Self {
            announced: false,
            pending_token: None,
        }
    }

    /// A session is starting. `id_known` is true on a resume.
    fn on_start(&mut self, id_known: bool) {
        self.announced = id_known;
        self.pending_token = None;
    }

    /// A token arrived: returns it if it is safe to persist now, otherwise
    /// holds it for the announce.
    fn on_token(&mut self, token_hex: String) -> Option<String> {
        if self.announced {
            Some(token_hex)
        } else {
            self.pending_token = Some(token_hex);
            None
        }
    }

    /// The announce arrived: returns any held token, to persist together
    /// with the id it was minted for.
    fn on_announce(&mut self) -> Option<String> {
        self.announced = true;
        self.pending_token.take()
    }
}

struct Shell {
    cfg: Config,
    cfg_path: Option<std::path::PathBuf>,
    /// Where the debug log landed (None when no config dir / init failed);
    /// error cards point at it so refusals are diagnosable from the field.
    log_path: Option<std::path::PathBuf>,
    state: UiState,
    session: Option<Arc<Session>>,
    #[cfg(windows)]
    pipeline: Option<pipeline::Pipeline>,
    #[cfg(windows)]
    encode_info: Option<pipeline::PipelineInfo>,
    capture_mode: &'static str, // "app" | "screen"
    identity: IdentityLatch,
    broadcast_id: String,
    last_error: String,
    first_viewer_seen: bool,
    reporter: Arc<Reporter>,
    clock: Arc<MonotonicClock>,
    rt: tokio::runtime::Runtime,
    msg_tx: mpsc::Sender<ShellMsg>,
    msg_rx: mpsc::Receiver<ShellMsg>,
    #[cfg(windows)]
    picked_windows: Vec<gawk_capture::picker::WindowCandidate>,
    #[cfg(windows)]
    picked_monitors: Vec<gawk_capture::picker::MonitorCandidate>,
    stats_countdown: u8,
}

fn creds() -> Box<dyn config::Credentials> {
    #[cfg(windows)]
    {
        Box::new(dpapi::Dpapi)
    }
    #[cfg(not(windows))]
    {
        Box::new(config::Plaintext)
    }
}

fn main() {
    #[cfg(windows)]
    toast::init();

    let cfg_path = config::default_path();
    // The debug log lives next to broadcast.json; a windowed EXE has no
    // stderr, so this file is the only runtime record (docs/38 F-8).
    let log_path = debuglog::init(cfg_path.as_deref().and_then(|p| p.parent()));
    log::info!(
        "gawk-broadcast {} starting on {} {}",
        env!("CARGO_PKG_VERSION"),
        std::env::consts::OS,
        std::env::consts::ARCH
    );

    let clock = Arc::new(MonotonicClock::new());
    let reporter = Arc::new(Reporter::new(env!("CARGO_PKG_VERSION"), clock.clone()));
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .expect("tokio runtime");

    let cfg = match &cfg_path {
        Some(p) => {
            let (cfg, warn) = config::load(p, &*creds());
            if let Some(w) = warn {
                log::warn!("{w}");
            }
            cfg
        }
        None => Config::default(),
    };

    let (msg_tx, msg_rx) = mpsc::channel();
    let shell = Rc::new(RefCell::new(Shell {
        cfg,
        cfg_path,
        log_path,
        state: UiState::Idle,
        session: None,
        #[cfg(windows)]
        pipeline: None,
        #[cfg(windows)]
        encode_info: None,
        capture_mode: "app",
        identity: IdentityLatch::new(),
        broadcast_id: String::new(),
        last_error: String::new(),
        first_viewer_seen: false,
        reporter,
        clock,
        rt,
        msg_tx,
        msg_rx,
        #[cfg(windows)]
        picked_windows: Vec::new(),
        #[cfg(windows)]
        picked_monitors: Vec::new(),
        stats_countdown: 0,
    }));

    let ui = MainWindow::new().expect("create window");
    seed_settings(&ui, &shell.borrow().cfg);
    refresh_captions(&ui);
    ui.set_resume_code(shell.borrow().cfg.last_broadcast_id.clone().into());
    refresh_picker(&ui, &mut shell.borrow_mut());

    wire_callbacks(&ui, &shell);

    // One timer drains the message channel and, while broadcasting, ticks
    // stats/thumbnail/hints at 1 Hz. Idle cost: an empty channel poll.
    let timer = slint::Timer::default();
    {
        let ui_weak = ui.as_weak();
        let shell = shell.clone();
        timer.start(
            slint::TimerMode::Repeated,
            std::time::Duration::from_millis(250),
            move || {
                if let Some(ui) = ui_weak.upgrade() {
                    pump_messages(&ui, &shell);
                    tick(&ui, &shell);
                }
            },
        );
    }

    ui.run().expect("run event loop");
}

fn seed_settings(ui: &MainWindow, cfg: &Config) {
    ui.set_set_relay(cfg.relay_url.clone().into());
    ui.set_set_app_url(cfg.app_url.clone().into());
    ui.set_set_secret(cfg.publish_secret.clone().into());
    ui.set_set_telemetry(cfg.telemetry_url.clone().into());
    ui.set_set_bitrate(if cfg.bitrate_bps == 0 {
        SharedString::new()
    } else {
        format!("{}", cfg.bitrate_bps as f64 / 1e6).into()
    });
    ui.set_set_resolution(match (cfg.width, cfg.height) {
        (2560, 1440) => 0,
        (1280, 720) => 2,
        (854, 480) => 3,
        _ => 1,
    });
    ui.set_set_framerate(match cfg.fps {
        120 => 0,
        30 => 2,
        5 => 3,
        _ => 1,
    });
}

/// Reads the settings widgets back into the config: verbatim including
/// blanks — blank means "follow the default", and baking today's default in
/// would pin this user to it forever.
fn read_settings(ui: &MainWindow, cfg: &mut Config) {
    cfg.relay_url = ui.get_set_relay().trim().to_string();
    cfg.app_url = ui.get_set_app_url().trim().to_string();
    cfg.publish_secret = ui.get_set_secret().trim().to_string();
    cfg.telemetry_url = ui.get_set_telemetry().trim().to_string();
    cfg.bitrate_bps = parse_bitrate_mbps(ui.get_set_bitrate().as_str());
    (cfg.width, cfg.height) = match ui.get_set_resolution() {
        0 => (2560, 1440),
        2 => (1280, 720),
        3 => (854, 480),
        _ => (0, 0), // the default rung stays a blank, not a number
    };
    cfg.fps = match ui.get_set_framerate() {
        0 => 120,
        2 => 30,
        3 => 5,
        _ => 0,
    };
}

/// Blank/unparseable/≤0 ⇒ 0 (= the default); otherwise clamped to
/// [1, 100] Mbps. Comma decimals accepted (the Linux GUI's parser).
fn parse_bitrate_mbps(s: &str) -> u32 {
    let s = s.trim().replace(',', ".");
    if s.is_empty() {
        return 0;
    }
    match s.parse::<f64>() {
        Ok(v) if v > 0.0 => (v.clamp(1.0, 100.0) * 1e6) as u32,
        _ => 0,
    }
}

fn save_config(shell: &mut Shell) {
    if let Some(path) = shell.cfg_path.clone()
        && let Err(e) = config::save(&path, &shell.cfg, &*creds())
    {
        log::warn!("could not save settings: {e}");
    }
}

fn refresh_captions(ui: &MainWindow) {
    let relay = ui.get_set_relay();
    let telemetry = ui.get_set_telemetry();
    ui.set_caption_broadcast(
        format!(
            "Broadcasting to {}",
            config::resolve_relay_url(relay.as_str())
        )
        .into(),
    );
    let diag = config::resolve_telemetry_url(relay.as_str(), telemetry.as_str())
        .unwrap_or_else(|| "off — nothing is sent".into());
    ui.set_caption_diag(format!("Diagnostics to {diag}").into());
    let app_url = {
        let s = ui.get_set_app_url();
        let t = s.trim();
        if t.is_empty() {
            gawk_engine::defaults::APP_URL.to_string()
        } else {
            t.to_string()
        }
    };
    ui.set_terms_link(format!("{}/#/terms", app_url.trim_end_matches('/')).into());
}

fn refresh_picker(ui: &MainWindow, shell: &mut Shell) {
    #[cfg(windows)]
    {
        shell.picked_windows = gawk_capture::wgc::enumerate_windows();
        shell.picked_monitors = gawk_capture::wgc::enumerate_monitors();
        let rows: Vec<WindowRow> = shell
            .picked_windows
            .iter()
            .map(|w| {
                let (icon, has_icon) = match &w.icon {
                    Some(i) => (rgba_image(i.width, i.height, &i.rgba), true),
                    None => (slint::Image::default(), false),
                };
                WindowRow {
                    hwnd: w.hwnd as i32,
                    pid: w.pid as i32,
                    title: w.title.clone().into(),
                    icon,
                    has_icon,
                }
            })
            .collect();
        ui.set_windows(ModelRc::new(VecModel::from(rows)));
        let monitors: Vec<MonitorRow> = shell
            .picked_monitors
            .iter()
            .map(|m| MonitorRow {
                hmonitor: m.hmonitor as i32,
                label: m.label().into(),
            })
            .collect();
        ui.set_monitors(ModelRc::new(VecModel::from(monitors)));
    }
    #[cfg(not(windows))]
    {
        let _ = (ui, shell);
    }
}

#[cfg_attr(not(windows), allow(dead_code))]
fn rgba_image(w: u32, h: u32, rgba: &[u8]) -> slint::Image {
    let mut buf = slint::SharedPixelBuffer::<slint::Rgba8Pixel>::new(w, h);
    buf.make_mut_bytes().copy_from_slice(rgba);
    slint::Image::from_rgba8(buf)
}

fn wire_callbacks(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let ui_weak = ui.as_weak();

    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_refresh_picker(move || {
            if let Some(ui) = ui_weak.upgrade() {
                refresh_picker(&ui, &mut shell.borrow_mut());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_start_broadcast(move || {
            if let Some(ui) = ui_weak.upgrade() {
                start_broadcast(&ui, &shell, false);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_resume_broadcast(move || {
            if let Some(ui) = ui_weak.upgrade() {
                start_broadcast(&ui, &shell, true);
            }
        });
    }
    {
        let shell = shell.clone();
        ui.on_stop_broadcast(move || {
            let sh = shell.borrow();
            if let Some(session) = sh.session.clone() {
                sh.rt.spawn(async move { session.stop().await });
            }
        });
    }
    {
        let ui_weak = ui_weak.clone();
        ui.on_copy_link(move || {
            if let Some(ui) = ui_weak.upgrade() {
                copy_text(ui.get_join_link().as_str());
                ui.set_copied_note("Link copied".into());
            }
        });
    }
    {
        let ui_weak = ui_weak.clone();
        ui.on_copy_code(move || {
            if let Some(ui) = ui_weak.upgrade() {
                copy_text(ui.get_code().as_str());
                ui.set_copied_note("Code copied".into());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_copy_diagnostics(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let sh = shell.borrow();
                let st = merged_stats(&sh);
                let dump = diagnostics::render(
                    &st,
                    &sh.broadcast_id,
                    state_label(sh.state),
                    &sh.last_error,
                    sh.capture_mode,
                    debuglog::now_rfc3339(),
                );
                copy_text(&dump);
                ui.set_copied_note("Diagnostics copied".into());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_settings_edited(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                read_settings(&ui, &mut sh.cfg);
                save_config(&mut sh);
                drop(sh);
                refresh_captions(&ui);
            }
        });
    }
    {
        let shell = shell.clone();
        ui.on_switch_to_system_audio(move || {
            #[cfg(windows)]
            if let Some(p) = &shell.borrow().pipeline {
                p.switch_audio_to_system();
            }
            #[cfg(not(windows))]
            let _ = &shell;
        });
    }
    {
        ui.on_open_link(move |link| open_in_browser(link.as_str()));
    }
    {
        let shell = shell.clone();
        ui.on_quit_confirmed(move || {
            // Stop cleanly (bounded), then leave: the relay's grace period,
            // not this app, decides how long viewers wait.
            let session = shell.borrow_mut().session.take();
            if let Some(session) = session {
                let sh = shell.borrow();
                let _ = sh.rt.block_on(async {
                    tokio::time::timeout(std::time::Duration::from_secs(3), session.stop()).await
                });
            }
            #[cfg(windows)]
            if let Some(p) = shell.borrow_mut().pipeline.take() {
                p.shutdown();
            }
            let _ = slint::quit_event_loop();
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.window().on_close_requested(move || {
            let busy = shell.borrow().state != UiState::Idle;
            if busy {
                // Fullscreen-game alt-tab misclicks make accidental closes
                // likely on the target machine; ask (docs/38 D12).
                if let Some(ui) = ui_weak.upgrade() {
                    ui.set_confirm_close(true);
                }
                slint::CloseRequestResponse::KeepWindowShown
            } else {
                let _ = slint::quit_event_loop();
                slint::CloseRequestResponse::HideWindow
            }
        });
    }
}

fn state_label(s: UiState) -> &'static str {
    match s {
        UiState::Idle => "Not broadcasting",
        UiState::Starting => "Starting…",
        UiState::Live => "Live",
    }
}

fn start_broadcast(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, resume: bool) {
    let mut sh = shell.borrow_mut();
    if sh.state != UiState::Idle {
        return;
    }
    read_settings(ui, &mut sh.cfg);
    save_config(&mut sh);
    refresh_captions(ui);

    // The reclaim identity: only on the Resume button, and the persisted
    // token travels only with the ID it was minted for.
    let broadcast_id = if resume {
        sh.cfg.last_broadcast_id.clone()
    } else {
        String::new()
    };
    let resume_token = if resume && !broadcast_id.is_empty() {
        sh.cfg.last_resume_token.clone()
    } else {
        String::new()
    };

    // The picker decides mode + target BEFORE the session dial, so a
    // missing selection is an instant, local error.
    #[cfg(windows)]
    let target = {
        let tab = ui.get_picker_tab();
        if tab == 0 {
            let idx = ui.get_selected_window();
            match sh.picked_windows.get(idx.max(0) as usize) {
                Some(w) if idx >= 0 => gawk_capture::wgc::CaptureTarget::Window {
                    hwnd: w.hwnd,
                    pid: w.pid,
                },
                _ => {
                    ui.set_error_text("Pick a window (or a screen) to share first.".into());
                    return;
                }
            }
        } else {
            let idx = ui.get_selected_monitor();
            match sh.picked_monitors.get(idx.max(0) as usize) {
                Some(m) if idx >= 0 => gawk_capture::wgc::CaptureTarget::Monitor {
                    hmonitor: m.hmonitor,
                },
                _ => {
                    ui.set_error_text("Pick a screen (or a window) to share first.".into());
                    return;
                }
            }
        }
    };
    #[cfg(windows)]
    {
        sh.capture_mode = if ui.get_picker_tab() == 0 {
            "app"
        } else {
            "screen"
        };
    }

    log::info!(
        "start requested: mode {}, resume {resume} (id {:?}), relay {}",
        sh.capture_mode,
        broadcast_id,
        sh.cfg.resolve_relay_url()
    );
    sh.state = UiState::Starting;
    sh.identity.on_start(!broadcast_id.is_empty());
    sh.last_error.clear();
    sh.first_viewer_seen = false;
    ui.set_error_text("".into());
    ui.set_can_mint(false);
    ui.set_busy(true);
    ui.set_live(false);
    ui.set_state_label("Starting…".into());
    ui.set_copied_note("".into());

    let scfg = SessionConfig {
        relay_url: sh.cfg.resolve_relay_url(),
        broadcast_id,
        resume_token_hex: resume_token,
        publish_secret: sh.cfg.publish_secret.clone(),
        origin: sh.cfg.resolve_origin(),
        insecure: false,
    };
    let clock: Arc<dyn gawk_engine::clock::Clock> = sh.clock.clone();
    let msg_tx = sh.msg_tx.clone();
    let rt_handle = sh.rt.handle().clone();
    sh.reporter.set_url(sh.cfg.resolve_telemetry_url());

    #[cfg(windows)]
    let build_params = {
        let (w, h, fps, bps) = sh.cfg.resolve_rung();
        pipeline::PipelineParams {
            target,
            width: w,
            height: h,
            fps,
            bitrate_bps: bps,
            last_good_encoder: (!sh.cfg.last_good_encoder.is_empty())
                .then(|| sh.cfg.last_good_encoder.clone()),
            audio_mode: if sh.cfg.disable_audio {
                gawk_audio::AudioMode::Off
            } else if sh.capture_mode == "app" {
                match target {
                    gawk_capture::wgc::CaptureTarget::Window { pid, .. } => {
                        gawk_audio::AudioMode::ProcessLoopback { pid }
                    }
                    _ => gawk_audio::AudioMode::SystemLoopback,
                }
            } else {
                gawk_audio::AudioMode::SystemLoopback
            },
        }
    };

    drop(sh);

    std::thread::spawn(move || {
        // Phase 1: connect (the only phase where a mint may be offered).
        let started = rt_handle.block_on(Session::start(scfg, clock.clone()));
        let (session, mut events) = match started {
            Ok(x) => x,
            Err(e) => {
                log::error!(
                    "relay connect failed: {} (phase {:?}, status {})",
                    e.message,
                    e.phase,
                    e.status
                );
                let _ = msg_tx.send(ShellMsg::StartFailed(StartFailure::Relay(e)));
                return;
            }
        };

        // Engine events flow to the UI for the life of the session.
        {
            let msg_tx = msg_tx.clone();
            rt_handle.spawn(async move {
                while let Some(ev) = events.recv().await {
                    let _ = msg_tx.send(ShellMsg::Engine(ev));
                }
            });
        }

        // Phase 2: media. A failure here stops the session and is NOT
        // offered a mint (R1's rule — the relay may already hold our ID).
        #[cfg(windows)]
        {
            match pipeline::Pipeline::build(
                build_params,
                session.sender(),
                clock,
                rt_handle.clone(),
            ) {
                Ok(p) => {
                    let _ = msg_tx.send(ShellMsg::Started {
                        session,
                        pipeline: p,
                    });
                }
                Err(f) => {
                    rt_handle.block_on(session.stop());
                    let _ = msg_tx.send(ShellMsg::StartFailed(f));
                }
            }
        }
        #[cfg(not(windows))]
        {
            rt_handle.block_on(session.stop());
            let _ = msg_tx.send(ShellMsg::StartFailed(StartFailure::Capture(
                "this binary only captures on Windows (dev shell)".into(),
            )));
        }
    });
}

fn pump_messages(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    loop {
        let msg = shell.borrow().msg_rx.try_recv();
        match msg {
            Ok(m) => handle_message(ui, shell, m),
            Err(_) => break,
        }
    }
}

fn handle_message(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, msg: ShellMsg) {
    match msg {
        ShellMsg::Started {
            session,
            #[cfg(windows)]
            pipeline,
        } => {
            let mut sh = shell.borrow_mut();
            // The engine-event forwarder runs while Pipeline::build is still
            // working, so an Ended (e.g. a 4004 supersede during the
            // trial-encode phase) can be handled BEFORE this message. That
            // ending already ran end_broadcast; a late Started must not
            // resurrect the dead session as "Live" — the run loop's single
            // Ended has been spent, so nothing would ever flip the UI back.
            if sh.state != UiState::Starting {
                #[cfg(windows)]
                pipeline.shutdown();
                sh.rt.spawn(async move { session.stop().await });
                return;
            }
            sh.session = Some(session);
            #[cfg(windows)]
            {
                // Cache the accepted encoder for next launch (D9).
                if sh.cfg.last_good_encoder != pipeline.info.encoder {
                    sh.cfg.last_good_encoder = pipeline.info.encoder.clone();
                    save_config(&mut sh);
                }
                let (_, h, fps, _) = sh.cfg.resolve_rung();
                ui.set_encode_line(
                    format!(
                        "Media Foundation — {} · {} · {}p{}",
                        pipeline.info.encoder, pipeline.info.capture_path, h, fps
                    )
                    .into(),
                );
                sh.encode_info = Some(pipeline.info.clone());
                sh.pipeline = Some(pipeline);
                ui.set_show_thumbnail(sh.capture_mode == "app");
            }
            sh.state = UiState::Live;
            log::info!("live (broadcast id {:?})", sh.broadcast_id);
            let body = live_body(&sh.broadcast_id, ui.get_join_link().as_str());
            drop(sh);
            ui.set_live(true);
            ui.set_busy(true);
            ui.set_state_label("Live".into());
            notify("Broadcast started", &body, false);
        }
        ShellMsg::StartFailed(f) => {
            let mut sh = shell.borrow_mut();
            sh.state = UiState::Idle;
            sh.session = None;
            let app_url = sh.cfg.resolve_app_url();
            let text = message(&f, &app_url);
            log::error!("start failed: {}", first_line(&text));
            sh.last_error = text.clone();
            // The error card points at the debug log: the curated sentence
            // says what happened, the log says why (docs/38 F-8).
            let card = debuglog::with_pointer(&text, sh.log_path.as_deref());
            drop(sh);
            ui.set_busy(false);
            ui.set_live(false);
            ui.set_state_label("Not broadcasting".into());
            ui.set_error_text(card.into());
            ui.set_can_mint(can_mint(&f));
            notify("Broadcast failed to start", first_line(&text), true);
        }
        ShellMsg::Engine(ev) => handle_engine_event(ui, shell, ev),
    }
}

fn handle_engine_event(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, ev: EngineEvent) {
    match ev {
        EngineEvent::Announce { broadcast_id } => {
            log::info!("announce: broadcast id {broadcast_id}");
            let mut sh = shell.borrow_mut();
            sh.broadcast_id = broadcast_id.clone();
            sh.cfg.last_broadcast_id = broadcast_id.clone();
            // A token that beat this announce persists with it, atomically
            // paired with the id it was minted for.
            if let Some(token) = sh.identity.on_announce() {
                sh.cfg.last_resume_token = token;
            }
            save_config(&mut sh);
            let link = gawk_engine::join_link(&sh.cfg.resolve_app_url(), &broadcast_id);
            drop(sh);
            ui.set_code(broadcast_id.clone().into());
            ui.set_join_link(link.into());
            ui.set_resume_code(broadcast_id.into());
        }
        EngineEvent::ResumeToken { token_hex } => {
            let mut sh = shell.borrow_mut();
            if let Some(token) = sh.identity.on_token(token_hex) {
                sh.cfg.last_resume_token = token;
                save_config(&mut sh);
            }
        }
        EngineEvent::ViewerCount(n) => {
            let mut sh = shell.borrow_mut();
            ui.set_watching_line(format!("{n} watching").into());
            if n > 0 && !sh.first_viewer_seen && sh.state == UiState::Live {
                sh.first_viewer_seen = true;
                notify(
                    "First viewer joined",
                    "Someone is watching your stream.",
                    false,
                );
            }
        }
        EngineEvent::TelemetryHello {
            enabled,
            report_interval_ms,
            token,
            broadcast_key_hex,
        } => {
            let sh = shell.borrow();
            sh.reporter.begin(&Hello {
                enabled,
                report_interval_ms,
                token,
                broadcast_key_hex,
            });
        }
        EngineEvent::Resuming { attempt } => {
            log::info!("resuming (attempt {attempt})");
            let sh = shell.borrow();
            sh.reporter.event("resuming", "");
            drop(sh);
            ui.set_resuming(true);
            ui.set_status_line(
                if attempt > 1 {
                    format!("Reconnecting to the relay… (attempt {attempt})")
                } else {
                    "Reconnecting to the relay…".to_string()
                }
                .into(),
            );
        }
        EngineEvent::Resumed => {
            log::info!("resumed");
            let sh = shell.borrow();
            sh.reporter.event("resumed", "");
            #[cfg(windows)]
            if let Some(p) = &sh.pipeline {
                // Re-prime the relay's invalidated keyframe cache NOW
                // instead of waiting out the GOP (docs/38 D5).
                p.force_idr();
            }
            drop(sh);
            ui.set_resuming(false);
            ui.set_status_line("".into());
        }
        EngineEvent::Ended { error } => end_broadcast(ui, shell, error),
    }
}

fn end_broadcast(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, error: Option<String>) {
    let mut sh = shell.borrow_mut();
    let was_live = sh.state == UiState::Live;
    sh.state = UiState::Idle;
    sh.session = None;
    #[cfg(windows)]
    {
        sh.encode_info = None;
        if let Some(p) = sh.pipeline.take() {
            p.shutdown();
        }
    }
    if let Some(e) = &error {
        log::error!("broadcast ended with error: {e}");
        sh.last_error = e.clone();
        sh.reporter.event("error", e);
    } else {
        log::info!("broadcast ended");
    }
    sh.reporter.event("ended", "");
    sh.reporter.finish();
    drop(sh);

    ui.set_busy(false);
    ui.set_live(false);
    ui.set_resuming(false);
    ui.set_state_label("Not broadcasting".into());
    ui.set_status_line("".into());
    ui.set_watching_line("".into());
    ui.set_encode_line("".into());
    ui.set_audio_line("".into());
    ui.set_audio_hint(false);
    ui.set_show_thumbnail(false);
    ui.set_minimized_hint("".into());
    if let Some(e) = &error {
        ui.set_error_text(e.clone().into());
        ui.set_can_mint(false);
        notify(
            "Broadcast ended unexpectedly",
            "Your screen is no longer being shared.",
            true,
        );
    } else if was_live {
        notify(
            "Broadcast ended",
            "Your screen is no longer being shared.",
            false,
        );
    }
}

/// The 1 Hz working tick while broadcasting: stats rows, telemetry sample,
/// thumbnail, minimized hint, audio line, pipeline failure surfacing.
fn tick(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    {
        let mut sh = shell.borrow_mut();
        if sh.state == UiState::Idle {
            return;
        }
        sh.stats_countdown = sh.stats_countdown.saturating_sub(1);
        if sh.stats_countdown > 0 {
            return;
        }
        sh.stats_countdown = 4; // 4 × 250 ms = 1 s
    }

    // A dead media pump ends the broadcast through the normal path.
    #[cfg(windows)]
    {
        let failure = shell
            .borrow()
            .pipeline
            .as_ref()
            .and_then(|p| p.take_failure());
        if let Some(f) = failure {
            log::error!("media pump died: {f}");
            let sh = shell.borrow();
            if let Some(session) = sh.session.clone() {
                sh.rt.spawn(async move { session.stop().await });
            }
            drop(sh);
            ui.set_error_text(f.into());
            return;
        }
    }

    let sh = shell.borrow();
    let st = merged_stats(&sh);
    sh.reporter.report(st.clone());
    sh.reporter.tick();

    ui.set_stats_rows(ModelRc::new(VecModel::from(stat_rows(&st))));

    #[cfg(windows)]
    if let Some(p) = &sh.pipeline {
        ui.set_audio_level(p.audio_level());
        let state = p.audio_state();
        ui.set_audio_line(audio_line(&state, sh.capture_mode).into());
        let hint = sh.capture_mode == "app" && p.audio_silence_hint();
        ui.set_audio_hint(hint);
        if hint {
            ui.set_audio_hint_text(
                "No audio from the app yet. Some games play audio through a helper process this capture can't see — switch to whole-system audio?"
                    .into(),
            );
        }
        ui.set_minimized_hint(
            if p.minimized() {
                "The shared window is minimized — restore it to resume video."
            } else {
                ""
            }
            .into(),
        );
        if let Some((w, h, rgba)) = p.take_thumbnail() {
            ui.set_thumbnail(rgba_image(w, h, &rgba));
        }
    }
}

/// Session counters merged with what the shell knows that the sender
/// cannot: the rung, the accepted encoder, real capture fps, audio state.
fn merged_stats(sh: &Shell) -> gawk_engine::stats::Stats {
    let mut st = sh.session.as_ref().map(|s| s.stats()).unwrap_or_default();
    let (w, h, fps, bps) = sh.cfg.resolve_rung();
    st.width = w;
    st.height = h;
    st.fps = fps;
    st.bitrate_bps = bps;
    #[cfg(windows)]
    {
        if let Some(info) = &sh.encode_info {
            st.encoder = info.encoder.clone();
            st.capture_path = info.capture_path.into();
            if st.codec.is_empty() {
                st.codec = info.codec.clone();
            }
        }
        if let Some(p) = &sh.pipeline {
            if let Some(f) = p.capture_fps() {
                st.capture_fps_available = true;
                st.capture_fps = f;
            }
            st.audio_state = p.audio_state();
        }
    }
    if st.audio_state.is_empty() {
        st.audio_state = "off".into();
    }
    st
}

#[cfg_attr(not(windows), allow(dead_code))]
fn audio_line(state: &str, capture_mode: &str) -> String {
    match state {
        "active" => {
            if capture_mode == "app" {
                "App audio".into()
            } else {
                "System audio".into()
            }
        }
        "unavailable" => "No audio — this machine has no usable audio source".into(),
        "error" => "No audio — the encoder produced a stream we could not publish".into(),
        _ => "No audio (turned off)".into(),
    }
}

fn stat_rows(st: &gawk_engine::stats::Stats) -> Vec<StatRow> {
    let row = |label: &str, value: String| StatRow {
        label: label.into(),
        value: value.into(),
    };
    let na = || "n/a".to_string();
    vec![
        row(
            "Watching",
            if st.viewer_count_available {
                format!("{}", st.viewer_count)
            } else {
                "n/a (relay predates R18)".into()
            },
        ),
        row(
            "Encoder",
            if st.encoder.is_empty() {
                "—".into()
            } else {
                st.encoder.clone()
            },
        ),
        row(
            "Capture path",
            if st.capture_path.is_empty() {
                "—".into()
            } else {
                st.capture_path.clone()
            },
        ),
        row(
            "Codec",
            if st.codec.is_empty() {
                "—".into()
            } else {
                st.codec.clone()
            },
        ),
        row(
            "Rung",
            format!(
                "{}x{}@{} · {:.1} Mbps",
                st.width,
                st.height,
                st.fps,
                f64::from(st.bitrate_bps) / 1e6
            ),
        ),
        row(
            "Capture fps",
            if st.capture_fps_available {
                format!("{:.1}", st.capture_fps)
            } else {
                na()
            },
        ),
        row("Encode fps", format!("{:.1}", st.encoder_fps)),
        row("Sent fps", format!("{:.1}", st.sent_fps)),
        row(
            "Keyframes",
            format!(
                "{} sent · {} failed · {} superseded",
                st.keyframe_streams_sent,
                st.keyframe_streams_failed,
                st.keyframe_streams_superseded
            ),
        ),
        row(
            "Keyframe interval",
            if st.keyframe_interval_available {
                format!("{:.0} ms (target 500)", st.keyframe_interval_ms)
            } else {
                na()
            },
        ),
        row("Dropped at send", format!("{}", st.frames_dropped_at_send)),
        row(
            "Datagrams",
            format!(
                "{} · {:.1} MB",
                st.datagrams_sent,
                st.bytes_sent as f64 / 1e6
            ),
        ),
        row(
            "RTT (time-sync)",
            if st.time_sync_available {
                format!("{:.1} ms", st.time_sync_rtt_ms)
            } else {
                na()
            },
        ),
        row("Audio", st.audio_state.clone()),
        row(
            "Audio format",
            if st.audio_codec.is_empty() {
                "—".into()
            } else {
                format!(
                    "{} · {} Hz · {} ch · {:.0} kbps",
                    st.audio_codec,
                    st.audio_sample_rate,
                    st.audio_channels,
                    f64::from(st.audio_bitrate_bps) / 1000.0
                )
            },
        ),
        row(
            "Audio packets",
            format!(
                "{} sent · {} configs · {} dropped · {:.2} MB",
                st.audio_packets_sent,
                st.audio_configs_sent,
                st.audio_packets_dropped,
                st.audio_bytes_sent as f64 / 1e6
            ),
        ),
    ]
}

fn live_body(id: &str, link: &str) -> String {
    if id.is_empty() {
        "Your screen is being shared.".into()
    } else if link.is_empty() {
        format!("Code {id}")
    } else {
        format!("Code {id} · {link}")
    }
}

fn notify(summary: &str, body: &str, critical: bool) {
    #[cfg(windows)]
    toast::show(
        summary,
        body,
        if critical {
            toast::Urgency::Critical
        } else {
            toast::Urgency::Normal
        },
    );
    #[cfg(not(windows))]
    eprintln!("[{}] {summary}: {body}", if critical { "!" } else { " " });
}

fn copy_text(text: &str) {
    if let Ok(mut cb) = arboard::Clipboard::new() {
        let _ = cb.set_text(text.to_string());
    }
}

fn open_in_browser(url: &str) {
    if url.is_empty() {
        return;
    }
    #[cfg(windows)]
    let _ = std::process::Command::new("cmd")
        .args(["/c", "start", "", url])
        .spawn();
    #[cfg(target_os = "macos")]
    let _ = std::process::Command::new("open").arg(url).spawn();
    #[cfg(all(unix, not(target_os = "macos")))]
    let _ = std::process::Command::new("xdg-open").arg(url).spawn();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bitrate_parser_matches_the_linux_gui() {
        assert_eq!(parse_bitrate_mbps(""), 0);
        assert_eq!(parse_bitrate_mbps("  "), 0);
        assert_eq!(parse_bitrate_mbps("junk"), 0);
        assert_eq!(parse_bitrate_mbps("-4"), 0);
        assert_eq!(parse_bitrate_mbps("16"), 16_000_000);
        assert_eq!(parse_bitrate_mbps("2,5"), 2_500_000);
        assert_eq!(parse_bitrate_mbps("0.5"), 1_000_000); // clamped up
        assert_eq!(parse_bitrate_mbps("400"), 100_000_000); // clamped down
    }

    // docs/22 finding 9: the token stream can beat the announce. On a mint
    // the persisted id is still the PREVIOUS broadcast's until the announce,
    // so an early token must be held, never persisted against the old id.
    #[test]
    fn a_token_that_beats_the_announce_is_held_for_it() {
        let mut latch = IdentityLatch::new();
        latch.on_start(false); // mint: no id yet
        assert_eq!(latch.on_token("aa11".into()), None, "must not persist yet");
        assert_eq!(
            latch.on_announce(),
            Some("aa11".into()),
            "the announce releases the held token"
        );
        // After the announce, later tokens (mid-session re-mints) persist
        // immediately — the id on disk is now this session's.
        assert_eq!(latch.on_token("bb22".into()), Some("bb22".into()));
    }

    #[test]
    fn announce_then_token_persists_immediately() {
        let mut latch = IdentityLatch::new();
        latch.on_start(false);
        assert_eq!(latch.on_announce(), None);
        assert_eq!(latch.on_token("aa11".into()), Some("aa11".into()));
    }

    #[test]
    fn a_resume_start_reclaims_a_known_id_so_tokens_persist_immediately() {
        let mut latch = IdentityLatch::new();
        latch.on_start(true); // resume: the id on disk IS this session's
        assert_eq!(latch.on_token("aa11".into()), Some("aa11".into()));
    }

    #[test]
    fn a_new_start_clears_any_stale_held_token() {
        let mut latch = IdentityLatch::new();
        latch.on_start(false);
        assert_eq!(latch.on_token("aa11".into()), None);
        // The session dies before its announce; a new mint starts.
        latch.on_start(false);
        assert_eq!(
            latch.on_announce(),
            None,
            "the dead session's token must not attach to the new announce"
        );
    }

    #[test]
    fn live_body_composes_like_the_linux_shell() {
        assert_eq!(live_body("", ""), "Your screen is being shared.");
        assert_eq!(live_body("K7XQ2M", ""), "Code K7XQ2M");
        assert_eq!(
            live_body("K7XQ2M", "https://gawk.ioio.fi/#/view/K7XQ2M"),
            "Code K7XQ2M · https://gawk.ioio.fi/#/view/K7XQ2M"
        );
    }
}
