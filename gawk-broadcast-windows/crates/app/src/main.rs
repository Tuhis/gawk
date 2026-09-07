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
mod version;

use gawk_engine::clock::MonotonicClock;
use gawk_engine::config::{self, Config, DEFAULT_SERVER_NAME, ServerProfile};
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
        // Boxed: the pipeline dwarfs the other variants (clippy
        // large_enum_variant) and this message is sent once per broadcast.
        #[cfg(windows)]
        pipeline: Box<pipeline::Pipeline>,
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
    /// 1-per-minute keyframe/fps health line into debug.log (F-12: the
    /// supersede livelock was invisible without send-side counters).
    health_countdown: u8,
    /// The upload-bandwidth watchdog, fed 1 Hz; fresh per broadcast.
    uplink: gawk_engine::uplink::UplinkMonitor,
    uplink_warned: bool,
    /// R42: the grant the "Open room view" link carries — the creator token
    /// (hex) of a room this session minted, or the static room's attach
    /// key. In memory only: it is a one-broadcast affair.
    room_grant: String,
    /// Set when the user clicked Detach/Leave, so the RoomDetached that
    /// follows is read as "we left" rather than "the creator removed us".
    room_leaving: bool,
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
    // Panics must reach the log: with no console, an unhooked panic is a
    // thread silently gone (the F-10 symptom class). The default hook still
    // runs after ours so dev shells keep the stderr backtrace.
    let default_hook = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        let thread = std::thread::current();
        log::error!("PANIC on thread {:?}: {info}", thread.name().unwrap_or("?"));
        default_hook(info);
    }));
    log::info!(
        "gawk-broadcast v{} starting on {} {}",
        version::display(),
        std::env::consts::OS,
        std::env::consts::ARCH
    );
    #[cfg(windows)]
    log::info!(
        "UniversalApiContract level: {}",
        gawk_capture::wgc::universal_contract_level()
    );

    let clock = Arc::new(MonotonicClock::new());
    // version::RELEASE, not version::display(): this field doubles as the
    // telemetry schema version and gawk-telemetry groups sessions by it, so
    // the per-build "+g<sha>" suffix belongs in the window and the diagnostics
    // dump, not on the wire.
    let reporter = Arc::new(Reporter::new(version::RELEASE, clock.clone()));
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .expect("tokio runtime");

    let mut cfg = match &cfg_path {
        Some(p) => {
            let (cfg, warn) = config::load(p, &*creds());
            if let Some(w) = warn {
                log::warn!("{w}");
            }
            cfg
        }
        None => Config::default(),
    };
    // R37 SP9: fold the legacy flat relay/secret pair into server profiles
    // (docs/40 §4.1.2). Writing the migrated shape back is what retires the
    // legacy fields; failure is only a warning — the in-memory shape is
    // already migrated and the write retries on the next settings save.
    if config::migrate(&mut cfg) {
        log::info!("migrated legacy relay settings to server profiles");
        if let Some(p) = &cfg_path
            && let Err(e) = config::save(p, &cfg, &*creds())
        {
            log::warn!("could not save migrated settings: {e}");
        }
    }

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
        health_countdown: 0,
        uplink: gawk_engine::uplink::UplinkMonitor::new(),
        uplink_warned: false,
        room_grant: String::new(),
        room_leaving: false,
    }));

    let ui = MainWindow::new().expect("create window");
    ui.set_app_version(format!("v{}", version::display()).into());
    seed_settings(&ui, &shell.borrow().cfg);
    refresh_captions(&ui, &shell.borrow().cfg);
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

/// The custom (non-default) profiles, in stored order — the combo box lists
/// them after the pinned default at index 0. The default's credentials-only
/// record is not a listed server; its secret shows in the secret field when
/// the default is selected.
fn custom_profiles(cfg: &Config) -> Vec<&ServerProfile> {
    cfg.servers
        .iter()
        .filter(|p| p.name != DEFAULT_SERVER_NAME)
        .collect()
}

fn server_labels(cfg: &Config) -> Vec<SharedString> {
    let mut labels = vec![SharedString::from(format!(
        "Default relay — {}",
        gawk_engine::defaults::RELAY_URL
    ))];
    for p in custom_profiles(cfg) {
        let name = p.name.trim();
        let url = p.url.trim();
        labels.push(
            if name.is_empty() && url.is_empty() {
                "(new server)".to_string()
            } else if name.is_empty() {
                url.to_string()
            } else {
                name.to_string()
            }
            .into(),
        );
    }
    labels
}

