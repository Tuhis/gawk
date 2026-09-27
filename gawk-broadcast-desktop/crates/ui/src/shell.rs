//! The broadcaster shell both desktop apps run (WB6, docs/38 D12; shared
//! from R52 on, docs/54 D11): one window, the Linux GUI's card architecture;
//! window-is-the-app (closing ends the broadcast; no tray, no background
//! presence).
//!
//! Everything here is platform-neutral — settings, server profiles, rooms,
//! the session lifecycle, the identity latch, stats, diagnostics. A platform
//! plugs in through two seams, and nowhere else:
//!
//! * [`Platform`] — what to capture (its picker), how to notify, where
//!   credentials live, and its own callbacks and ticks;
//! * [`Media`] — the running capture/encode/audio pipeline the platform
//!   builds on Start.
//!
//! Moved verbatim from the Windows shell's `main.rs` when the macOS shell
//! arrived; the `#[cfg(windows)]` sites that used to reach into the Windows
//! pipeline are exactly the calls [`Media`] now carries.
//!
//! Threading model: Slint owns the UI thread; the engine runs on a tokio
//! runtime; media pumps run on their own threads. Everything flows back to
//! the UI through one std mpsc channel drained by a UI timer — no shared
//! state crosses the boundary.

use crate::messages::{StartFailure, can_mint, first_line, message};
use crate::{
    MainWindow, RecentRow, RoomRow, StatRow, debuglog, diagnostics, refresh_captions, version,
};
use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::config::{self, Config, DEFAULT_SERVER_NAME, ServerProfile};
use gawk_engine::room::RoomSummary;
use gawk_engine::sender::Sender;
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use gawk_engine::telemetry::{Hello, Reporter};
use gawk_engine::{RoomGrant, RoomInput, parse_room_input};
use slint::{ComponentHandle, Model, ModelRc, SharedString, VecModel};
use std::any::Any;
use std::cell::RefCell;
use std::rc::Rc;
use std::sync::mpsc;
use std::sync::{Arc, OnceLock};

/// A downscaled RGBA thumbnail: width, height, pixels.
pub type Thumb = (u32, u32, Vec<u8>);

/// What the shell shows about a built pipeline.
#[derive(Clone, Debug)]
pub struct MediaInfo {
    /// The encoder family for the header line: "Media Foundation",
    /// "VideoToolbox".
    pub family: &'static str,
    /// The accepted encoder's stable id — the last-good cache key (D9).
    pub encoder: String,
    pub codec: String,
    pub capture_path: String,
    /// The ACTUAL encode dimensions — the configured rung box fitted to
    /// the source aspect (D11 amendment), not the box itself.
    pub width: u32,
    pub height: u32,
    /// Whether the "what viewers see" thumbnail card shows.
    pub show_thumbnail: bool,
}

/// A running media pipeline, as the shell drives it. Every method is one
/// the Windows shell used to call on its pipeline directly.
pub trait Media: Send {
    fn info(&self) -> &MediaInfo;
    /// Resume re-prime (docs/38 D5): the next frame carries an IDR.
    fn force_idr(&self);
    fn take_thumbnail(&self) -> Option<Thumb>;
    fn capture_fps(&self) -> Option<f64>;
    /// "off" | "unavailable" | "active" | "error"
    fn audio_state(&self) -> String;
    fn audio_level(&self) -> f32;
    fn audio_silence_hint(&self) -> bool;
    /// The one-click switch from per-app to whole-system audio.
    fn switch_audio_to_system(&self);
    /// Whether the shared window is minimized (the GUI hint).
    fn minimized(&self) -> bool;
    /// The capture mode now ("app" | "screen"), when the pipeline can change
    /// it mid-broadcast — macOS re-picks a display for whole-system audio
    /// (docs/54 D6). `None` keeps the mode Start resolved.
    fn capture_mode(&self) -> Option<&'static str> {
        None
    }
    /// A pump died; the broadcast should end with this message.
    fn take_failure(&self) -> Option<String>;
    /// Tears the media down in dependency order. No zombie capture.
    fn shutdown(self: Box<Self>);
    fn as_any(&self) -> &dyn Any;
}

/// What a pipeline builder gets on the start thread.
pub struct MediaEnv {
    pub sender: Arc<Sender>,
    pub clock: Arc<dyn Clock>,
    pub rt: tokio::runtime::Handle,
}

/// Builds the pipeline, off the GUI thread (trial encodes run here).
pub type MediaBuilder = Box<dyn FnOnce(MediaEnv) -> Result<Box<dyn Media>, StartFailure> + Send>;

/// A start the platform has resolved: what is being shared, and how to
/// build the media for it.
pub struct Prepared {
    /// "app" | "screen" — diagnostics and the audio line.
    pub capture_mode: &'static str,
    pub build: MediaBuilder,
}

/// Stateless platform services, reachable from code that holds no shell.
#[derive(Clone, Copy)]
pub struct Hooks {
    pub notify: fn(&str, &str, bool),
    pub creds: fn() -> Box<dyn config::Credentials>,
}

static HOOKS: OnceLock<Hooks> = OnceLock::new();

fn hooks() -> Hooks {
    *HOOKS
        .get()
        .expect("shell::run installs the platform hooks first")
}

/// What a platform supplies to the shared shell.
pub trait Platform: 'static {
    fn hooks(&self) -> Hooks;
    /// After the logger is up: platform facts worth a debug.log line.
    fn launch_log(&self) {}
    /// Before the window shows: the platform's card and initial picker.
    fn init_window(&mut self, ui: &MainWindow);
    /// Start was pressed: resolve what to share, or say why not (the
    /// error card's text). Runs on the GUI thread before the dial, so a
    /// missing selection is an instant, local error.
    fn prepare_start(&mut self, ui: &MainWindow, cfg: &Config) -> Result<Prepared, String>;
    /// Every UI tick (250 ms), for platform events that arrive off-thread.
    fn tick(&mut self, _ui: &MainWindow, _media: Option<&dyn Media>) {}
    fn as_any_mut(&mut self) -> &mut dyn Any;
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum UiState {
    Idle,
    Starting,
    Live,
}

