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
use crate::{MainWindow, StatRow, debuglog, diagnostics, refresh_captions, version};
use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::config::{self, Config, DEFAULT_SERVER_NAME, ServerProfile};
use gawk_engine::sender::Sender;
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use gawk_engine::telemetry::{Hello, Reporter};
use slint::{ComponentHandle, ModelRc, SharedString, VecModel};
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
    /// (hex) of a room this session minted, or the static room's attach
    /// key. In memory only: it is a one-broadcast affair.
    room_grant: String,
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
        room_grant: String::new(),
        room_leaving: false,
        nick_timer: slint::Timer::default(),
        nick_sent: String::new(),
    }));

    let ui = MainWindow::new().expect("create window");
    ui.set_app_version(format!("v{}", version::display()).into());
    seed_settings(&ui, &shell.borrow().cfg);
    refresh_captions(&ui, &shell.borrow().cfg);
    ui.set_resume_code(shell.borrow().cfg.last_broadcast_id.clone().into());
    shell.borrow_mut().platform.init_window(&ui);

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
        let sh = shell.borrow();
        let _ = sh.rt.block_on(async {
            tokio::time::timeout(std::time::Duration::from_secs(3), session.stop()).await
        });
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
            if let Some(m) = shell.borrow().media() {
                m.switch_audio_to_system();
            }
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
                ui.set_show_thumbnail(info.show_thumbnail);
                sh.media_info = Some(info);
                sh.media = Some(media);
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

    if let Some(p) = sh.media() {
        ui.set_audio_level(p.audio_level());
        let state = p.audio_state();
        let mode = p.capture_mode().unwrap_or(sh.capture_mode);
        ui.set_audio_line(audio_line(&state, mode).into());
        let hint = mode == "app" && p.audio_silence_hint();
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