/// The combo index of the selected server (0 = default; unknown names fall
/// back to the default, matching `Config::selected_profile`).
fn selected_combo_index(cfg: &Config) -> i32 {
    custom_profiles(cfg)
        .iter()
        .position(|p| p.name == cfg.selected_server)
        .map_or(0, |i| (i + 1) as i32)
}

/// The profile name a combo index selects ("default" = the pinned default).
fn combo_index_to_name(cfg: &Config, index: i32) -> String {
    if index <= 0 {
        return DEFAULT_SERVER_NAME.to_string();
    }
    custom_profiles(cfg)
        .get((index - 1) as usize)
        .map(|p| p.name.clone())
        .unwrap_or_else(|| DEFAULT_SERVER_NAME.to_string())
}

/// Seeds the server dropdown and the per-server fields from the config.
/// Called on load and whenever the selection or the list changes — NOT on
/// every keystroke (rewriting a LineEdit's text mid-edit moves the caret).
fn seed_server_fields(ui: &MainWindow, cfg: &Config) {
    ui.set_server_labels(ModelRc::new(VecModel::from(server_labels(cfg))));
    ui.set_set_server(selected_combo_index(cfg));
    match cfg.selected_profile() {
        Some(p) => {
            ui.set_server_is_custom(true);
            ui.set_set_server_name(p.name.clone().into());
            ui.set_set_relay(p.url.clone().into());
            ui.set_set_secret(p.publish_secret.clone().into());
        }
        None => {
            ui.set_server_is_custom(false);
            ui.set_set_server_name("".into());
            ui.set_set_relay("".into());
            ui.set_set_secret(cfg.resolve_publish_secret().into());
        }
    }
}

fn seed_settings(ui: &MainWindow, cfg: &Config) {
    seed_server_fields(ui, cfg);
    ui.set_room_input(cfg.room.clone().into());
    ui.set_room_attach_key(cfg.room_attach_secret.clone().into());
    ui.set_room_label(cfg.room_label.clone().into());
    ui.set_room_nickname(cfg.nickname.clone().into());
    ui.set_set_app_url(cfg.app_url.clone().into());
    ui.set_set_telemetry(cfg.telemetry_url.clone().into());
    ui.set_set_bitrate(if cfg.bitrate_bps == 0 {
        SharedString::new()
    } else {
        format!("{}", cfg.bitrate_bps as f64 / 1e6).into()
    });
    ui.set_set_resolution(match (cfg.width, cfg.height) {
        (2560, 1440) => 0,
        (0, 0) | (1920, 1080) => 1,
        (1280, 720) => 2,
        (854, 480) => 3,
        // Any other stored size is a custom one; show it in the fields.
        _ => 4,
    });
    if ui.get_set_resolution() == 4 {
        ui.set_set_custom_width(format!("{}", cfg.width).into());
        ui.set_set_custom_height(format!("{}", cfg.height).into());
    }
    ui.set_set_framerate(match cfg.fps {
        120 => 0,
        30 => 2,
        5 => 3,
        _ => 1,
    });
}