/// Everything background threads report back to the UI.
enum ShellMsg {
    Started {
        session: Arc<Session>,
        media: Box<dyn Media>,
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

pub struct Shell {
    platform: Box<dyn Platform>,
    cfg: Config,
    cfg_path: Option<std::path::PathBuf>,
    /// Where the debug log landed (None when no config dir / init failed);
    /// error cards point at it so refusals are diagnosable from the field.
    log_path: Option<std::path::PathBuf>,
    state: UiState,
    session: Option<Arc<Session>>,
    media: Option<Box<dyn Media>>,
    media_info: Option<MediaInfo>,
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
    stats_countdown: u8,
    /// 1-per-minute keyframe/fps health line into debug.log (F-12: the
    /// supersede livelock was invisible without send-side counters).
    health_countdown: u8,
    /// The upload-bandwidth watchdog, fed 1 Hz; fresh per broadcast.
    uplink: gawk_engine::uplink::UplinkMonitor,
    uplink_warned: bool,
    /// R42: the grant the "Open room view" link carries — the creator token
    /// of a room this session minted, or the static room's attach key. In
    /// memory only: it is a one-broadcast affair.
    room_grant: Option<RoomGrant>,
    /// Set when the user clicked Detach/Leave, so the RoomDetached that
    /// follows is read as "we left" rather than "the creator removed us".
    room_leaving: bool,
    /// Debounces the live rename: `edited` fires per keystroke, and every
    /// SetNickname costs the relay a re-Attach and every participant a
    /// roster event, so the send waits until typing pauses (single-shot,
    /// restarted per edit) or Enter is pressed. Persisting is not debounced.
    nick_timer: slint::Timer,
    /// The last nickname sent to the relay for this session — a flush that
    /// would repeat it is skipped.
    nick_sent: String,
    /// When the broadcast went live: the elapsed clock and the stopped
    /// summary (docs/60 D11).
    live_since: Option<std::time::Instant>,
    /// The most viewers seen during this broadcast.
    peak_viewers: u32,
    /// Bytes sent at the last 1 Hz tick, for the live upload rate.
    last_bytes: u64,
    /// "Create a new room" chosen before going live: the room is minted
    /// once the broadcast has its identity (docs/60 D8). In memory only.
    pending_create: bool,
    /// Set when this app asked the relay to end the room: that end shows no
    /// card (docs/60 D10).
    room_ending_by_me: bool,
    /// The running room session has had a snapshot: its end is a room
    /// ending, not a refused join.
    room_joined: bool,
    /// The latest room picture, for the roster.
    room: Option<RoomSummary>,
    /// The attach key the running room session joined with; remembered with
    /// the room in "Your rooms".
    room_key_used: String,
}

/// Sends the current nickname to the running session unless it is the one
/// already sent. Called from the debounce timer and from Enter.
fn flush_nickname(sh: &mut Shell) {
    let nick = sh.cfg.nickname.clone();
    if nick == sh.nick_sent {
        return;
    }
    if let Some(session) = sh.session.clone() {
        session.room_set_nickname(&nick);
    }
    sh.nick_sent = nick;
}

impl Shell {
    /// The platform, as its concrete type — for the platform's own
    /// callbacks, which captured the shell.
    pub fn platform_mut<T: Platform>(&mut self) -> &mut T {
        self.platform
            .as_any_mut()
            .downcast_mut::<T>()
            .expect("the shell runs the platform it was started with")
    }

    /// The running media pipeline, if live.
    pub fn media(&self) -> Option<&dyn Media> {
        self.media.as_deref()
    }

    /// The platform and the running media together — for a platform
    /// callback that acts on the live pipeline (macOS "Change…" re-picks
    /// for the running capture).
    pub fn platform_and_media<T: Platform>(&mut self) -> (&mut T, Option<&dyn Media>) {
        let media = self.media.as_deref();
        let platform = self
            .platform
            .as_any_mut()
            .downcast_mut::<T>()
            .expect("the shell runs the platform it was started with");
        (platform, media)
    }
}

fn creds() -> Box<dyn config::Credentials> {
    (hooks().creds)()
}

/// Runs the broadcaster window until it closes. `wire_platform` connects
/// the platform's own callbacks (its picker) once the window exists.
pub fn run(
    platform: Box<dyn Platform>,
    wire_platform: impl FnOnce(&MainWindow, &Rc<RefCell<Shell>>),
) {
    let _ = HOOKS.set(platform.hooks());

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
    platform.launch_log();

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
        platform,
        cfg,
        cfg_path,
        log_path,
        state: UiState::Idle,
        session: None,
        media: None,
        media_info: None,
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
        stats_countdown: 0,
        health_countdown: 0,
        uplink: gawk_engine::uplink::UplinkMonitor::new(),
        uplink_warned: false,
        room_grant: None,
        room_leaving: false,
        nick_timer: slint::Timer::default(),
        nick_sent: String::new(),
        live_since: None,
        peak_viewers: 0,
        last_bytes: 0,
        pending_create: false,
        room_ending_by_me: false,
        room_joined: false,
        room: None,
        room_key_used: String::new(),
    }));

    let ui = MainWindow::new().expect("create window");
    ui.set_app_version(format!("v{}", version::display()).into());
    seed_settings(&ui, &shell.borrow().cfg);
    refresh_captions(&ui, &shell.borrow().cfg);
    {
        let sh = shell.borrow();
        let cfg = &sh.cfg;
        ui.set_resume_code(cfg.last_broadcast_id.clone().into());
        ui.set_code_chars(code_chars(&cfg.last_broadcast_id));
        // docs/60 D12: still marked live at launch means the app died live.
        let crashed = cfg.was_live && !cfg.last_broadcast_id.is_empty();
        ui.set_resume_offer(crashed);
        if crashed {
            log::info!("the last broadcast did not end in the app; offering to resume it");
            let (label, _) = pending_room_view(cfg, false);
            ui.set_resume_room(label.into());
        }
    }
    shell.borrow_mut().platform.init_window(&ui);
    apply_default_source(&ui, &shell.borrow().cfg);

    wire_callbacks(&ui, &shell);
    wire_platform(&ui, &shell);

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
                    {
                        let mut sh = shell.borrow_mut();
                        let sh = &mut *sh;
                        sh.platform.tick(&ui, sh.media.as_deref());
                    }
                    tick(&ui, &shell);
                }
            },
        );
    }

    ui.run().expect("run event loop");
    // ⌘Q (macOS) ends the event loop without the close dialog: never leave
    // a capture or a publisher session behind the window.
    shutdown_now(&shell);
}

/// Stops the session (bounded) and the media, for a quit.
fn shutdown_now(shell: &Rc<RefCell<Shell>>) {
    let session = shell.borrow_mut().session.take();
    if let Some(session) = session {
        {
            let sh = shell.borrow();
            let _ = sh.rt.block_on(async {
                tokio::time::timeout(std::time::Duration::from_secs(3), session.stop()).await
            });
        }
        // Stopped by the app, so not a crash: no resume question at the
        // next launch (docs/60 D12).
        let mut sh = shell.borrow_mut();
        sh.cfg.was_live = false;
        save_config(&mut sh);
    }
    if let Some(m) = shell.borrow_mut().media.take() {
        m.shutdown();
    }
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
    let mut labels = vec![SharedString::from("Official server")];
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

/// The detail line under each server in Settings: its relay address.
fn server_urls(cfg: &Config) -> Vec<SharedString> {
    let mut urls = vec![SharedString::from(gawk_engine::defaults::RELAY_URL)];
    for p in custom_profiles(cfg) {
        urls.push(
            if p.url.trim().is_empty() {
                "No address yet".to_string()
            } else {
                p.url.trim().to_string()
            }
            .into(),
        );
    }
    urls
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
    ui.set_server_urls(ModelRc::new(VecModel::from(server_urls(cfg))));
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
    refresh_ready(ui, cfg, false);
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
    // The room itself is chosen by explicit actions (the room sheet), not
    // read back from a field here.
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
        "Your upload can't keep up with the stream, so people watching may see frozen or \
         delayed video. Lower the upload cap (now {:.0} Mbps) for the next broadcast, or free \
         up upload bandwidth.",
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

/// Mbps for display: "12", "2.5".
fn fmt_mbps(bps: u32) -> String {
    let mbps = f64::from(bps) / 1e6;
    if (mbps - mbps.round()).abs() < 0.05 {
        format!("{:.0}", mbps)
    } else {
        format!("{:.1}", mbps)
    }
}

/// The Ready page's Quality row (docs/60 D4): what the next broadcast sends.
fn quality_line(cfg: &Config) -> String {
    let (w, h, fps, bps) = cfg.resolve_rung();
    let size = match (w, h) {
        (2560, 1440) => "1440p".to_string(),
        (1920, 1080) => "1080p".to_string(),
        (1280, 720) => "720p".to_string(),
        (854, 480) => "480p".to_string(),
        (w, h) => format!("up to {w}×{h}"),
    };
    format!("{size} · {fps} fps · up to {} Mbps", fmt_mbps(bps))
}

/// The host of a URL, for display: `https://gawk.ioio.fi/x` → `gawk.ioio.fi`.
fn host_of(url: &str) -> String {
    let rest = url.trim().split_once("://").map_or(url.trim(), |(_, r)| r);
    rest.split(['/', '?', '#']).next().unwrap_or("").to_string()
}

/// The header's server pill: the official site's name, or the selected
/// custom server's (docs/60 D6).
fn server_pill(cfg: &Config) -> String {
    match cfg.selected_profile() {
        Some(p) if !p.name.trim().is_empty() => p.name.trim().to_string(),
        Some(p) => host_of(&p.url),
        None => host_of(gawk_engine::defaults::APP_URL),
    }
}

/// The pending room's detail line when "Create a new room" was chosen
/// before going live. The window compares against this literal to label
/// the Go live button.
const PENDING_CREATE_DETAIL: &str = "Made when you go live";

/// The Ready page's Room row (docs/60 D8): the room the next broadcast
/// joins or creates, as (label, detail). Empty = none.
fn pending_room_view(cfg: &Config, pending_create: bool) -> (String, String) {
    if pending_create {
        return ("New room".into(), PENDING_CREATE_DETAIL.into());
    }
    match parse_room_input(&cfg.room) {
        Some(RoomInput { code, .. }) => (code, "Joins when you go live".into()),
        None => (String::new(), String::new()),
    }
}

fn now_unix() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

/// "today", "yesterday", "3 days ago".
fn ago(now: u64, then: u64) -> String {
    if then == 0 || then > now {
        return "recently".into();
    }
    match (now - then) / 86_400 {
        0 => "today".into(),
        1 => "yesterday".into(),
        d if d < 30 => format!("{d} days ago"),
        _ => "over a month ago".into(),
    }
}

/// "Your rooms" (docs/60 D8): saved rooms first, each group most recent
/// first (the config keeps that order).
fn recent_rows(cfg: &Config, now: u64) -> Vec<RecentRow> {
    let row = |r: &config::RecentRoom| RecentRow {
        code: r.code.clone().into(),
        detail: if r.saved {
            format!("Saved · last joined {}", ago(now, r.last_joined))
        } else {
            format!("Last joined {}", ago(now, r.last_joined))
        }
        .into(),
        saved: r.saved,
    };
    let saved = cfg.recent_rooms.iter().filter(|r| r.saved).map(row);
    let others = cfg.recent_rooms.iter().filter(|r| !r.saved).map(row);
    saved.chain(others).collect()
}

/// What the room field was read as, and whether Join may go (docs/60 D8):
/// a pasted link is echoed so the user sees its key was picked up.
fn room_input_echo(input: &str) -> (String, bool) {
    if input.trim().is_empty() {
        return (String::new(), false);
    }
    match parse_room_input(input) {
        None => ("That isn't a room code or a room link.".into(), false),
        Some(RoomInput {
            code,
            grant: Some(RoomGrant::Attach(_)),
        }) => (format!("Room {code} · key included"), true),
        Some(RoomInput {
            code,
            grant: Some(RoomGrant::Creator(_)),
        }) => (format!("Room {code} · you made this room"), true),
        Some(RoomInput { code, grant: None }) if input.contains("#/room/") => {
            (format!("Room {code}"), true)
        }
        Some(_) => (String::new(), true),
    }
}

/// The Ready page's derived lines: quality, server, the pending room and
/// "Your rooms".
fn refresh_ready(ui: &MainWindow, cfg: &Config, pending_create: bool) {
    ui.set_quality_line(quality_line(cfg).into());
    ui.set_server_pill(server_pill(cfg).into());
    let (label, detail) = pending_room_view(cfg, pending_create);
    ui.set_room_pending(label.into());
    ui.set_room_pending_detail(detail.into());
    ui.set_recent_rooms(ModelRc::new(VecModel::from(recent_rows(cfg, now_unix()))));
}

/// A stable avatar colour index for a name.
fn tint_for(name: &str) -> i32 {
    (name.bytes().map(u32::from).sum::<u32>() % 6) as i32
}

fn initial_of(name: &str) -> String {
    name.chars()
        .next()
        .map(|c| c.to_uppercase().collect())
        .unwrap_or_else(|| "?".into())
}

/// The in-app roster (docs/60 D9).
struct RosterView {
    rows: Vec<RoomRow>,
    watchers: String,
    watcher_initials: Vec<SharedString>,
    streaming: i32,
    watching: i32,
}

/// Builds the roster: streams first (ours, then live, then away), then a
/// line naming who is only watching. `our_id` is this broadcast's code;
/// `creator` lets the rows offer Remove.
fn roster_view(s: &RoomSummary, our_id: &str, app_url: &str, creator: bool) -> RosterView {
    let rank = |a: &gawk_engine::room::RoomAttachmentInfo| {
        if a.broadcast_id == our_id {
            0
        } else if a.live {
            1
        } else {
            2
        }
    };
    let mut streams: Vec<_> = s.attachments.iter().collect();
    streams.sort_by_key(|a| rank(a));
    let rows = streams
        .into_iter()
        .map(|a| {
            let ours = a.broadcast_id == our_id;
            let name = if a.label.trim().is_empty() {
                "Unnamed stream".to_string()
            } else {
                a.label.trim().to_string()
            };
            let detail = if !a.live {
                "Away · their stream is paused".to_string()
            } else {
                match a.viewer_count {
                    0 => "Streaming".to_string(),
                    1 => "Streaming · 1 watching".to_string(),
                    n => format!("Streaming · {n} watching"),
                }
            };
            let speaking = s
                .people
                .iter()
                .any(|p| p.speaking && p.streaming && p.nickname == a.label);
            RoomRow {
                initial: initial_of(&name).into(),
                tint: tint_for(&name),
                name: name.into(),
                detail: detail.into(),
                you: ours,
                away: !a.live,
                speaking,
                broadcast_id: a.broadcast_id.clone().into(),
                watch_link: if ours {
                    SharedString::new()
                } else {
                    gawk_engine::join_link(app_url, &a.broadcast_id).into()
                },
                removable: creator && !ours,
            }
        })
        .collect();

    let names: Vec<String> = s
        .people
        .iter()
        .filter(|p| !p.streaming && p.id != s.your_id)
        .map(|p| {
            if p.nickname.trim().is_empty() {
                "Someone".to_string()
            } else {
                p.nickname.trim().to_string()
            }
        })
        .collect();
    let watchers = match names.as_slice() {
        [] => String::new(),
        [a] => format!("{a} is watching"),
        [a, b] => format!("{a} and {b} are watching"),
        [a, b, c] => format!("{a}, {b} and {c} are watching"),
        [a, b, c, rest @ ..] => format!("{a}, {b}, {c} and {} more are watching", rest.len()),
    };
    let mut watcher_initials: Vec<SharedString> =
        names.iter().take(3).map(|n| initial_of(n).into()).collect();
    if names.len() > 3 {
        watcher_initials.push(format!("+{}", names.len() - 3).into());
    }
    RosterView {
        rows,
        watchers,
        watcher_initials,
        streaming: s.attachments.len() as i32,
        watching: names.len() as i32,
    }
}

/// A gated static room admitted us as a watcher: the app asks for its key
/// in place (docs/60 D8).
fn room_needs_key(s: &RoomSummary, attached: bool) -> bool {
    !s.attach_ok && !s.dynamic && !attached
}

/// The live clock: "4:07", "1:02:03".
fn format_elapsed(secs: u64) -> String {
    let (h, m, s) = (secs / 3600, (secs / 60) % 60, secs % 60);
    if h > 0 {
        format!("{h}:{m:02}:{s:02}")
    } else {
        format!("{m}:{s:02}")
    }
}

/// "42 seconds", "1 minute", "42 minutes", "1 h 12 min".
fn format_duration(secs: u64) -> String {
    match secs {
        0..=59 => format!("{secs} second{}", if secs == 1 { "" } else { "s" }),
        60..=3599 => {
            let m = secs / 60;
            format!("{m} minute{}", if m == 1 { "" } else { "s" })
        }
        _ => format!("{} h {} min", secs / 3600, (secs / 60) % 60),
    }
}

/// The stopped summary's rows (docs/60 D11).
fn summary_rows(peak: u32, bytes: u64, secs: u64, w: u32, h: u32, fps: u32) -> Vec<StatRow> {
    let row = |label: &str, value: String| StatRow {
        label: label.into(),
        value: value.into(),
    };
    let avg = if secs == 0 {
        "n/a".to_string()
    } else {
        format!("{:.1} Mbps", bytes as f64 * 8.0 / secs as f64 / 1e6)
    };
    vec![
        row("Most people watching", peak.to_string()),
        row("Average upload", avg),
        row("Sent", format!("{w}×{h} · {fps} fps")),
    ]
}

/// A code as its characters, for the six boxes.
fn code_chars(code: &str) -> ModelRc<SharedString> {
    let chars: Vec<SharedString> = code.chars().map(|c| c.to_string().into()).collect();
    ModelRc::new(VecModel::from(chars))
}

/// A stable key for a Windows source (docs/60 D5): `display:<label>` or
/// `window:<title>`.
fn source_key(tab: i32, name: &str) -> String {
    if tab == 1 {
        format!("display:{name}")
    } else {
        format!("window:{name}")
    }
}

/// Which source to select, as (tab, index): the remembered one when it is
/// still listed, else the first display. `None` when nothing is listed.
fn default_source(windows: &[String], monitors: &[String], last: &str) -> Option<(i32, usize)> {
    if let Some(title) = last.strip_prefix("window:")
        && let Some(i) = windows.iter().position(|w| w == title)
    {
        return Some((0, i));
    }
    if let Some(label) = last.strip_prefix("display:")
        && let Some(i) = monitors.iter().position(|m| m == label)
    {
        return Some((1, i));
    }
    (!monitors.is_empty()).then_some((1, 0))
}

/// Selects the default source in the Windows picker (docs/60 D5). The
/// macOS window lists nothing itself, so this is a no-op there.
fn apply_default_source(ui: &MainWindow, cfg: &Config) {
    if ui.get_system_picker() {
        return;
    }
    let windows: Vec<String> = ui
        .get_windows()
        .iter()
        .map(|w| w.title.to_string())
        .collect();
    let monitors: Vec<String> = ui
        .get_monitors()
        .iter()
        .map(|m| m.label.to_string())
        .collect();
    match default_source(&windows, &monitors, &cfg.last_source) {
        Some((0, i)) => {
            ui.set_picker_tab(0);
            ui.set_selected_window(i as i32);
            ui.set_selected_monitor(-1);
        }
        Some((_, i)) => {
            ui.set_picker_tab(1);
            ui.set_selected_monitor(i as i32);
            ui.set_selected_window(-1);
        }
        None => {
            ui.set_selected_window(-1);
            ui.set_selected_monitor(-1);
        }
    }
}

/// The current picker selection's key, or `None` when nothing is selected.
fn current_source_key(ui: &MainWindow) -> Option<String> {
    let tab = ui.get_picker_tab();
    if tab == 0 {
        let i = usize::try_from(ui.get_selected_window()).ok()?;
        ui.get_windows()
            .row_data(i)
            .map(|w| source_key(0, w.title.as_str()))
    } else {
        let i = usize::try_from(ui.get_selected_monitor()).ok()?;
        ui.get_monitors()
            .row_data(i)
            .map(|m| source_key(1, m.label.as_str()))
    }
}

/// Re-selects after the platform refreshed the window list (its indices
/// moved): the remembered source, else the first display.
pub fn reselect_source(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    apply_default_source(ui, &shell.borrow().cfg);
}

fn save_config(shell: &mut Shell) {
    if let Some(path) = shell.cfg_path.clone()
        && let Err(e) = config::save(&path, &shell.cfg, &*creds())
    {
        log::warn!("could not save settings: {e}");
    }
}

/// An RGBA buffer as a Slint image (thumbnails, window icons).
pub fn rgba_image(w: u32, h: u32, rgba: &[u8]) -> slint::Image {
    let mut buf = slint::SharedPixelBuffer::<slint::Rgba8Pixel>::new(w, h);
    buf.make_mut_bytes().copy_from_slice(rgba);
    slint::Image::from_rgba8(buf)
}

fn wire_callbacks(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let ui_weak = ui.as_weak();

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
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
            }
        });
    }
    {
        // The nickname persists like every other setting and, while a
        // session exists, renames on the relay too (the tile label follows
        // — one name feeds both, as on the web broadcaster page).
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_nickname_edited(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                read_settings(&ui, &mut sh.cfg);
                save_config(&mut sh);
                let flush_shell = shell.clone();
                sh.nick_timer.start(
                    slint::TimerMode::SingleShot,
                    std::time::Duration::from_millis(600),
                    move || flush_nickname(&mut flush_shell.borrow_mut()),
                );
            }
        });
    }
    {
        let shell = shell.clone();
        ui.on_room_nickname_accepted(move || {
            let mut sh = shell.borrow_mut();
            sh.nick_timer.stop();
            flush_nickname(&mut sh);
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
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
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
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
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
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
            }
        });
    }
    {
        let shell = shell.clone();
        ui.on_switch_to_system_audio(move || {
            if let Some(m) = shell.borrow().media() {
                m.switch_audio_to_system();
            }
        });
    }
    {
        // The room sheet's field: echo what it reads as (docs/60 D8).
        let ui_weak = ui_weak.clone();
        ui.on_room_input_edited(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let (echo, ok) = room_input_echo(ui.get_room_input().as_str());
                ui.set_room_input_echo(echo.into());
                ui.set_room_input_ok(ok);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_join(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let raw = ui.get_room_input().to_string();
                match parse_room_input(&raw) {
                    Some(input) => choose_room(&ui, &shell, &raw, input),
                    None => {
                        ui.set_room_input_echo("That isn't a room code or a room link.".into());
                        ui.set_room_input_ok(false);
                    }
                }
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_join_recent(move |code| {
            if let Some(ui) = ui_weak.upgrade() {
                let code = code.to_string();
                let input = RoomInput {
                    code: code.clone(),
                    grant: None,
                };
                choose_room(&ui, &shell, &code, input);
            }
        });
    }
    {
        // Idle, "Create a new room" is a pending create that mints once the
        // broadcast has its identity; live, it mints now (docs/60 D8).
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_create(move || {
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_show_room_sheet(false);
                let mut sh = shell.borrow_mut();
                let Some(session) = sh.session.clone() else {
                    sh.pending_create = true;
                    sh.cfg.room.clear();
                    sh.cfg.room_attach_secret.clear();
                    save_config(&mut sh);
                    refresh_ready(&ui, &sh.cfg, true);
                    return;
                };
                sh.room_grant = None;
                begin_room_session(&mut sh, "");
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
        ui.on_room_toggle_saved(move |code| {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let saved = sh
                    .cfg
                    .recent_rooms
                    .iter()
                    .any(|r| r.saved && r.code.eq_ignore_ascii_case(code.as_str()));
                sh.cfg.set_room_saved(code.as_str(), !saved);
                save_config(&mut sh);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_pending_dismiss(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                sh.pending_create = false;
                sh.cfg.room.clear();
                sh.cfg.room_attach_secret.clear();
                save_config(&mut sh);
                refresh_ready(&ui, &sh.cfg, false);
            }
        });
    }
    {
        // Leave room: detach and leave, and the next broadcast joins no room.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_detach(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                sh.room_leaving = true;
                sh.cfg.room.clear();
                sh.cfg.room_attach_secret.clear();
                save_config(&mut sh);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
                if let Some(session) = sh.session.clone() {
                    session.room_detach();
                }
                ui.set_room_status("Leaving the room…".into());
            }
        });
    }
    {
        let shell = shell.clone();
        ui.on_room_remove(move |broadcast_id| {
            if let Some(session) = shell.borrow().session.clone() {
                log::info!("removing another stream from the room");
                session.room_remove(broadcast_id.as_str());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_end(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let Some(session) = sh.session.clone() else {
                    return;
                };
                sh.room_ending_by_me = true;
                drop(sh);
                log::info!("ending the room");
                session.room_end();
                ui.set_room_status("Ending the room…".into());
            }
        });
    }
    {
        // A gated static room admitted us as a watcher: rejoin with the key
        // the user typed, and keep it with the room (docs/60 D8).
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_key_submit(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let key = ui.get_room_key_input().trim().to_string();
                if key.is_empty() {
                    return;
                }
                let mut sh = shell.borrow_mut();
                let Some(session) = sh.session.clone() else {
                    return;
                };
                let code = match &sh.room {
                    Some(s) => s.code.clone(),
                    None => ui.get_room_code().to_string(),
                };
                if code.is_empty() {
                    return;
                }
                sh.cfg.room_attach_secret = key.clone();
                sh.cfg.remember_room(&code, &key, now_unix());
                save_config(&mut sh);
                sh.room_grant = attach_grant(&key);
                begin_room_session(&mut sh, &key);
                drop(sh);
                log::info!("rejoining the room with an attach key");
                session.room_join(&code, &key, "");
                ui.set_room_key_input("".into());
                ui.set_room_needs_key(false);
                ui.set_room_status("Joining with the key…".into());
            }
        });
    }
    {
        let ui_weak = ui_weak.clone();
        ui.on_room_card_dismiss(move || {
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_room_card_title("".into());
                ui.set_room_card_body("".into());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_copy_room_link(move || {
            if let Some(ui) = ui_weak.upgrade() {
                // The link friends get carries no grant: the creator token
                // and an attach key are this broadcaster's own.
                let sh = shell.borrow();
                let code = match &sh.room {
                    Some(s) => s.code.clone(),
                    None => ui.get_room_code().to_string(),
                };
                if code.is_empty() {
                    return;
                }
                copy_text(&gawk_engine::room_link(
                    &sh.cfg.resolve_app_url(),
                    &code,
                    None,
                ));
                ui.set_copied_note("Room link copied".into());
            }
        });
    }
    {
        let ui_weak = ui_weak.clone();
        ui.on_summary_dismiss(move || {
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_summary_visible(false);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_resume_dismiss(move || {
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_resume_offer(false);
                let mut sh = shell.borrow_mut();
                sh.cfg.was_live = false;
                save_config(&mut sh);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_source_picked(move || {
            if let Some(ui) = ui_weak.upgrade()
                && let Some(key) = current_source_key(&ui)
            {
                let mut sh = shell.borrow_mut();
                sh.cfg.last_source = key;
                save_config(&mut sh);
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
            shutdown_now(&shell);
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

    // The platform decides mode + target BEFORE the session dial, so a
    // missing selection is an instant, local error.
    let prepared = {
        let sh = &mut *sh;
        match sh.platform.prepare_start(ui, &sh.cfg) {
            Ok(p) => p,
            Err(text) => {
                ui.set_error_text(text.into());
                return;
            }
        }
    };
    sh.capture_mode = prepared.capture_mode;
    // docs/60 D5: the source this broadcast shares is the one to preselect
    // next time.
    if let Some(key) = current_source_key(ui) {
        sh.cfg.last_source = key;
        save_config(&mut sh);
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
    ui.set_summary_visible(false);
    ui.set_resume_offer(false);
    if !resume {
        // A mint's code arrives with the announce; until then the last
        // broadcast's code must not show (or be copied) as this one's.
        ui.set_code("".into());
        ui.set_join_link("".into());
    }
    ui.set_room_card_title("".into());
    ui.set_room_card_body("".into());
    ui.set_live_elapsed("".into());
    ui.set_watching(0);
    ui.set_watching_known(false);
    ui.set_connection_line("".into());
    ui.set_connection_ok(true);
    sh.live_since = None;
    sh.peak_viewers = 0;
    sh.last_bytes = 0;

    // R42: the pending room is joined (or, docs/60 D8, created) from the
    // start; the attach lands once the identity does (the engine's own
    // latch). A pasted link is reduced to its code, and its grant used: an
    // attach key joins with it, a creator token rejoins as creator.
    let room = if sh.pending_create {
        None
    } else {
        parse_room_input(&sh.cfg.room)
    };
    let room_new = sh.pending_create;
    // A create is one-shot: the next broadcast mints no second room.
    sh.pending_create = false;
    let (room_code, attach, creator) = match &room {
        Some(RoomInput { code, grant }) => {
            let attach = match grant {
                Some(RoomGrant::Attach(k)) => k.clone(),
                _ => sh.cfg.room_attach_secret.clone(),
            };
            let creator = match grant {
                Some(RoomGrant::Creator(hex)) => hex.clone(),
                _ => String::new(),
            };
            (code.clone(), attach, creator)
        }
        None => (String::new(), String::new(), String::new()),
    };
    sh.room_grant = if !creator.is_empty() {
        Some(RoomGrant::Creator(creator.clone()))
    } else if room_code.is_empty() {
        None
    } else {
        attach_grant(&attach)
    };
    begin_room_session(&mut sh, &attach);
    reset_room_ui(ui);
    if room_new || !room_code.is_empty() {
        ui.set_room_active(true);
        ui.set_room_code(room_code.clone().into());
        ui.set_room_status(
            if room_new {
                "Creating a room…"
            } else {
                "Joining the room…"
            }
            .into(),
        );
    }
    // The session joins a plain code itself; a creator grant is presented
    // through room_join once the session exists.
    let late_join = (!creator.is_empty()).then(|| (room_code.clone(), attach.clone(), creator));

    let scfg = SessionConfig {
        relay_url: sh.cfg.resolve_relay_url(),
        broadcast_id,
        resume_token_hex: resume_token,
        publish_secret: sh.cfg.resolve_publish_secret(),
        origin: sh.cfg.resolve_origin(),
        insecure: false,
        room_code: if late_join.is_some() {
            String::new()
        } else {
            room_code
        },
        room_new,
        room_attach_secret: attach,
        room_create_secret: String::new(),
        nickname: sh.cfg.nickname.clone(),
    };
    sh.nick_sent = sh.cfg.nickname.clone();
    let clock: Arc<dyn gawk_engine::clock::Clock> = sh.clock.clone();
    let msg_tx = sh.msg_tx.clone();
    let rt_handle = sh.rt.handle().clone();
    // No advertised URL yet — a fresh session starts from the configured
    // resolution; a 0x12 TelemetryEndpoint repoints it when it arrives.
    sh.reporter.set_url(sh.cfg.effective_telemetry_url(None));

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

        if let Some((code, attach, creator)) = &late_join {
            session.room_join(code, attach, creator);
        }

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
        //
        // catch_unwind: a panic in the media bring-up must become a visible
        // start failure, not a thread that dies leaving the UI in
        // "Starting…" forever (the panic hook has already logged the
        // payload and location).
        let env = MediaEnv {
            sender: session.sender(),
            clock,
            rt: rt_handle.clone(),
        };
        let built =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| (prepared.build)(env)));
        match built {
            Ok(Ok(media)) => {
                let _ = msg_tx.send(ShellMsg::Started { session, media });
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
        ShellMsg::Started { session, media } => {
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
                media.shutdown();
                sh.rt.spawn(async move { session.stop().await });
                return;
            }
            sh.session = Some(session);
            {
                let info = media.info().clone();
                // Cache the accepted encoder for next launch (D9).
                if sh.cfg.last_good_encoder != info.encoder {
                    sh.cfg.last_good_encoder = info.encoder.clone();
                    save_config(&mut sh);
                }
                let (_, _, fps, _) = sh.cfg.resolve_rung();
                ui.set_encode_line(
                    format!(
                        "{} — {} · {} · {}×{}@{}",
                        info.family, info.encoder, info.capture_path, info.width, info.height, fps
                    )
                    .into(),
                );
                // The cascade accepts hardware encoders only (docs/38 G3).
                ui.set_encode_badge(format!("{}p{fps} · H.264 hardware", info.height).into());
                ui.set_show_thumbnail(info.show_thumbnail);
                sh.media_info = Some(info);
                sh.media = Some(media);
            }
            sh.state = UiState::Live;
            sh.live_since = Some(std::time::Instant::now());
            // docs/60 D12: set while live; a launch that still finds it set
            // means the app died live.
            sh.cfg.was_live = true;
            save_config(&mut sh);
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
            ui.set_code_chars(code_chars(&broadcast_id));
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
            sh.peak_viewers = sh.peak_viewers.max(n);
            ui.set_watching(n as i32);
            ui.set_watching_known(true);
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
            if let Some(m) = sh.media() {
                // Re-prime the relay's invalidated keyframe cache NOW
                // instead of waiting out the GOP (docs/38 D5).
                m.force_idr();
            }
            drop(sh);
            ui.set_resuming(false);
            ui.set_status_line("".into());
        }
        EngineEvent::Ended { error } => end_broadcast(ui, shell, error),

        // --- R42 rooms ---
        EngineEvent::RoomState(s) => {
            let mut sh = shell.borrow_mut();
            let attached = s.has(&sh.broadcast_id);
            let app_url = sh.cfg.resolve_app_url();
            let link = gawk_engine::room_link(&app_url, &s.code, sh.room_grant.as_ref());
            // Never the code: the HMAC'd key is the log handle.
            log::info!(
                "room state: key {} · {} broadcasts · {} participants · attached {attached}",
                s.key_hex,
                s.attachments.len(),
                s.participants
            );
            if !sh.room_joined {
                // The first snapshot: the join worked, so it goes in "Your
                // rooms" (docs/60 D8).
                sh.room_joined = true;
                let key = sh.room_key_used.clone();
                sh.cfg.remember_room(&s.code, &key, now_unix());
                save_config(&mut sh);
                refresh_ready(ui, &sh.cfg, sh.pending_create);
            }
            let roster = roster_view(&s, &sh.broadcast_id, &app_url, s.creator);
            ui.set_room_active(true);
            ui.set_room_attached(attached);
            ui.set_room_code(s.code.clone().into());
            ui.set_room_link(link.into());
            ui.set_room_status("".into());
            ui.set_room_creator(s.creator);
            ui.set_room_streaming(roster.streaming);
            ui.set_room_watching(roster.watching);
            ui.set_room_rows(ModelRc::new(VecModel::from(roster.rows)));
            ui.set_room_watchers(roster.watchers.into());
            ui.set_room_watcher_initials(ModelRc::new(VecModel::from(roster.watcher_initials)));
            ui.set_room_needs_key(room_needs_key(&s, attached));
            sh.room = Some(s);
        }
        EngineEvent::RoomCreated {
            code,
            creator_token_hex,
        } => {
            let mut sh = shell.borrow_mut();
            sh.room_grant = Some(RoomGrant::Creator(creator_token_hex));
            let link =
                gawk_engine::room_link(&sh.cfg.resolve_app_url(), &code, sh.room_grant.as_ref());
            drop(sh);
            log::info!("room minted");
            ui.set_room_code(code.into());
            ui.set_room_link(link.into());
        }
        EngineEvent::RoomAttached => {
            log::info!("attached to the room");
            ui.set_room_attached(true);
            ui.set_room_needs_key(false);
        }
        EngineEvent::RoomDetached { reason, by_creator } => {
            let mut sh = shell.borrow_mut();
            let left = sh.room_leaving;
            sh.room_leaving = false;
            log::info!("detached from the room: {reason}");
            ui.set_room_attached(false);
            if left {
                sh.room = None;
                drop(sh);
                reset_room_ui(ui);
            } else if by_creator {
                // docs/60 D10: a card, and out of the room — a broadcaster is
                // not left in a room its stream is no longer part of.
                let code = sh.room.take().map(|s| s.code).unwrap_or_default();
                if let Some(session) = sh.session.clone() {
                    session.room_leave();
                }
                forget_pending_room(&mut sh, &code);
                refresh_ready(ui, &sh.cfg, sh.pending_create);
                drop(sh);
                reset_room_ui(ui);
                let (title, body) = removed_card(&code);
                ui.set_room_card_title(title.into());
                ui.set_room_card_body(body.into());
            } else {
                drop(sh);
                ui.set_room_status(format!("Your stream isn't in the room: {reason}.").into());
            }
        }
        EngineEvent::RoomEnded { reason } => {
            let mut sh = shell.borrow_mut();
            log::warn!("room session over: {reason}");
            let by_me = sh.room_ending_by_me;
            let joined = sh.room_joined;
            let live = sh.state == UiState::Live;
            let code = sh
                .room
                .take()
                .map(|s| s.code)
                .unwrap_or_else(|| ui.get_room_code().to_string());
            sh.room_leaving = false;
            sh.room_ending_by_me = false;
            sh.room_joined = false;
            if joined {
                // A room that ended can't be rejoined: don't dial it again
                // on the next go-live.
                forget_pending_room(&mut sh, &code);
                refresh_ready(ui, &sh.cfg, sh.pending_create);
            }
            drop(sh);
            reset_room_ui(ui);
            match room_end_view(by_me, joined, live, &code, &reason) {
                RoomEndView::Quiet => {}
                RoomEndView::Status(text) => ui.set_room_status(text.into()),
                RoomEndView::Card(title, body) => {
                    ui.set_room_card_title(title.into());
                    ui.set_room_card_body(body.into());
                }
            }
        }
        EngineEvent::RoomRejected { reason, message } => {
            log::warn!("room command rejected: {reason} ({message})");
            let mut sh = shell.borrow_mut();
            // An EndRoom the relay refused leaves the room up.
            sh.room_ending_by_me = false;
            drop(sh);
            ui.set_room_status(format!("The server refused: {reason}.").into());
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

/// Clears the pending room when it is `code`: the room is gone for us.
fn forget_pending_room(sh: &mut Shell, code: &str) {
    let pending = parse_room_input(&sh.cfg.room).map(|r| r.code);
    if !code.is_empty() && pending.is_some_and(|p| p.eq_ignore_ascii_case(code)) {
        sh.cfg.room.clear();
        sh.cfg.room_attach_secret.clear();
        save_config(sh);
    }
}

/// How a room session's end shows (docs/60 D10).
#[derive(Debug, PartialEq, Eq)]
enum RoomEndView {
    /// We ended it ourselves: back to "Not in a room", no card.
    Quiet,
    /// The join never worked, or we are not live: one status line.
    Status(String),
    /// Someone else ended a room we were in, while live: a card.
    Card(String, String),
}

fn sentence(s: &str) -> String {
    let mut c = s.chars();
    let first: String = c
        .next()
        .map(|f| f.to_uppercase().collect())
        .unwrap_or_default();
    let mut out = first + c.as_str();
    if !out.ends_with('.') {
        out.push('.');
    }
    out
}

fn room_end_view(by_me: bool, joined: bool, live: bool, code: &str, reason: &str) -> RoomEndView {
    if by_me {
        return RoomEndView::Quiet;
    }
    if !joined {
        return RoomEndView::Status(format!("Couldn't join the room: {reason}."));
    }
    if !live {
        return RoomEndView::Status(sentence(reason));
    }
    let title = if code.is_empty() {
        "The room is over".to_string()
    } else {
        format!("Room {code} is over")
    };
    RoomEndView::Card(
        title,
        format!(
            "{} Your stream is still live on its own code, and people watching through it are still watching.",
            sentence(reason)
        ),
    )
}

/// The card for our stream being removed by the room's creator (D10).
fn removed_card(code: &str) -> (String, String) {
    (
        if code.is_empty() {
            "Your stream was removed from the room".to_string()
        } else {
            format!("Your stream was removed from {code}")
        },
        "The room's creator took your stream out of the room. It is still live on its own code, and people watching through it are still watching.".to_string(),
    )
}

/// Resets the per-room-session flags for a new join or mint.
fn begin_room_session(sh: &mut Shell, key: &str) {
    sh.room_leaving = false;
    sh.room_ending_by_me = false;
    sh.room_joined = false;
    sh.room = None;
    sh.room_key_used = key.to_owned();
}

/// A room was chosen in the sheet (docs/60 D8). It becomes the room the
/// next broadcast joins, and when live it is joined now. The grant a pasted
/// link carried wins over a key on file for that room.
fn choose_room(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, raw: &str, input: RoomInput) {
    let mut sh = shell.borrow_mut();
    let attach = match &input.grant {
        Some(RoomGrant::Attach(k)) => k.clone(),
        _ => sh
            .cfg
            .room_attach_key(&input.code)
            .unwrap_or_default()
            .to_owned(),
    };
    let creator = match &input.grant {
        Some(RoomGrant::Creator(hex)) => hex.clone(),
        _ => String::new(),
    };
    sh.cfg.room = raw.trim().to_string();
    sh.cfg.room_attach_secret = attach.clone();
    sh.pending_create = false;
    save_config(&mut sh);
    refresh_ready(ui, &sh.cfg, false);
    ui.set_show_room_sheet(false);
    ui.set_room_input("".into());
    ui.set_room_input_echo("".into());
    ui.set_room_input_ok(false);
    let Some(session) = sh.session.clone() else {
        return;
    };
    sh.room_grant = if creator.is_empty() {
        attach_grant(&attach)
    } else {
        Some(RoomGrant::Creator(creator.clone()))
    };
    begin_room_session(&mut sh, &attach);
    drop(sh);
    log::info!("room join requested");
    session.room_join(&input.code, &attach, &creator);
    ui.set_room_active(true);
    ui.set_room_attached(false);
    ui.set_room_code(input.code.into());
    ui.set_room_link("".into());
    ui.set_room_status("Joining the room…".into());
}

/// A static room's attach key as the room-view grant; none when unset.
fn attach_grant(key: &str) -> Option<RoomGrant> {
    (!key.is_empty()).then(|| RoomGrant::Attach(key.to_owned()))
}

/// The room card back to "no room session": the inputs stay, the live
/// picture goes.
fn reset_room_ui(ui: &MainWindow) {
    ui.set_room_active(false);
    ui.set_room_attached(false);
    ui.set_room_code("".into());
    ui.set_room_link("".into());
    ui.set_room_status("".into());
    ui.set_room_creator(false);
    ui.set_room_streaming(0);
    ui.set_room_watching(0);
    ui.set_room_rows(ModelRc::default());
    ui.set_room_watchers("".into());
    ui.set_room_watcher_initials(ModelRc::default());
    ui.set_room_needs_key(false);
    ui.set_show_manage(false);
}

fn end_broadcast(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, error: Option<String>) {
    let mut sh = shell.borrow_mut();
    let was_live = sh.state == UiState::Live;
    // The stopped summary (docs/60 D11), read before the session goes.
    let summary = sh.live_since.map(|since| {
        let st = merged_stats(&sh);
        let secs = since.elapsed().as_secs();
        (
            format!("You were live for {}", format_duration(secs)),
            summary_rows(
                sh.peak_viewers,
                st.bytes_sent + st.audio_bytes_sent,
                secs,
                st.width,
                st.height,
                st.fps,
            ),
        )
    });
    sh.live_since = None;
    // Ended inside the app: no resume question at the next launch (D12).
    sh.cfg.was_live = false;
    save_config(&mut sh);
    sh.state = UiState::Idle;
    sh.session = None;
    sh.media_info = None;
    if let Some(m) = sh.media.take() {
        m.shutdown();
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
    sh.room_grant = None;
    sh.room_leaving = false;
    sh.room = None;
    let pending_create = sh.pending_create;
    refresh_ready(ui, &sh.cfg, pending_create);
    drop(sh);
    reset_room_ui(ui);
    ui.set_show_room_sheet(false);
    ui.set_show_details(false);

    // A clean stop shows the summary with Go live again; an error shows the
    // error card on the plain Ready page.
    match (&summary, &error) {
        (Some((title, rows)), None) => {
            ui.set_summary_title(title.clone().into());
            ui.set_summary_rows(ModelRc::new(VecModel::from(rows.clone())));
            ui.set_summary_visible(true);
        }
        _ => ui.set_summary_visible(false),
    }

    ui.set_busy(false);
    ui.set_live(false);
    ui.set_resuming(false);
    ui.set_state_label("Not broadcasting".into());
    ui.set_status_line("".into());
    ui.set_watching(0);
    ui.set_watching_known(false);
    ui.set_live_elapsed("".into());
    ui.set_connection_line("".into());
    ui.set_encode_line("".into());
    ui.set_encode_badge("".into());
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
    {
        let failure = shell.borrow().media().and_then(|m| m.take_failure());
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

    // The remainder needs the shell only for the pipeline's widgets.
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
    if let Some(since) = sh.live_since {
        ui.set_live_elapsed(format_elapsed(since.elapsed().as_secs()).into());
    }
    let bytes = st.bytes_sent + st.audio_bytes_sent;
    let rate_bps = bytes.saturating_sub(sh.last_bytes) * 8;
    ui.set_connection_line(format!("{:.1} Mbps", rate_bps as f64 / 1e6).into());
    ui.set_connection_ok(!sh.uplink_warned);
    drop(sh);
    shell.borrow_mut().last_bytes = bytes;
    let sh = shell.borrow();

    if let Some(p) = sh.media() {
        ui.set_audio_level(p.audio_level());
        let state = p.audio_state();
        let mode = p.capture_mode().unwrap_or(sh.capture_mode);
        ui.set_audio_line(audio_line(&state, mode).into());
        let hint = mode == "app" && p.audio_silence_hint();
        ui.set_audio_hint(hint);
        if hint {
            ui.set_audio_hint_text(
                "No sound from the app yet. Some games play their sound through a separate process this capture can't hear."
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
    {
        if let Some(info) = &sh.media_info {
            st.encoder = info.encoder.clone();
            st.capture_path = info.capture_path.clone();
            // The rung reads as what is ACTUALLY encoded (the aspect-fitted
            // dims), not the configured bounding box.
            st.width = info.width;
            st.height = info.height;
            if st.codec.is_empty() {
                st.codec = info.codec.clone();
            }
        }
        if let Some(p) = sh.media() {
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
    (hooks().notify)(summary, body, critical);
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
        assert_eq!(labels[0], "Official server");
        let urls = server_urls(&cfg);
        assert_eq!(urls.len(), labels.len(), "one detail line per server");
        assert_eq!(urls[0], gawk_engine::defaults::RELAY_URL);
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

    // --- docs/60 DR3: the redesign's view logic ---------------------------

    #[test]
    fn quality_line_names_the_next_broadcast() {
        let mut cfg = Config::default();
        assert_eq!(quality_line(&cfg), "1080p · 60 fps · up to 12 Mbps");
        cfg.width = 1280;
        cfg.height = 720;
        cfg.fps = 30;
        cfg.bitrate_bps = 2_500_000;
        assert_eq!(quality_line(&cfg), "720p · 30 fps · up to 2.5 Mbps");
        cfg.width = 3440;
        cfg.height = 1440;
        assert!(quality_line(&cfg).starts_with("up to 3440×1440 · "));
    }

    #[test]
    fn server_pill_is_the_site_or_the_custom_server() {
        let mut cfg = cfg_with_two_customs();
        assert_eq!(server_pill(&cfg), "gawk.ioio.fi");
        cfg.selected_server = "Juho's homelab".into();
        assert_eq!(server_pill(&cfg), "Juho's homelab");
        // A nameless profile shows its host.
        cfg.selected_server = "  ".into();
        assert_eq!(server_pill(&cfg), "other.example:4433");
        assert_eq!(host_of("https://gawk.ioio.fi/#/x"), "gawk.ioio.fi");
    }

    #[test]
    fn the_pending_room_reads_from_the_config_or_a_pending_create() {
        let mut cfg = Config::default();
        assert_eq!(
            pending_room_view(&cfg, false),
            (String::new(), String::new())
        );
        cfg.room = "https://gawk.ioio.fi/#/room/lan-party?rt=a%3Ak3y".into();
        assert_eq!(
            pending_room_view(&cfg, false),
            ("lan-party".into(), "Joins when you go live".into())
        );
        // A pending create wins over a stored room.
        assert_eq!(
            pending_room_view(&cfg, true),
            ("New room".into(), PENDING_CREATE_DETAIL.into())
        );
        cfg.room = "not a room".into();
        assert_eq!(pending_room_view(&cfg, false).0, "");
    }

    #[test]
    fn your_rooms_lists_saved_ones_first() {
        let mut cfg = Config::default();
        let day = 86_400;
        cfg.remember_room("older", "", 10 * day);
        cfg.remember_room("TuhisRoom", "", 11 * day);
        cfg.remember_room("newest", "", 12 * day);
        cfg.set_room_saved("TuhisRoom", true);
        let rows = recent_rows(&cfg, 12 * day + 60);
        let codes: Vec<&str> = rows.iter().map(|r| r.code.as_str()).collect();
        assert_eq!(codes, ["TuhisRoom", "newest", "older"]);
        assert!(rows[0].saved);
        assert_eq!(rows[0].detail, "Saved · last joined yesterday");
        assert_eq!(rows[1].detail, "Last joined today");
        assert_eq!(rows[2].detail, "Last joined 2 days ago");
        assert_eq!(ago(100 * day, 10 * day), "over a month ago");
        assert_eq!(ago(100, 0), "recently");
    }

    #[test]
    fn the_room_field_echoes_what_it_read() {
        assert_eq!(room_input_echo("  "), (String::new(), false));
        assert_eq!(room_input_echo("lan-party"), (String::new(), true));
        assert_eq!(
            room_input_echo("https://gawk.ioio.fi/#/room/lan-party?rt=a%3Ak3y"),
            ("Room lan-party · key included".into(), true)
        );
        assert_eq!(
            room_input_echo(&format!(
                "https://gawk.ioio.fi/#/room/K7XQ2M?rt=c:{}",
                "ab".repeat(16)
            )),
            ("Room K7XQ2M · you made this room".into(), true)
        );
        assert_eq!(
            room_input_echo("https://gawk.ioio.fi/#/room/K7XQ2M"),
            ("Room K7XQ2M".into(), true)
        );
        assert!(!room_input_echo("not a room").1);
    }

    fn room_with_people() -> RoomSummary {
        use gawk_engine::room::{RoomAttachmentInfo, RoomPerson};
        let person = |id, nick: &str, streaming, speaking| RoomPerson {
            id,
            nickname: nick.into(),
            kind: 0,
            streaming,
            speaking,
        };
        let stream = |id: &str, label: &str, live, viewers| RoomAttachmentInfo {
            broadcast_id: id.into(),
            label: label.into(),
            live,
            viewer_count: viewers,
        };
        RoomSummary {
            code: "TuhisRoom".into(),
            your_id: 1,
            attach_ok: true,
            attachments: vec![
                stream("AWAY01", "sanna", false, 0),
                stream("MIKA01", "mika", true, 3),
                stream("K7XQ2M", "tuhis", true, 5),
            ],
            people: vec![
                person(1, "tuhis", true, false),
                person(2, "mika", true, true),
                person(3, "sanna", true, false),
                person(4, "jussi", false, false),
                person(5, "ella", false, false),
                person(6, "kai", false, false),
                person(7, "", false, false),
            ],
            participants: 7,
            ..RoomSummary::default()
        }
    }

    #[test]
    fn the_roster_lists_ours_then_live_then_away_and_names_the_watchers() {
        let s = room_with_people();
        let v = roster_view(&s, "K7XQ2M", "https://gawk.ioio.fi", false);
        let names: Vec<&str> = v.rows.iter().map(|r| r.name.as_str()).collect();
        assert_eq!(names, ["tuhis", "mika", "sanna"]);
        assert!(v.rows[0].you && v.rows[0].watch_link.is_empty());
        assert_eq!(v.rows[0].detail, "Streaming · 5 watching");
        assert_eq!(v.rows[1].watch_link, "https://gawk.ioio.fi/#/view/MIKA01");
        assert!(v.rows[1].speaking, "mika's speaking flag reaches the row");
        assert!(v.rows[2].away);
        assert_eq!(v.rows[2].detail, "Away · their stream is paused");
        assert!(v.rows.iter().all(|r| !r.removable), "not the creator");
        assert_eq!((v.streaming, v.watching), (3, 4));
        assert_eq!(v.watchers, "jussi, ella, kai and 1 more are watching");
        let initials: Vec<&str> = v.watcher_initials.iter().map(|i| i.as_str()).collect();
        assert_eq!(initials, ["J", "E", "K", "+1"]);
    }

    #[test]
    fn the_creator_may_remove_every_stream_but_its_own() {
        let s = room_with_people();
        let v = roster_view(&s, "K7XQ2M", "https://gawk.ioio.fi", true);
        let removable: Vec<bool> = v.rows.iter().map(|r| r.removable).collect();
        assert_eq!(removable, [false, true, true]);
    }

    #[test]
    fn watcher_lines_read_naturally() {
        let mut s = room_with_people();
        s.people.retain(|p| p.streaming || p.nickname == "jussi");
        assert_eq!(
            roster_view(&s, "K7XQ2M", "x", false).watchers,
            "jussi is watching"
        );
        s.people.retain(|p| p.streaming);
        let v = roster_view(&s, "K7XQ2M", "x", false);
        assert_eq!((v.watchers.as_str(), v.watching), ("", 0));
    }

    #[test]
    fn a_gated_static_room_asks_for_its_key() {
        let mut s = room_with_people();
        s.attach_ok = false;
        assert!(room_needs_key(&s, false));
        assert!(!room_needs_key(&s, true), "attached: nothing to ask");
        s.dynamic = true;
        assert!(!room_needs_key(&s, false), "dynamic rooms have no key");
    }

    #[test]
    fn a_room_end_is_a_card_only_when_someone_else_ended_our_room() {
        assert_eq!(
            room_end_view(true, true, true, "P9HDZ3", "the room's creator ended it"),
            RoomEndView::Quiet
        );
        match room_end_view(false, true, true, "P9HDZ3", "the room's creator ended it") {
            RoomEndView::Card(title, body) => {
                assert_eq!(title, "Room P9HDZ3 is over");
                assert!(body.starts_with("The room's creator ended it. Your stream is still live"));
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(
            room_end_view(false, false, true, "NOPE00", "no such room"),
            RoomEndView::Status("Couldn't join the room: no such room.".into())
        );
        assert_eq!(
            room_end_view(false, true, false, "P9HDZ3", "the room ended"),
            RoomEndView::Status("The room ended.".into())
        );
        let (title, body) = removed_card("TuhisRoom");
        assert_eq!(title, "Your stream was removed from TuhisRoom");
        assert!(body.contains("still live on its own code"));
    }

    #[test]
    fn clocks_and_the_stopped_summary() {
        assert_eq!(format_elapsed(0), "0:00");
        assert_eq!(format_elapsed(724), "12:04");
        assert_eq!(format_elapsed(3723), "1:02:03");
        assert_eq!(format_duration(1), "1 second");
        assert_eq!(format_duration(42), "42 seconds");
        assert_eq!(format_duration(60), "1 minute");
        assert_eq!(format_duration(42 * 60 + 5), "42 minutes");
        assert_eq!(format_duration(72 * 60), "1 h 12 min");
        // 84 MB over 60 s is 11.2 Mbps.
        let rows = summary_rows(7, 84_000_000, 60, 1920, 1080, 60);
        let pairs: Vec<(&str, &str)> = rows
            .iter()
            .map(|r| (r.label.as_str(), r.value.as_str()))
            .collect();
        assert_eq!(
            pairs,
            [
                ("Most people watching", "7"),
                ("Average upload", "11.2 Mbps"),
                ("Sent", "1920×1080 · 60 fps"),
            ]
        );
        assert_eq!(summary_rows(0, 5, 0, 1, 1, 1)[1].value, "n/a");
        assert_eq!(fmt_mbps(12_000_000), "12");
    }

    #[test]
    fn the_source_is_the_remembered_one_else_the_first_display() {
        let windows = vec!["ELDEN RING™".to_string(), "Discord".to_string()];
        let monitors = vec!["Display 1".to_string(), "Display 2".to_string()];
        assert_eq!(default_source(&windows, &monitors, ""), Some((1, 0)));
        assert_eq!(
            default_source(&windows, &monitors, &source_key(0, "Discord")),
            Some((0, 1))
        );
        assert_eq!(
            default_source(&windows, &monitors, &source_key(1, "Display 2")),
            Some((1, 1))
        );
        // Remembered but gone: back to the first display.
        assert_eq!(
            default_source(&windows, &monitors, "window:Closed game"),
            Some((1, 0))
        );
        assert_eq!(default_source(&windows, &[], "window:Closed game"), None);
        assert_eq!(source_key(1, "Display 1"), "display:Display 1");
        assert_eq!(source_key(0, "Discord"), "window:Discord");
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