/// Reads the settings widgets back into the config: verbatim including
/// blanks — blank means "follow the default", and baking today's default in
/// would pin this user to it forever. The server fields land in the SELECTED
/// profile (R37 SP9); the legacy flat relay/secret pair stays retired after
/// migration.
fn read_settings(ui: &MainWindow, cfg: &mut Config) {
    let secret = ui.get_set_secret().trim().to_string();
    let selected = cfg.selected_server.clone();
    let is_custom = cfg.selected_profile().is_some();
    if is_custom {
        // A rename follows the Linux UpdateCustomServer rule: an empty,
        // reserved, or already-taken new name keeps the old one — the name
        // is the selection key, so a collision would make two profiles
        // indistinguishable. The selection follows the rename.
        let new_name = ui.get_set_server_name().trim().to_string();
        let rename =
            !new_name.is_empty() && new_name != selected && !cfg.profile_name_taken(&new_name);
        let p = cfg
            .servers
            .iter_mut()
            .find(|p| p.name == selected)
            .expect("selected profile exists");
        if rename {
            p.name = new_name.clone();
        }
        p.url = ui.get_set_relay().trim().to_string();
        p.publish_secret = secret;
        if rename {
            cfg.selected_server = new_name;
        }
    } else {
        // The default: the secret edits its credentials-only record (F4 —
        // identity fixed, credential slot editable), keyed to the URL it is
        // saved against (F9). An empty secret removes the record.
        cfg.set_default_secret(&secret);
    }
    // The room card: stored as typed (a pasted link is reduced to its code
    // at dial time, so the user sees what they pasted).
    cfg.room = ui.get_room_input().trim().to_string();
    cfg.room_attach_secret = ui.get_room_attach_key().trim().to_string();
    cfg.room_label = ui.get_room_label().trim().to_string();
    cfg.nickname = ui.get_room_nickname().trim().to_string();
    cfg.app_url = ui.get_set_app_url().trim().to_string();
    cfg.telemetry_url = ui.get_set_telemetry().trim().to_string();
    cfg.bitrate_bps = parse_bitrate_mbps(ui.get_set_bitrate().as_str());
    (cfg.width, cfg.height) = match ui.get_set_resolution() {
        0 => (2560, 1440),
        2 => (1280, 720),
        3 => (854, 480),
        4 => parse_custom_resolution(
            ui.get_set_custom_width().as_str(),
            ui.get_set_custom_height().as_str(),
        ),
        _ => (0, 0), // the default rung stays a blank, not a number
    };
    cfg.fps = match ui.get_set_framerate() {
        0 => 120,
        2 => 30,
        3 => 5,
        _ => 0,
    };
}

/// The custom-resolution parser: both fields must parse to positive
/// integers or the pair falls back to the default rung (0, 0) — a half-
/// typed size must not persist as a mangled rung. Values clamp into
/// [128, 3840] × [128, 2160] (4K is the ceiling) and floor to even (NV12
/// needs even dimensions). The result is a bounding box: the encode
/// resolution is the source aspect fitted inside it (docs/38 D11).
/// The uplink warning line: names the active bitrate so the remedy (lower
/// it) is one thought away.
fn uplink_warning_text(bitrate_bps: u32) -> String {
    format!(
        "Your connection can't keep up with the stream — delivery to the relay is taking too \
         long and viewers may see frozen or delayed video. Lower the peak bitrate in Settings \
         (now {:.0} Mbps), or free up upload bandwidth.",
        f64::from(bitrate_bps) / 1e6
    )
}

fn parse_custom_resolution(w: &str, h: &str) -> (u32, u32) {
    let dim = |s: &str, max: u32| -> Option<u32> {
        match s.trim().parse::<u32>() {
            Ok(v) if v > 0 => Some((v.clamp(128, max)) & !1),
            _ => None,
        }
    };
    match (dim(w, 3840), dim(h, 2160)) {
        (Some(w), Some(h)) => (w, h),
        _ => (0, 0),
    }
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

fn refresh_captions(ui: &MainWindow, cfg: &Config) {
    ui.set_caption_broadcast(format!("Broadcasting to {}", cfg.resolve_relay_url()).into());
    // What is known before dialing: a non-default relay may still advertise
    // an ingest URL in-session (0x12), and the reporter follows it then.
    let opted_out = cfg.telemetry_url.trim().eq_ignore_ascii_case(config::OFF);
    let diag = cfg.resolve_telemetry_url().unwrap_or_else(|| {
        if cfg.selected_profile().is_some() && !opted_out {
            "off unless this server advertises a diagnostics endpoint".into()
        } else {
            "off — nothing is sent".into()
        }
    });
    ui.set_caption_diag(format!("Diagnostics to {diag}").into());
    let app_url = cfg.resolve_app_url();
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
                // The labels model follows name/URL edits live; the full
                // reseed is reserved for selection changes (it would move
                // the caret of the field being typed in).
                ui.set_server_labels(ModelRc::new(VecModel::from(server_labels(&sh.cfg))));
                refresh_captions(&ui, &sh.cfg);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_server_selected(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                // Field edits were already persisted per keystroke, so the
                // old selection's values are safe; just repoint and reseed.
                sh.cfg.selected_server = combo_index_to_name(&sh.cfg, ui.get_set_server());
                save_config(&mut sh);
                seed_server_fields(&ui, &sh.cfg);
                refresh_captions(&ui, &sh.cfg);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_add_server(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let name = sh.cfg.add_custom_server();
                sh.cfg.selected_server = name;
                save_config(&mut sh);
                seed_server_fields(&ui, &sh.cfg);
                refresh_captions(&ui, &sh.cfg);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_remove_server(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let selected = sh.cfg.selected_server.clone();
                if sh.cfg.selected_profile().is_none() {
                    return; // the pinned default is not removable
                }
                sh.cfg.servers.retain(|p| p.name != selected);
                sh.cfg.selected_server = DEFAULT_SERVER_NAME.to_string();
                save_config(&mut sh);
                seed_server_fields(&ui, &sh.cfg);
                refresh_captions(&ui, &sh.cfg);
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
        // R42: join (and attach to) the room in the card while live. The
        // static room's attach key doubles as the room-view grant.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_attach(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                read_settings(&ui, &mut sh.cfg);
                save_config(&mut sh);
                let Some(code) = gawk_engine::parse_room_code(&sh.cfg.room) else {
                    ui.set_room_status("That is not a room code or room link.".into());
                    return;
                };
                let Some(session) = sh.session.clone() else {
                    return;
                };
                let key = sh.cfg.room_attach_secret.clone();
                sh.room_grant = key.clone();
                sh.room_leaving = false;
                drop(sh);
                log::info!("room join requested");
                session.room_join(&code, &key, "");
                ui.set_room_active(true);
                ui.set_room_attached(false);
                ui.set_room_code("".into());
                ui.set_room_link("".into());
                ui.set_room_status("Joining the room…".into());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_new(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let Some(session) = sh.session.clone() else {
                    return;
                };
                sh.room_grant.clear();
                sh.room_leaving = false;
                drop(sh);
                log::info!("room mint requested");
                session.room_create();
                ui.set_room_active(true);
                ui.set_room_attached(false);
                ui.set_room_code("".into());
                ui.set_room_link("".into());
                ui.set_room_status("Creating a room…".into());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_detach(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                sh.room_leaving = true;
                if let Some(session) = sh.session.clone() {
                    session.room_detach();
                }
                ui.set_room_status("Leaving the room…".into());
            }
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
    refresh_captions(ui, &sh.cfg);

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
    sh.health_countdown = 0; // first health line right after going live
    sh.uplink = gawk_engine::uplink::UplinkMonitor::new();
    sh.uplink_warned = false;
    ui.set_uplink_warning("".into());
    sh.last_error.clear();
    sh.first_viewer_seen = false;
    ui.set_error_text("".into());
    ui.set_can_mint(false);
    ui.set_busy(true);
    ui.set_live(false);
    ui.set_state_label("Starting…".into());
    ui.set_copied_note("".into());

    // R42: the configured room is joined from the start; the attach lands
    // once the identity does (the engine's own latch). A pasted link is
    // reduced to its code here; junk is reported instead of dialed.
    let room_code = match (
        sh.cfg.room.trim().is_empty(),
        gawk_engine::parse_room_code(&sh.cfg.room),
    ) {
        (true, _) => String::new(),
        (false, Some(code)) => code,
        (false, None) => {
            ui.set_room_status(
                "The room field is not a room code or room link — not joining.".into(),
            );
            String::new()
        }
    };
    sh.room_grant = if room_code.is_empty() {
        String::new()
    } else {
        sh.cfg.room_attach_secret.clone()
    };
    sh.room_leaving = false;
    reset_room_ui(ui);
    if !room_code.is_empty() {
        ui.set_room_active(true);
        ui.set_room_status("Joining the room…".into());
    }

    let scfg = SessionConfig {
        relay_url: sh.cfg.resolve_relay_url(),
        broadcast_id,
        resume_token_hex: resume_token,
        publish_secret: sh.cfg.resolve_publish_secret(),
        origin: sh.cfg.resolve_origin(),
        insecure: false,
        room_code,
        room_new: false,
        room_attach_secret: sh.cfg.room_attach_secret.clone(),
        room_create_secret: String::new(),
        room_label: sh.cfg.room_label.clone(),
        nickname: sh.cfg.nickname.clone(),
    };
    let clock: Arc<dyn gawk_engine::clock::Clock> = sh.clock.clone();
    let msg_tx = sh.msg_tx.clone();
    let rt_handle = sh.rt.handle().clone();
    // No advertised URL yet — a fresh session starts from the configured
    // resolution; a 0x12 TelemetryEndpoint repoints it when it arrives.
    sh.reporter.set_url(sh.cfg.effective_telemetry_url(None));

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
            // catch_unwind: a panic in the media bring-up must become a
            // visible start failure, not a thread that dies leaving the UI
            // in "Starting…" forever (the panic hook has already logged
            // the payload and location).
            let built = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                pipeline::Pipeline::build(build_params, session.sender(), clock, rt_handle.clone())
            }));
            match built {
                Ok(Ok(p)) => {
                    let _ = msg_tx.send(ShellMsg::Started {
                        session,
                        pipeline: Box::new(p),
                    });
                }
                Ok(Err(f)) => {
                    rt_handle.block_on(session.stop());
                    let _ = msg_tx.send(ShellMsg::StartFailed(f));
                }
                Err(_) => {
                    rt_handle.block_on(session.stop());
                    let _ = msg_tx.send(ShellMsg::StartFailed(StartFailure::Capture(
                        "the media pipeline crashed while starting (a bug — the details are in the debug log)"
                            .into(),
                    )));
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
                log::warn!(
                    "pipeline came up after the session already ended; discarding it (state is no longer Starting)"
                );
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
                let (_, _, fps, _) = sh.cfg.resolve_rung();
                ui.set_encode_line(
                    format!(
                        "Media Foundation — {} · {} · {}×{}@{}",
                        pipeline.info.encoder,
                        pipeline.info.capture_path,
                        pipeline.info.width,
                        pipeline.info.height,
                        fps
                    )
                    .into(),
                );
                sh.encode_info = Some(pipeline.info.clone());
                sh.pipeline = Some(*pipeline);
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
        EngineEvent::TelemetryEndpoint { url } => {
            // R37 §4.10: the fleet that gates collection and mints the token
            // owns the destination — the advertised URL wins over the
            // configured one. The user's "off" still wins over both, and the
            // hello/endpoint arrival order doesn't matter (the reporter
            // adopts a session before its URL resolves).
            let sh = shell.borrow();
            let effective = sh.cfg.effective_telemetry_url(Some(&url));
            log::info!("relay advertised telemetry ingest {url}; reporting to {effective:?}");
            sh.reporter.set_url(effective);
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

        // --- R42 rooms ---
        EngineEvent::RoomState(s) => {
            let sh = shell.borrow();
            let attached = s.has(&sh.broadcast_id);
            let link = gawk_engine::room_link(&sh.cfg.resolve_app_url(), &s.code, &sh.room_grant);
            // Never the code: the HMAC'd key is the log handle.
            log::info!(
                "room state: key {} · {} broadcasts · {} participants · attached {attached}",
                s.key_hex,
                s.attachments.len(),
                s.participants
            );
            drop(sh);
            ui.set_room_active(true);
            ui.set_room_attached(attached);
            ui.set_room_code(s.code.clone().into());
            ui.set_room_link(link.into());
            ui.set_room_status(room_status_text(&s, attached).into());
        }
        EngineEvent::RoomCreated {
            code,
            creator_token_hex,
        } => {
            let mut sh = shell.borrow_mut();
            sh.room_grant = creator_token_hex;
            let link = gawk_engine::room_link(&sh.cfg.resolve_app_url(), &code, &sh.room_grant);
            drop(sh);
            log::info!("room minted");
            ui.set_room_code(code.into());
            ui.set_room_link(link.into());
        }
        EngineEvent::RoomAttached => {
            log::info!("attached to the room");
            ui.set_room_attached(true);
        }
        EngineEvent::RoomDetached { reason } => {
            let mut sh = shell.borrow_mut();
            let left = sh.room_leaving;
            sh.room_leaving = false;
            drop(sh);
            log::info!("detached from the room: {reason}");
            ui.set_room_attached(false);
            if left {
                reset_room_ui(ui);
                ui.set_room_status("Left the room.".into());
            } else {
                ui.set_room_status(format!("Not attached — {reason}.").into());
            }
        }
        EngineEvent::RoomEnded { reason } => {
            log::warn!("room session over: {reason}");
            shell.borrow_mut().room_leaving = false;
            reset_room_ui(ui);
            ui.set_room_status(format!("Room over — {reason}.").into());
        }
        EngineEvent::RoomRejected { reason, message } => {
            log::warn!("room command rejected: {reason} ({message})");
            ui.set_room_status(format!("The relay refused: {reason} ({message}).").into());
        }
        EngineEvent::RoomReconnecting { attempt } => {
            ui.set_room_status(
                if attempt > 1 {
                    format!("Reconnecting to the room… (attempt {attempt})")
                } else {
                    "Reconnecting to the room…".to_string()
                }
                .into(),
            );
        }
    }
}

/// The room card back to "no room session": the inputs stay, the live
/// picture goes.
fn reset_room_ui(ui: &MainWindow) {
    ui.set_room_active(false);
    ui.set_room_attached(false);
    ui.set_room_code("".into());
    ui.set_room_link("".into());
    ui.set_room_status("".into());
}

/// The room status line: what the room holds and whether we are in it.
fn room_status_text(s: &gawk_engine::room::RoomSummary, attached: bool) -> String {
    let n = s.attachments.len();
    let live = s.attachments.iter().filter(|a| a.live).count();
    let broadcasts = match (n, live) {
        (0, _) => "no broadcasts".to_string(),
        (n, l) if n == l => format!("{n} broadcast{}", if n == 1 { "" } else { "s" }),
        (n, l) => format!("{n} broadcasts ({l} live)"),
    };
    let me = if attached {
        "attached"
    } else if s.attach_ok {
        "not attached yet"
    } else {
        "attach not allowed here (attach key?)"
    };
    format!(
        "{}{} · {broadcasts} · {} in the room · {me}",
        if s.creator { "Your room" } else { "In room" },
        if s.display_name.is_empty() {
            String::new()
        } else {
            format!(" “{}”", s.display_name)
        },
        s.participants
    )
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
    // The room session lives as long as the broadcast it attaches; the
    // engine already stopped it.
    sh.room_grant.clear();
    sh.room_leaving = false;
    drop(sh);
    reset_room_ui(ui);

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
    ui.set_uplink_warning("".into());
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
    let log_health;
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
        sh.health_countdown = sh.health_countdown.saturating_sub(1);
        log_health = sh.health_countdown == 0;
        if log_health {
            sh.health_countdown = 60; // one line per minute while live
        }
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

    let st = {
        let sh = shell.borrow();
        let st = merged_stats(&sh);
        sh.reporter.report(st.clone());
        sh.reporter.tick();
        st
    };

    // The upload-bandwidth watchdog (1 Hz): transitions are logged and the
    // warning line follows the hysteresis, not each individual sample.
    {
        let mut sh = shell.borrow_mut();
        let warned = sh.uplink.observe(&st);
        if warned != sh.uplink_warned {
            sh.uplink_warned = warned;
            if warned {
                let (_, _, _, bps) = sh.cfg.resolve_rung();
                let text = uplink_warning_text(bps);
                log::warn!("uplink warning raised: {text}");
                ui.set_uplink_warning(text.into());
            } else {
                log::info!("uplink warning cleared");
                ui.set_uplink_warning("".into());
            }
        }
    }

    // The remainder needs the shell only for the Windows pipeline widgets.
    #[cfg(windows)]
    let sh = shell.borrow();
    if log_health {
        log::info!(
            "health: capture {} fps, encode {:.1} fps, sent {:.1} fps, keyframe streams {} sent / {} superseded / {} failed, frames dropped at send {}, audio {}",
            if st.capture_fps_available {
                format!("{:.1}", st.capture_fps)
            } else {
                "n/a".into()
            },
            st.encoder_fps,
            st.sent_fps,
            st.keyframe_streams_sent,
            st.keyframe_streams_superseded,
            st.keyframe_streams_failed,
            st.frames_dropped_at_send,
            st.audio_state
        );
    }

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
            // The rung reads as what is ACTUALLY encoded (the aspect-fitted
            // dims), not the configured bounding box.
            st.width = info.width;
            st.height = info.height;
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

    #[test]
    fn custom_resolution_clamps_to_4k_and_falls_back_when_half_typed() {
        assert_eq!(parse_custom_resolution("2560", "1080"), (2560, 1080));
        // The 4K ceiling, per axis.
        assert_eq!(parse_custom_resolution("7680", "4320"), (3840, 2160));
        // Odd sizes floor to even (NV12), tiny sizes clamp up.
        assert_eq!(parse_custom_resolution("1281", "721"), (1280, 720));
        assert_eq!(parse_custom_resolution("16", "16"), (128, 128));
        // Half-typed or junk: fall back to the default rung, never persist
        // a mangled pair.
        assert_eq!(parse_custom_resolution("1920", ""), (0, 0));
        assert_eq!(parse_custom_resolution("", ""), (0, 0));
        assert_eq!(parse_custom_resolution("abc", "720"), (0, 0));
        assert_eq!(parse_custom_resolution("0", "720"), (0, 0));
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

    fn cfg_with_two_customs() -> Config {
        Config {
            servers: vec![
                ServerProfile {
                    name: DEFAULT_SERVER_NAME.into(),
                    url: gawk_engine::defaults::RELAY_URL.into(),
                    publish_secret: "default-secret".into(),
                },
                ServerProfile {
                    name: "Juho's homelab".into(),
                    url: "https://relay.example:4433".into(),
                    publish_secret: "s1".into(),
                },
                ServerProfile {
                    name: "  ".into(), // hand-edited file; the GUI never writes this
                    url: "https://other.example:4433".into(),
                    publish_secret: String::new(),
                },
            ],
            ..Default::default()
        }
    }

    // R37 SP9: the combo mapping — index 0 is the pinned default, customs
    // follow in stored order, and the default's credentials-only record is
    // never a listed server.
    #[test]
    fn server_combo_mapping_round_trips() {
        let mut cfg = cfg_with_two_customs();
        let labels = server_labels(&cfg);
        assert_eq!(labels.len(), 3, "default + 2 customs, no credential row");
        assert!(labels[0].starts_with("Default relay"));
        assert_eq!(labels[1], "Juho's homelab");
        // A nameless profile is labelled by its URL, never a blank row.
        assert_eq!(labels[2], "https://other.example:4433");

        assert_eq!(selected_combo_index(&cfg), 0);
        cfg.selected_server = "Juho's homelab".into();
        assert_eq!(selected_combo_index(&cfg), 1);
        // Unknown selection degrades to the default.
        cfg.selected_server = "gone".into();
        assert_eq!(selected_combo_index(&cfg), 0);

        assert_eq!(combo_index_to_name(&cfg, 0), DEFAULT_SERVER_NAME);
        assert_eq!(combo_index_to_name(&cfg, 1), "Juho's homelab");
        assert_eq!(combo_index_to_name(&cfg, 2), "  ");
        // Out-of-range indices name the default, never a panic.
        assert_eq!(combo_index_to_name(&cfg, -1), DEFAULT_SERVER_NAME);
        assert_eq!(combo_index_to_name(&cfg, 99), DEFAULT_SERVER_NAME);
    }

    #[test]
    fn the_engine_dial_follows_the_selected_profile() {
        let mut cfg = cfg_with_two_customs();
        // Default selected: default URL, the credential record's secret.
        assert_eq!(cfg.resolve_relay_url(), gawk_engine::defaults::RELAY_URL);
        assert_eq!(cfg.resolve_publish_secret(), "default-secret");
        // Custom selected: that profile's URL and secret.
        cfg.selected_server = "Juho's homelab".into();
        assert_eq!(cfg.resolve_relay_url(), "https://relay.example:4433");
        assert_eq!(cfg.resolve_publish_secret(), "s1");
    }

    #[test]
    fn room_status_reads_as_a_sentence() {
        use gawk_engine::room::{RoomAttachmentInfo, RoomSummary};
        let mut s = RoomSummary {
            code: "K7XQ2M".into(),
            attach_ok: true,
            participants: 3,
            ..RoomSummary::default()
        };
        assert_eq!(
            room_status_text(&s, false),
            "In room · no broadcasts · 3 in the room · not attached yet"
        );
        s.creator = true;
        s.attachments = vec![
            RoomAttachmentInfo {
                broadcast_id: "K7XQ2M".into(),
                live: true,
                ..RoomAttachmentInfo::default()
            },
            RoomAttachmentInfo {
                broadcast_id: "ABCDEF".into(),
                live: false,
                ..RoomAttachmentInfo::default()
            },
        ];
        assert_eq!(
            room_status_text(&s, true),
            "Your room · 2 broadcasts (1 live) · 3 in the room · attached"
        );
        s.attach_ok = false;
        s.display_name = "LAN party".into();
        assert!(room_status_text(&s, false).starts_with("Your room “LAN party” · "));
        assert!(room_status_text(&s, false).ends_with("attach key?)"));
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
