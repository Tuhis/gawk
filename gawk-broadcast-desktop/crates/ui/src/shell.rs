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

use crate::instance::{self, Incoming, Launch, Request};
use crate::messages::{StartFailure, can_mint, first_line, message};
use crate::preview::PreviewFrame;
use crate::{
    MainWindow, RecentRow, RoomRow, StatRow, debuglog, diagnostics, fit, refresh_captions, version,
};
use gawk_engine::clock::{Clock, MonotonicClock};
use gawk_engine::config::{self, Config, DEFAULT_SERVER_NAME, ServerProfile};
use gawk_engine::install::{self, Layout, Plan};
use gawk_engine::link::{self, DropReason, Dropped, Link};
use gawk_engine::lossnotice::{LossMonitor, NetworkFacts, Notice};
use gawk_engine::probe::ProbeResult;
use gawk_engine::room::RoomSummary;
use gawk_engine::sender::Sender;
use gawk_engine::session::{EngineEvent, Session, SessionConfig};
use gawk_engine::telemetry::{Hello, Reporter};
use gawk_engine::update::{self, Outcome, Update};
use gawk_engine::{RoomGrant, RoomInput, parse_room_input};
use slint::{ComponentHandle, Model, ModelRc, SharedString, VecModel};
use std::any::Any;
use std::cell::RefCell;
use std::ffi::OsString;
use std::path::{Path, PathBuf};
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
    /// Linux (docs/58 D6/D14): completed mid-session capture rebuilds.
    fn capture_restarts(&self) -> u64 {
        0
    }
    /// Linux: what the portal returned, "screen" | "window".
    fn share_mode(&self) -> Option<&'static str> {
        None
    }
    /// Linux: the binary whose audio is captured, when one application's is.
    fn audio_app(&self) -> Option<String> {
        None
    }
    /// Linux: the system-audio cascade's winner, to cache as
    /// `lastGoodAudioSource` and re-verify first next time (docs/58 D7).
    fn audio_source_to_cache(&self) -> Option<String> {
        None
    }
    /// A pump died; the broadcast should end with this message.
    fn take_failure(&self) -> Option<String>;
    /// Tears the media down in dependency order. No zombie capture.
    fn shutdown(self: Box<Self>);
    /// Tears the media down like [`Media::shutdown`], but hands back what the
    /// platform needs to capture the same source again — Linux's portal
    /// grant — for a pause or a quick restart (docs/64 D8, D9). The platform
    /// gets it back through [`Platform::source_returned`]. `None`: nothing
    /// to keep (the platform's own selection is still there).
    fn shutdown_keep_source(self: Box<Self>) -> Option<Box<dyn Any + Send>> {
        self.shutdown();
        None
    }
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
    /// What is shared, as Live's Sharing row names it: a window's title, a
    /// display's label, the picker's summary (docs/64 D7).
    pub source: String,
    /// The source is one window (the row's icon).
    pub source_is_window: bool,
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
    /// 1 Hz while live: what carries the broadcast to `relay` (docs/57 D7).
    /// `None` — the default — keeps the "dropping some video" line off.
    fn network_facts(&mut self, _relay: std::net::SocketAddr) -> Option<NetworkFacts> {
        None
    }
    /// After a successful `prepare_start`: what this start decided that the
    /// config should remember (Linux: the whose-audio choice, docs/39 D5).
    /// Returns true when it changed anything; the shell then saves.
    fn remember(&mut self, _cfg: &mut Config) -> bool {
        false
    }
    /// A pause or a quick restart shut the media down and handed its source
    /// back ([`Media::shutdown_keep_source`]): keep it for the next
    /// `prepare_start`, unless a newer pick already replaced it.
    fn source_returned(&mut self, _ui: &MainWindow, _source: Box<dyn Any + Send>) {}
    /// The platform's own picker chose a new source while live and wants the
    /// broadcast to switch to it: true once per request, and the shell does
    /// a quick restart on the same code (docs/64 D12).
    fn take_restart_request(&mut self) -> bool {
        false
    }
    /// The broadcast is over (End, a failure, a quit): let go of anything
    /// held for it. Ending from Paused finds no media to shut down, so a
    /// source handed back at the pause ([`Platform::source_returned`]) is
    /// released here — Linux's portal grant and its sharing indicator.
    fn broadcast_ended(&mut self, _ui: &MainWindow) {}
    /// The Ready page's source preview (docs/65 D4, D7), asked every tick:
    /// `wanted` while the window is idle on the main page and not minimized,
    /// and false before every `prepare_start` (D5). The platform runs a
    /// capture-only preview exactly while it is wanted and something is
    /// chosen — its [`crate::preview::PreviewSlot`] does the bookkeeping. The default: none.
    fn preview(&mut self, _ui: &MainWindow, _wanted: bool) -> PreviewFrame {
        PreviewFrame::Hidden
    }
    /// Where the window and the screen's work area are, for window fit
    /// (R64, docs/66 D15). `None` — the default — leaves the window's size
    /// to the user.
    fn placement(&self, _ui: &MainWindow) -> Option<fit::Placement> {
        None
    }
    /// Brings the window to the front for a second launch or a link (R66,
    /// docs/68 D6). `activation` is the Wayland activation token the
    /// launcher gave the second launch (D8). The default shows the window
    /// and nothing more.
    fn raise_window(&mut self, ui: &MainWindow, _activation: Option<String>) {
        let _ = ui.show();
    }
    fn as_any_mut(&mut self) -> &mut dyn Any;
}

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum UiState {
    Idle,
    Starting,
    Live,
    /// A session with its publish leg closed and no media (docs/64 D9).
    Paused,
}

/// Everything background threads report back to the UI.
enum ShellMsg {
    Started {
        session: Arc<Session>,
        media: Box<dyn Media>,
    },
    StartFailed(StartFailure),
    /// A resume from pause or a quick restart built its new media, for the
    /// build generation `build`.
    Restarted {
        build: u64,
        media: Box<dyn Media>,
    },
    /// It could not: the broadcast ends with this reason.
    RestartFailed {
        build: u64,
        failure: StartFailure,
    },
    /// The header's probe of `url` came back (docs/64 D1).
    Probed {
        url: String,
        result: ProbeResult,
    },
    /// Test connection on the Edit server page came back.
    Tested {
        url: String,
        result: ProbeResult,
    },
    Engine(EngineEvent),
    /// An update check came back: the launch one, or one the Settings button
    /// asked for (`manual`), which also reports its result in Settings.
    UpdateChecked {
        outcome: Outcome,
        manual: bool,
    },
    /// R47: the download for `version` came back — verified and ready to
    /// swap in, or why not.
    UpdateStaged {
        version: String,
        result: Result<Ready, String>,
    },
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
    /// Loss in the air the bandwidth watchdog cannot see (docs/57 D7), fed
    /// 1 Hz; fresh per broadcast.
    loss: LossMonitor,
    loss_notice: Notice,
    network: Option<NetworkFacts>,
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
    /// The update notice and the checks behind it, for this run.
    update: UpdateState,
    /// A resume from pause or a quick restart is building its media
    /// (docs/64 D8); a second request while one runs waits for it.
    restarting: bool,
    restart_again: bool,
    /// The running reclaim was asked for by the app, not caused by a loss:
    /// its first attempt is not narrated (docs/64 OD4).
    reclaim_quiet: bool,
    /// The reclaim running is a resume from pause: its failure means the
    /// relay let the paused code go.
    resuming_from_pause: bool,
    /// When the current pause began, and how long earlier ones lasted — the
    /// summary counts time live, not time paused.
    paused_since: Option<std::time::Instant>,
    paused_total: std::time::Duration,
    /// What the broadcast last sent (width, height, fps), for the summary.
    last_sent: Option<(u32, u32, u32)>,
    /// A reason to show when the session's Ended arrives without one (a
    /// restart that could not build its media).
    pending_error: Option<String>,
    /// Debounces a quality change while live into one restart (docs/64 D13).
    quality_timer: slint::Timer,
    /// The header's probe of the selected relay (docs/64 D1).
    probe: ProbeState,
    /// The room this broadcast just left, for Rejoin (docs/64 D6).
    left_room: Option<LeftRoom>,
    /// The server the Edit server page shows (docs/64 D15): a profile name,
    /// or the reserved default name. Editing never selects.
    edit_server: String,
    /// The selected relay advertised the telemetry endpoint this broadcast
    /// reports to: its operator gets the diagnostics (docs/40 D16).
    foreign_telemetry: bool,
    /// Which media build is current: bumped by every restart and resume,
    /// and by every start and end, so a build finishing for a broadcast
    /// that moved on is recognised and dropped (review of #423).
    build_gen: u64,
    /// The status the relay refused this broadcast's last reclaim with.
    reclaim_refused: Option<u16>,
    /// Window fit and your size (R64, docs/66 D8–D13).
    fit: fit::FitState,
    /// Second launches and links, from the single-instance endpoint or the
    /// macOS Apple Event handler (R66, docs/68 D6).
    inbox: Option<mpsc::Receiver<Incoming>>,
    /// A broadcast link that arrived while starting (docs/68 D4).
    pending_link: Option<BroadcastLink>,
    /// Until when the first request from the inbox may be this launch's
    /// own link (macOS, docs/68 D10): a viewer link then is a cold start.
    cold_until: Option<std::time::Instant>,
    /// What the link card on screen asks.
    link_card: Option<LinkCard>,
}

/// The header probe's state: what was found for which relay, and when to
/// look again.
#[derive(Debug, Default)]
struct ProbeState {
    url: String,
    result: Option<ProbeResult>,
    in_flight: bool,
    next_at: Option<std::time::Instant>,
}

/// How often the header looks at the relay again while idle.
const PROBE_INTERVAL: std::time::Duration = std::time::Duration::from_secs(30);

/// A room left with Leave room: what Rejoin joins again.
#[derive(Debug, Clone)]
struct LeftRoom {
    code: String,
    attach: String,
    creator: String,
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

/// The shell's state, before any window exists: the runtime, the reporter
/// and the message channel, around a loaded config. `run` builds one; so
/// do the tests that drive `handle_message`.
fn build_shell(
    platform: Box<dyn Platform>,
    cfg: Config,
    cfg_path: Option<std::path::PathBuf>,
    log_path: Option<std::path::PathBuf>,
    install_target: Option<InstallTarget>,
) -> Shell {
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

    let (msg_tx, msg_rx) = mpsc::channel();
    let fit = fit::FitState::new((cfg.window_width, cfg.window_height));
    Shell {
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
        loss: LossMonitor::new(),
        loss_notice: Notice::None,
        network: None,
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
        update: UpdateState {
            target: install_target,
            ..Default::default()
        },
        restarting: false,
        restart_again: false,
        reclaim_quiet: false,
        resuming_from_pause: false,
        paused_since: None,
        paused_total: std::time::Duration::ZERO,
        last_sent: None,
        pending_error: None,
        quality_timer: slint::Timer::default(),
        probe: ProbeState::default(),
        left_room: None,
        edit_server: DEFAULT_SERVER_NAME.to_string(),
        foreign_telemetry: false,
        build_gen: 0,
        reclaim_refused: None,
        fit,
        inbox: None,
        pending_link: None,
        link_card: None,
        cold_until: None,
    }
}

/// Runs the broadcaster window until it closes. `wire_platform` connects
/// the platform's own callbacks (its picker) once the window exists.
/// `launch` is what `main` found: this launch's link, and the inbox later
/// launches arrive on (R66, docs/68 D6).
pub fn run(
    platform: Box<dyn Platform>,
    wire_platform: impl FnOnce(&MainWindow, &Rc<RefCell<Shell>>),
    launch: Launch,
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
    let install_target = install_target();

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

    // A room saved as a pasted link (before the fix for #381's review) held
    // its grant in the clear: keep the code, move the grant to its wrapped
    // field, and rewrite the file.
    if let Some(input) = parse_room_input(&cfg.room)
        && input.grant.is_some()
    {
        store_room_choice(&mut cfg, &input);
        log::info!("moved a stored room link's grant into the credential store");
        if let Some(p) = &cfg_path
            && let Err(e) = config::save(p, &cfg, &*creds())
        {
            log::warn!("could not save the room settings: {e}");
        }
    }

    // docs/68 D3: a cold start for a viewer link opens the browser and exits
    // without showing a window.
    if let Some(url) = cold_viewer_url(&launch.request, &cfg) {
        log::info!("a viewer link at launch: opening the browser");
        open_in_browser(&url);
        return;
    }

    let shell = Rc::new(RefCell::new(build_shell(
        platform,
        cfg,
        cfg_path,
        log_path,
        install_target,
    )));
    if let Some(note) = &launch.note {
        log::warn!("{note}");
    }
    {
        let mut sh = shell.borrow_mut();
        sh.inbox = launch.inbox;
        if launch.link_in_inbox {
            sh.cold_until = Some(std::time::Instant::now() + COLD_LINK_WINDOW);
        }
    }

    let ui = MainWindow::new().expect("create window");
    ui.set_app_version(format!("v{}", version::display()).into());
    seed_settings(&ui, &shell.borrow().cfg);
    refresh_captions(&ui, &shell.borrow().cfg);
    {
        let sh = shell.borrow();
        let cfg = &sh.cfg;
        // docs/60 D12, docs/64 D11: still marked live at launch means the
        // app died live — it opens on Paused, with the code to resume.
        let crashed = cfg.was_live && !cfg.last_broadcast_id.is_empty();
        if crashed {
            log::info!("the last broadcast did not end in the app; offering to resume it");
            ui.set_resume_code(cfg.last_broadcast_id.clone().into());
            ui.set_code(cfg.last_broadcast_id.clone().into());
            ui.set_code_chars(code_chars(&cfg.last_broadcast_id));
            ui.set_join_link(
                gawk_engine::join_link(&cfg.resolve_app_url(), &cfg.last_broadcast_id).into(),
            );
            ui.set_crash_resume(true);
            ui.set_paused(true);
            if !cfg.room.is_empty() {
                ui.set_room_pending_detail("Rejoins when you resume".into());
            }
        }
    }
    shell.borrow_mut().platform.init_window(&ui);
    apply_default_source(&ui, &shell.borrow().cfg);

    wire_callbacks(&ui, &shell);
    wire_platform(&ui, &shell);
    start_update_check(&ui, &mut shell.borrow_mut());
    // docs/68 D6: the launch's own link, after the settings and the
    // crash-resume offer are in place. Nothing to raise: the window is
    // about to show.
    if let Request::Open(raw) = launch.request {
        open_link(&ui, &shell, raw, None);
    }

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
                    let restart = {
                        let mut sh = shell.borrow_mut();
                        let sh = &mut *sh;
                        sh.platform.tick(&ui, sh.media.as_deref());
                        // Taken only once live: a pick during Starting
                        // waits for Live, and one mid-restart queues
                        // behind it (restart_again).
                        sh.state == UiState::Live && sh.platform.take_restart_request()
                    };
                    // The platform's own picker chose a new source while
                    // live (Linux's portal): switch to it (docs/64 D12).
                    if restart {
                        restart_media(&ui, &shell);
                    }
                    tick(&ui, &shell);
                    fit_tick(&ui, &shell);
                }
            },
        );
    }

    // docs/66 D10, D13: the window opens at your size, and the first turn of
    // the event loop fits it to the screen rather than waiting a tick.
    let (w, h) = shell.borrow().fit.launch_size();
    if (w, h) != fit::DEFAULT_SIZE {
        ui.window().set_size(slint::LogicalSize::new(w, h));
    }
    {
        let ui_weak = ui.as_weak();
        let shell = shell.clone();
        slint::Timer::single_shot(std::time::Duration::ZERO, move || {
            if let Some(ui) = ui_weak.upgrade() {
                fit_tick(&ui, &shell);
            }
        });
    }

    ui.run().expect("run event loop");
    save_window_size(&shell);
    // ⌘Q (macOS) ends the event loop without the close dialog: never leave
    // a capture or a publisher session behind the window.
    shutdown_now(&shell);
}

/// Window fit (R64, docs/66 D8–D16): one look at the window and the main
/// page, the move that follows if any, and your size saved once it settles.
fn fit_tick(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let window = ui.window();
    let size = window.size().to_logical(window.scale_factor());
    let mut sh = shell.borrow_mut();
    let placement = sh.platform.placement(ui);
    let seen = fit::Observed {
        size: (size.width, size.height),
        main_page: ui.get_page() == 0,
        state: (ui.get_busy() || ui.get_paused(), ui.get_room_active()),
        needs: fit::Needs {
            min: ui.get_page_needs_min(),
            full: ui.get_page_needs(),
        },
        placement,
        arranged: window.is_maximized() || window.is_fullscreen() || window.is_minimized(),
        now: std::time::Instant::now(),
    };
    let mv = sh.fit.step(&seen);
    if let Some((w, h)) = sh.fit.take_save(seen.now) {
        store_window_size(&mut sh, w, h);
    }
    drop(sh);
    let Some(mv) = mv else {
        return;
    };
    log::info!(
        "window fit ({:?}): {} -> {} px{}",
        mv.reason,
        size.height.round(),
        mv.client_h.round(),
        mv.y.map(|y| format!(", top to {}", y.round()))
            .unwrap_or_default()
    );
    window.set_size(slint::LogicalSize::new(size.width, mv.client_h));
    if let (Some(y), Some(p)) = (mv.y, placement) {
        window.set_position(slint::LogicalPosition::new(p.frame.x, y));
    }
}

/// Your size, as it stands at quit, if a manual resize hasn't been saved yet.
fn save_window_size(shell: &Rc<RefCell<Shell>>) {
    let mut sh = shell.borrow_mut();
    let due = std::time::Instant::now() + fit::SAVE_DELAY;
    if let Some((w, h)) = sh.fit.take_save(due) {
        store_window_size(&mut sh, w, h);
    }
}

fn store_window_size(sh: &mut Shell, w: u32, h: u32) {
    if (sh.cfg.window_width, sh.cfg.window_height) != (w, h) {
        sh.cfg.window_width = w;
        sh.cfg.window_height = h;
        save_config(sh);
    }
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

/// The detail line under each server in Settings: its relay address, and
/// for the selected one what the header's probe found ("24 ms", "Can't
/// reach", docs/64 D15). `probe_note` is empty while unknown.
fn server_urls(cfg: &Config, probe_note: &str) -> Vec<SharedString> {
    let selected = selected_combo_index(cfg) as usize;
    let mut urls = vec![gawk_engine::defaults::RELAY_URL.to_string()];
    for p in custom_profiles(cfg) {
        urls.push(if p.url.trim().is_empty() {
            "No address yet".to_string()
        } else {
            p.url.trim().to_string()
        });
    }
    urls.into_iter()
        .enumerate()
        .map(|(i, u)| {
            if i == selected && !probe_note.is_empty() {
                format!("{u} · {probe_note}").into()
            } else {
                u.into()
            }
        })
        .collect()
}

/// What the probe found, as a server row's note.
fn probe_note(probe: &ProbeState) -> String {
    match &probe.result {
        Some(ProbeResult::Ok { rtt_ms, .. }) => format!("{rtt_ms} ms"),
        Some(ProbeResult::Failed) => "Can't reach".into(),
        None => String::new(),
    }
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

/// Seeds the Settings server list from the config: labels, addresses, the
/// selection. Called on load and whenever the selection or the list changes.
fn seed_server_list(ui: &MainWindow, cfg: &Config, probe_note: &str) {
    ui.set_server_labels(ModelRc::new(VecModel::from(server_labels(cfg))));
    ui.set_server_urls(ModelRc::new(VecModel::from(server_urls(cfg, probe_note))));
    ui.set_set_server(selected_combo_index(cfg));
    ui.set_default_relay(gawk_engine::defaults::RELAY_URL.into());
}

/// Seeds the Edit server page from the profile it edits (docs/64 D15) —
/// `edit` is a profile name or the reserved default name — NOT on every
/// keystroke (rewriting a field's text mid-edit moves the caret).
fn seed_edit_fields(ui: &MainWindow, cfg: &Config, edit: &str) {
    match custom_profiles(cfg).into_iter().find(|p| p.name == edit) {
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
            ui.set_set_secret(cfg.default_secret().into());
        }
    }
    ui.set_test_state(0);
    ui.set_test_result("".into());
}

fn seed_settings(ui: &MainWindow, cfg: &Config) {
    seed_server_list(ui, cfg, "");
    // The shell's `edit_server` starts at the default: the page's fields
    // must say the same, or the first unrelated settings edit would write
    // the selected server's fields into the default's slot.
    seed_edit_fields(ui, cfg, DEFAULT_SERVER_NAME);
    ui.set_room_nickname(cfg.nickname.clone().into());
    ui.set_picker_view(picker_view_tab(&cfg.picker_view));
    ui.set_set_app_url(cfg.app_url.clone().into());
    ui.set_set_telemetry(cfg.telemetry_url.clone().into());
    ui.set_set_update_check(!cfg.disable_update_check);
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
/// would pin this user to it forever. The server fields land in the profile
/// the Edit server page shows, `edit` (docs/64 D15), which need not be the
/// selected one; the legacy flat relay/secret pair stays retired after
/// migration. Returns the edited profile's name after any rename.
fn read_settings(ui: &MainWindow, cfg: &mut Config, edit: &str) -> String {
    let secret = ui.get_set_secret().trim().to_string();
    let mut edited = edit.to_string();
    let is_custom = custom_profiles(cfg).iter().any(|p| p.name == edit);
    if is_custom {
        // A rename follows the Linux UpdateCustomServer rule: an empty,
        // reserved, or already-taken new name keeps the old one — the name
        // is the selection key, so a collision would make two profiles
        // indistinguishable. The selection follows the rename.
        let new_name = ui.get_set_server_name().trim().to_string();
        let rename = !new_name.is_empty() && new_name != edit && !cfg.profile_name_taken(&new_name);
        let p = cfg
            .servers
            .iter_mut()
            .find(|p| p.name == edit)
            .expect("edited profile exists");
        if rename {
            p.name = new_name.clone();
        }
        p.url = ui.get_set_relay().trim().to_string();
        p.publish_secret = secret;
        if rename {
            if cfg.selected_server == edit {
                cfg.selected_server = new_name.clone();
            }
            edited = new_name;
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
    cfg.disable_update_check = !ui.get_set_update_check();
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
    edited
}

/// The custom-resolution parser: both fields must parse to positive
/// integers or the pair falls back to the default rung (0, 0) — a half-
/// typed size must not persist as a mangled rung. Values clamp into
/// [128, 3840] × [128, 2160] (4K is the ceiling) and floor to even (NV12
/// needs even dimensions). The result is a bounding box: the encode
/// resolution is the source aspect fitted inside it (docs/38 D11).
/// Help for the "dropping some video" line (docs/57 D7): the README
/// section with the remedies, in D7's order.
const NETWORK_HELP_URL: &str = "https://github.com/Tuhis/gawk/blob/main/gawk-broadcast-desktop/README.md#broadcasting-over-wi-fi-on-a-mac";

/// The uplink warning line: names the active bitrate so the remedy (lower
/// it) is one thought away.
fn uplink_warning_text(bitrate_bps: u32) -> String {
    format!(
        "Your upload can't keep up with the stream, so people watching may see frozen or \
         delayed video. Lower the upload cap (now {:.0} Mbps), or free up upload bandwidth.",
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
    let (title, detail) = quality_rows(cfg);
    format!("{title} · {detail}")
}

/// The Paused page's Quality row (docs/64 D9), in Live's shape: what the
/// next start sends ("1080p · 60 fps"), then its cap ("up to 12 Mbps").
fn quality_rows(cfg: &Config) -> (String, String) {
    let (w, h, fps, bps) = cfg.resolve_rung();
    let size = match (w, h) {
        (2560, 1440) => "1440p".to_string(),
        (1920, 1080) => "1080p".to_string(),
        (1280, 720) => "720p".to_string(),
        (854, 480) => "480p".to_string(),
        (w, h) => format!("up to {w}×{h}"),
    };
    (
        format!("{size} · {fps} fps"),
        format!("up to {} Mbps", fmt_mbps(bps)),
    )
}

/// Live's Quality row: what is actually encoded (the aspect-fitted size,
/// docs/38 D11), then the encoder and the cap.
fn live_quality_rows(height: u32, fps: u32, bps: u32) -> (String, String) {
    (
        format!("{height}p · {fps} fps"),
        // The cascade accepts hardware encoders only (docs/38 G3).
        format!("H.264 hardware · up to {} Mbps", fmt_mbps(bps)),
    )
}

/// The host of a URL, for display: `https://gawk.ioio.fi/x` → `gawk.ioio.fi`.
fn host_of(url: &str) -> String {
    let rest = url.trim().split_once("://").map_or(url.trim(), |(_, r)| r);
    rest.split(['/', '?', '#']).next().unwrap_or("").to_string()
}

/// The non-default server strip (docs/64 D3): `None` on the pinned default,
/// else the server's name (its address when unnamed) and its host.
fn server_strip(cfg: &Config) -> Option<(String, String)> {
    let p = cfg.selected_profile()?;
    let host = host_of(&p.url);
    let name = if p.name.trim().is_empty() {
        host.clone()
    } else {
        p.name.trim().to_string()
    };
    Some((name, host))
}

/// The strip's disclosure line (docs/40 D16): shown while this broadcast's
/// diagnostics go to the non-default server's operator.
const FOREIGN_DIAGNOSTICS_NOTE: &str =
    "Diagnostics from this broadcast go to this server's operator.";

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
    let (title, detail) = quality_rows(cfg);
    ui.set_quality_title(title.into());
    ui.set_quality_detail(detail.into());
    match server_strip(cfg) {
        Some((name, host)) => {
            ui.set_server_custom(true);
            ui.set_server_name(name.into());
            ui.set_server_host(host.into());
        }
        None => ui.set_server_custom(false),
    }
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
/// `creator` lets the rows offer Remove; `we_paused` names our own away row
/// for what it is (docs/64 D9: a pause keeps the room).
fn roster_view(
    s: &RoomSummary,
    our_id: &str,
    app_url: &str,
    creator: bool,
    we_paused: bool,
) -> RosterView {
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
            let detail = if !a.live && ours && we_paused {
                "Paused".to_string()
            } else if !a.live && ours {
                "Away".to_string()
            } else if !a.live {
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

/// The summary card's tiles (docs/64 D10): time live, most watching,
/// average upload, what was sent — each a value over what it means.
fn summary_rows(peak: u32, bytes: u64, secs: u64, height: u32, fps: u32) -> Vec<StatRow> {
    let tile = |value: String, label: &str| StatRow {
        label: label.into(),
        value: value.into(),
    };
    let avg = if secs == 0 {
        "n/a".to_string()
    } else {
        format!("{:.1} Mbps", bytes as f64 * 8.0 / secs as f64 / 1e6)
    };
    vec![
        tile(format_duration(secs), "live"),
        tile(peak.to_string(), "most watching"),
        tile(avg, "average upload"),
        tile(format!("{height}p{fps}"), "sent"),
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

/// The picker tab to open on, from the saved `pickerView`: Apps and games
/// unless the user last looked at Whole display.
fn picker_view_tab(saved: &str) -> i32 {
    if saved == "display" { 1 } else { 0 }
}

fn picker_view_key(tab: i32) -> &'static str {
    if tab == 1 { "display" } else { "" }
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
    // The picker page shows the choice too (a refresh happens with it open);
    // the tab it is on stays.
    ui.set_draft_window(ui.get_selected_window());
    ui.set_draft_monitor(ui.get_selected_monitor());
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

/// Why this launch skips the update check, or `None` to run it: the
/// setting, the environment, or a check GitHub answered within the last 15 minutes
/// (docs/47 D3, D6). `refetch` waives the 15 minutes (R47): a remembered
/// update carries no file list, so a build that can install in place asks
/// again rather than show a notice it cannot act on.
fn update_check_skip(
    cfg: &Config,
    env_opted_out: bool,
    now: u64,
    refetch: bool,
) -> Option<&'static str> {
    if cfg.disable_update_check {
        Some("turned off in settings")
    } else if env_opted_out {
        Some("turned off by GAWK_NO_UPDATE_CHECK")
    } else if !refetch && !update::due(&cfg.last_update_check, now) {
        Some("checked within the last 15 minutes")
    } else {
        None
    }
}

/// The launch-time update check (docs/47 D3, at most every 15 minutes):
/// shows what the last answered check found, then — when one is due — asks
/// GitHub again on its own thread. Nothing schedules another; the Settings
/// button is the only other way a check starts.
fn start_update_check(ui: &MainWindow, sh: &mut Shell) {
    let env_off = update::env_opted_out();
    // Turned off means no notice at all, not even a remembered one.
    if !sh.cfg.disable_update_check && !env_off {
        sh.update.shown =
            update::cached(&sh.cfg.update_version, &sh.cfg.update_url, version::RELEASE);
        show_update(ui, sh.update.shown.as_ref());
    }
    let refetch = sh.update.shown.is_some() && sh.update.target.is_some();
    if let Some(why) = update_check_skip(&sh.cfg, env_off, now_unix(), refetch) {
        log::info!("update check skipped: {why}");
        return;
    }
    sh.update.checking = spawn_update_check(sh, false);
    ui.set_update_checking(sh.update.checking);
    show_install(ui, &sh.update);
}

/// One check on its own thread, reported back through the message channel.
/// False when the thread could not be started.
fn spawn_update_check(sh: &Shell, manual: bool) -> bool {
    let tx = sh.msg_tx.clone();
    let spawned = std::thread::Builder::new()
        .name("update-check".into())
        .spawn(move || {
            let dist = gawk_engine::defaults::this();
            let outcome = update::check(&update::manifest_url(dist), version::RELEASE, dist);
            let _ = tx.send(ShellMsg::UpdateChecked { outcome, manual });
        });
    if let Err(e) = &spawned {
        log::warn!("update check not started: {e}");
    }
    spawned.is_ok()
}

/// The update notice for this run, kept apart from the shell so its rules
/// can be tested without a window.
#[derive(Debug, Default)]
struct UpdateState {
    /// What the notice shows.
    shown: Option<Update>,
    /// The version dismissed this run: an automatic check that finds it
    /// again leaves the notice hidden until the app restarts.
    dismissed: Option<String>,
    /// A check is in flight.
    checking: bool,
    /// Check now was pressed while the launch check was in flight: that
    /// check's answer is reported as the button's.
    manual_waiting: bool,
    /// R47: how this build replaces itself, read at startup. `None` when it
    /// cannot (macOS, docs/54 D17; or no path to itself).
    target: Option<InstallTarget>,
    /// A download is in flight.
    staging: bool,
    /// A verified build, ready to swap in.
    ready: Option<Ready>,
    /// The version this run stops trying to stage: its download failed, or
    /// this install cannot be updated in place. The R45 line stays.
    given_up: Option<String>,
    /// The version swapped in this run whose relaunch failed. It is on
    /// disk, so nothing more is staged until a restart; unlike `given_up`,
    /// Check now does not clear it.
    installed: Option<String>,
    /// The line under the notice: the `.deb` instructions (docs/48 D9), or
    /// why an install failed.
    note: String,
}

/// How this build replaces itself (docs/48 D5, D6).
#[derive(Debug, Clone)]
struct InstallTarget {
    /// The binary's path as read at startup.
    exe: PathBuf,
    layout: Layout,
    /// The arguments to relaunch with.
    args: Vec<OsString>,
}

/// A verified build that will replace the running one.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Ready {
    version: String,
    binary: PathBuf,
}

impl UpdateState {
    fn dismiss(&mut self) {
        if let Some(u) = self.shown.take() {
            self.dismissed = Some(u.version);
        }
    }

    /// The update to download now, if any (docs/48 D4): one that is shown,
    /// lists signed files, and can be installed here; only while idle, one
    /// download at a time, not one already ready or given up on, and
    /// nothing once an update is installed.
    fn to_stage(&self, idle: bool) -> Option<Update> {
        let u = self.shown.as_ref()?;
        let wanted = idle
            && self.target.is_some()
            && self.installed.is_none()
            && !self.staging
            && u.files.is_some()
            && self.ready.as_ref().is_none_or(|r| r.version != u.version)
            && self.given_up.as_deref() != Some(u.version.as_str());
        wanted.then(|| u.clone())
    }

    /// Whether the shown release is the one staged: the button shows.
    fn ready_for_shown(&self) -> bool {
        matches!((&self.shown, &self.ready), (Some(u), Some(r)) if u.version == r.version)
    }

    /// What the notice offers: 2 Install and relaunch; 1 nothing yet, the
    /// install is on its way (a download in flight, or the launch check
    /// re-fetching a remembered update's file list); 0 the release page.
    fn phase(&self) -> i32 {
        let Some(u) = &self.shown else {
            return 0;
        };
        let refetching = self.checking
            && u.files.is_none()
            && self.target.is_some()
            && self.installed.is_none()
            && self.given_up.as_deref() != Some(u.version.as_str());
        if self.ready_for_shown() {
            2
        } else if self.staging || refetching {
            1
        } else {
            0
        }
    }

    /// Check now was pressed. True means start a check; false means one is
    /// already in flight, whose answer will be reported as this request's.
    fn request_manual(&mut self) -> bool {
        if self.checking {
            self.manual_waiting = true;
            return false;
        }
        self.checking = true;
        true
    }

    /// A check came back with `found` (`None` = no answer, see
    /// [`apply_update_outcome`]). Returns whether the Settings row should
    /// report it: the button asked for it, directly or while it was in
    /// flight. An asked-for answer shows even a dismissed release — that is
    /// what was asked; an automatic one respects this run's dismissal.
    fn finish(&mut self, found: Option<Option<Update>>, manual: bool) -> bool {
        self.checking = false;
        let manual = manual || std::mem::take(&mut self.manual_waiting);
        if let Some(found) = found {
            self.shown = if manual {
                self.dismissed = None;
                // Asking again retries an install this run gave up on: a
                // download cut off by the network is worth a second try.
                // An installed update keeps its restart note.
                self.given_up = None;
                if self.installed.is_none() {
                    self.note.clear();
                }
                found
            } else {
                found.filter(|u| self.dismissed.as_deref() != Some(u.version.as_str()))
            };
        }
        manual
    }

    /// The swap worked but the new build did not start: `version` is on
    /// disk, and starting the app again runs it.
    fn relaunch_failed(&mut self, version: &str) {
        self.installed = Some(version.to_owned());
        self.note = format!("Installed v{version}. Start the app again to use it.");
    }
}

/// Applies a check's outcome to the config. An answer restarts the 15
/// minutes and replaces the cached result; the notice then shows what it
/// found (`Some(found)`). No answer changes nothing (`None`): the stamp and
/// any notice already showing stay.
fn apply_update_outcome(cfg: &mut Config, outcome: &Outcome, now: u64) -> Option<Option<Update>> {
    match outcome {
        Outcome::Answered(found) => {
            cfg.last_update_check = update::format_rfc3339(now);
            cfg.update_version = found.as_ref().map_or(String::new(), |u| u.version.clone());
            cfg.update_url = found
                .as_ref()
                .map_or(String::new(), |u| u.release_url.clone());
            Some(found.clone())
        }
        Outcome::Unreachable(_) => None,
    }
}

/// The Settings button's result line.
fn update_status_text(outcome: &Outcome) -> String {
    match outcome {
        Outcome::Answered(Some(u)) => format!("Version {} is available.", u.version),
        Outcome::Answered(None) => format!("You have the latest version (v{}).", version::RELEASE),
        Outcome::Unreachable(_) => "Couldn't reach GitHub. Try again later.".into(),
    }
}

/// R47: how this build replaces itself. Read once, at startup, before
/// anything could rename the binary: after a Linux swap `/proc/self/exe`
/// reads `<path> (deleted)` (docs/48 D6). Also clears what an earlier update
/// left beside the binary.
fn install_target() -> Option<InstallTarget> {
    let layout = Layout::of(gawk_engine::defaults::this())?;
    let exe = match std::env::current_exe() {
        Ok(p) => p,
        Err(e) => {
            log::info!("in-place update off: no path to this binary ({e})");
            return None;
        }
    };
    for p in install::cleanup(&exe) {
        log::info!("removed {}, left by an earlier update", p.display());
    }
    Some(InstallTarget {
        exe,
        layout,
        // docs/68 D7, G10: the launch's link was applied once already.
        args: instance::relaunch_args(&std::env::args_os().skip(1).collect::<Vec<_>>()),
    })
}

/// Starts the download of the shown update when one is due (docs/48 D4):
/// after a check answers, and on the idle tick, so an update found while
/// live downloads once the broadcast ends.
fn maybe_stage(ui: &MainWindow, sh: &mut Shell) {
    let Some(u) = sh.update.to_stage(sh.state == UiState::Idle) else {
        return;
    };
    let Some(target) = sh.update.target.clone() else {
        return;
    };
    sh.update.note.clear();
    match install::plan(&target.exe, target.layout, Path::new(install::DEB_LIST)) {
        Plan::Deb => {
            log::info!(
                "update v{}: a .deb install, so no in-place update",
                u.version
            );
            sh.update.given_up = Some(u.version.clone());
            sh.update.note = deb_note(&u);
        }
        Plan::NotWritable(why) => {
            log::info!("update v{}: cannot install in place: {why}", u.version);
            sh.update.given_up = Some(u.version.clone());
            sh.update.note = NOT_WRITABLE_NOTE.into();
        }
        Plan::InPlace { staging } => {
            log::info!("update v{}: downloading and verifying", u.version);
            let tx = sh.msg_tx.clone();
            let layout = target.layout;
            let version = u.version.clone();
            let spawned = std::thread::Builder::new()
                .name("update-download".into())
                .spawn(move || {
                    let result = update::stage(&u, version::RELEASE, &staging).and_then(|s| {
                        install::prepare(layout, &s.file, &staging).map(|binary| Ready {
                            version: s.version,
                            binary,
                        })
                    });
                    let _ = tx.send(ShellMsg::UpdateStaged {
                        version: u.version,
                        result,
                    });
                });
            match spawned {
                Ok(_) => sh.update.staging = true,
                Err(e) => {
                    log::warn!("update download not started: {e}");
                    sh.update.given_up = Some(version);
                    sh.update.note = DOWNLOAD_FAILED_NOTE.into();
                }
            }
        }
    }
    show_install(ui, &sh.update);
}

/// Why the notice offers the release page instead of a button. The
/// details are in the log; the user needs to know only that this copy
/// won't update itself, and that Check now tries again.
const NOT_WRITABLE_NOTE: &str = "This copy can't update itself: its folder isn't writable.";
const DOWNLOAD_FAILED_NOTE: &str =
    "The download didn't work. Check now in Settings tries again, or download it yourself.";

/// docs/48 D9: what a `.deb` install is told instead of a button.
fn deb_note(u: &Update) -> String {
    u.deb.as_ref().map_or(String::new(), |name| {
        format!("Installed from the .deb: download {name}, then run sudo apt install ./{name}")
    })
}

fn show_install(ui: &MainWindow, st: &UpdateState) {
    ui.set_update_phase(st.phase());
    ui.set_update_note(
        if st.shown.is_some() {
            st.note.as_str()
        } else {
            ""
        }
        .into(),
    );
}

/// The Install and relaunch button (docs/48 D4-D6): swap, start the new
/// build with this one's arguments, and quit. Never while a broadcast is
/// starting or live; the button is disabled then too.
fn install_now(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let mut sh = shell.borrow_mut();
    if sh.state != UiState::Idle {
        return;
    }
    let (Some(target), Some(ready)) = (sh.update.target.clone(), sh.update.ready.take()) else {
        return;
    };
    match install::swap(target.layout, &target.exe, &ready.binary) {
        Ok(()) => {
            log::info!("installed v{} over {}", ready.version, target.exe.display());
            // docs/68 D7, G11: the new build waits for this one to exit
            // and takes the instance over, rather than handing off to it.
            match std::process::Command::new(&target.exe)
                .args(&target.args)
                .arg(instance::RELAUNCHED_FROM)
                .arg(std::process::id().to_string())
                .spawn()
            {
                Ok(_) => {
                    log::info!("started v{}; this build exits", ready.version);
                    drop(sh);
                    let _ = slint::quit_event_loop();
                    return;
                }
                Err(e) => {
                    log::warn!("installed v{} but could not start it: {e}", ready.version);
                    sh.update.relaunch_failed(&ready.version);
                }
            }
        }
        Err(e) => {
            log::warn!("update v{} not installed: {e}", ready.version);
            sh.update.given_up = Some(ready.version.clone());
            sh.update.note = format!("Couldn't install the update: {e}");
        }
    }
    show_install(ui, &sh.update);
}

fn show_update(ui: &MainWindow, update: Option<&Update>) {
    ui.set_update_version(
        update
            .map_or(String::new(), |u| format!("v{}", u.version))
            .into(),
    );
    ui.set_update_url(
        update
            .map_or(String::new(), |u| u.release_url.clone())
            .into(),
    );
}

/// An RGBA buffer as a Slint image (thumbnails, window icons).
pub fn rgba_image(w: u32, h: u32, rgba: &[u8]) -> slint::Image {
    let mut buf = slint::SharedPixelBuffer::<slint::Rgba8Pixel>::new(w, h);
    buf.make_mut_bytes().copy_from_slice(rgba);
    slint::Image::from_rgba8(buf)
}

/// The Ready page's preview for this tick (docs/65 D4): wanted while idle
/// on the main page with the window in view.
fn update_preview(ui: &MainWindow, sh: &mut Shell) {
    let wanted = sh.state == UiState::Idle && ui.get_page() == 0 && !ui.window().is_minimized();
    show_preview(ui, sh.platform.preview(ui, wanted));
}

fn show_preview(ui: &MainWindow, frame: PreviewFrame) {
    match frame {
        PreviewFrame::Hidden => ui.set_has_preview(false),
        PreviewFrame::Keep => {}
        PreviewFrame::New((w, h, rgba)) => {
            ui.set_preview(rgba_image(w, h, &rgba));
            ui.set_has_preview(true);
        }
    }
}

/// Resolves a start, a resume or a restart — with the preview stopped
/// first, so the broadcast's capture never shares the source with it
/// (docs/65 D5).
fn prepare(ui: &MainWindow, sh: &mut Shell) -> Result<Prepared, String> {
    show_preview(ui, sh.platform.preview(ui, false));
    sh.platform.prepare_start(ui, &sh.cfg)
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
        // Resume: back from a pause on the running session (docs/64 D9), or
        // — with no session — the persisted code: after a crash (D11) or
        // the summary card's undo (D10).
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_resume_broadcast(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let paused = shell.borrow().state == UiState::Paused;
                if paused {
                    resume_from_pause(&ui, &shell);
                } else {
                    start_broadcast(&ui, &shell, true);
                }
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_pause_broadcast(move || {
            if let Some(ui) = ui_weak.upgrade() {
                pause_broadcast(&ui, &shell);
            }
        });
    }
    {
        // End (docs/64 D10): live or paused, the session stops and Ready
        // shows the summary card.
        let shell = shell.clone();
        ui.on_stop_broadcast(move || {
            let sh = shell.borrow();
            if let Some(session) = sh.session.clone() {
                sh.rt.spawn(async move { session.stop().await });
            }
        });
    }
    // Copy link and Copy code confirm in place, in the button and on the
    // code itself (docs/66 D2, docs/64 D17).
    {
        let ui_weak = ui_weak.clone();
        ui.on_copy_link(move || {
            if let Some(ui) = ui_weak.upgrade() {
                copy_text(ui.get_join_link().as_str());
            }
        });
    }
    {
        let ui_weak = ui_weak.clone();
        ui.on_copy_code(move || {
            if let Some(ui) = ui_weak.upgrade() {
                copy_text(ui.get_code().as_str());
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
                    sh.network,
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
                let rung_before = sh.cfg.resolve_rung();
                let relay_before = sh.cfg.resolve_relay_url();
                let edit = sh.edit_server.clone();
                let tested_before = edit_relay_url(&sh.cfg, &edit);
                sh.edit_server = read_settings(&ui, &mut sh.cfg, &edit);
                save_config(&mut sh);
                clear_test_if_moved(
                    &ui,
                    &tested_before,
                    &edit_relay_url(&sh.cfg, &sh.edit_server),
                );
                // The lists follow name/URL edits live; the Edit page's
                // fields are not reseeded (that would move the caret of the
                // field being typed in).
                ui.set_server_labels(ModelRc::new(VecModel::from(server_labels(&sh.cfg))));
                let note = probe_note(&sh.probe);
                ui.set_server_urls(ModelRc::new(VecModel::from(server_urls(&sh.cfg, &note))));
                refresh_captions(&ui, &sh.cfg);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
                if sh.cfg.resolve_relay_url() != relay_before {
                    // The selected server's own address changed: look again.
                    restart_probe(&ui, &mut sh);
                }
                // docs/64 D13: a quality change while live applies itself,
                // once typing and dragging settle.
                if sh.state == UiState::Live && sh.cfg.resolve_rung() != rung_before {
                    let restart_shell = shell.clone();
                    let restart_ui = ui.as_weak();
                    sh.quality_timer.start(
                        slint::TimerMode::SingleShot,
                        QUALITY_SETTLE,
                        move || {
                            if let Some(ui) = restart_ui.upgrade() {
                                log::info!(
                                    "quality changed while live: restarting on the same code"
                                );
                                restart_media(&ui, &restart_shell);
                            }
                        },
                    );
                }
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
                let edit = sh.edit_server.clone();
                sh.edit_server = read_settings(&ui, &mut sh.cfg, &edit);
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
                refresh_captions(&ui, &sh.cfg);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
                restart_probe(&ui, &mut sh);
            }
        });
    }
    {
        // Edit (docs/64 D15): open a server's page without selecting it.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_edit_server(move |index| {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                sh.edit_server = combo_index_to_name(&sh.cfg, index);
                seed_edit_fields(&ui, &sh.cfg, &sh.edit_server);
            }
        });
    }
    {
        // A new server is one to use: it is selected, and its page opens.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_add_server(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let name = sh.cfg.add_custom_server();
                sh.cfg.selected_server = name.clone();
                sh.edit_server = name;
                save_config(&mut sh);
                seed_edit_fields(&ui, &sh.cfg, &sh.edit_server);
                refresh_captions(&ui, &sh.cfg);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
                restart_probe(&ui, &mut sh);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_remove_server(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let edited = sh.edit_server.clone();
                if !custom_profiles(&sh.cfg).iter().any(|p| p.name == edited) {
                    return; // the pinned default is not removable
                }
                sh.cfg.servers.retain(|p| p.name != edited);
                if sh.cfg.selected_server == edited {
                    sh.cfg.selected_server = DEFAULT_SERVER_NAME.to_string();
                }
                sh.edit_server = DEFAULT_SERVER_NAME.to_string();
                save_config(&mut sh);
                seed_edit_fields(&ui, &sh.cfg, &sh.edit_server);
                refresh_captions(&ui, &sh.cfg);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
                restart_probe(&ui, &mut sh);
            }
        });
    }
    {
        // Test connection (docs/64 D15): the header's probe, on demand, for
        // the server the Edit page shows.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_test_server(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let sh = shell.borrow();
                let url = edit_relay_url(&sh.cfg, &sh.edit_server);
                if url.is_empty() {
                    ui.set_test_state(3);
                    ui.set_test_result("Add the relay address first.".into());
                    return;
                }
                ui.set_test_state(1);
                ui.set_test_result("".into());
                let origin = sh.cfg.resolve_origin();
                let tx = sh.msg_tx.clone();
                sh.rt.spawn(async move {
                    let result = gawk_engine::probe::probe(&url, &origin, false).await;
                    let _ = tx.send(ShellMsg::Tested { url, result });
                });
            }
        });
    }
    {
        // The unreachable banner's Try again.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_probe_now(move || {
            if let Some(ui) = ui_weak.upgrade() {
                restart_probe(&ui, &mut shell.borrow_mut());
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
                    Some(input) => choose_room(&ui, &shell, input),
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
                let input = RoomInput {
                    code: code.to_string(),
                    grant: None,
                };
                choose_room(&ui, &shell, input);
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
                ui.set_room_left("".into());
                let mut sh = shell.borrow_mut();
                sh.left_room = None;
                let live = sh.session.is_some();
                sh.pending_create = store_room_create(&mut sh.cfg, live);
                save_config(&mut sh);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
                let Some(session) = sh.session.clone() else {
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
                sh.cfg.room_creator_token.clear();
                sh.cfg.room_server.clear();
                save_config(&mut sh);
                refresh_ready(&ui, &sh.cfg, false);
            }
        });
    }
    {
        // Leave room: detach and leave, and the next broadcast joins no room.
        // What was left is kept for Rejoin (docs/64 D6).
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_detach(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let mut sh = shell.borrow_mut();
                let code = match &sh.room {
                    Some(s) => s.code.clone(),
                    None => ui.get_room_code().to_string(),
                };
                sh.left_room = left_room(&code, &sh.room_key_used, sh.room_grant.as_ref());
                sh.room_leaving = true;
                sh.cfg.room.clear();
                sh.cfg.room_attach_secret.clear();
                sh.cfg.room_creator_token.clear();
                sh.cfg.room_server.clear();
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
        // Rejoin: the room just left, with the grant it was joined with.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_room_rejoin(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let left = shell.borrow_mut().left_room.take();
                ui.set_room_left("".into());
                if let Some(left) = left {
                    log::info!("rejoining the room just left");
                    choose_room(&ui, &shell, rejoin_input(&left));
                }
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
                // The key typed is stored for the server it was used on
                // (docs/68 D5a).
                let here = sh.cfg.server_key();
                sh.cfg.room_attach_secret = key.clone();
                sh.cfg.room_server = here.clone();
                sh.cfg.remember_room(&here, &code, &key, now_unix());
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
        ui.on_link_notice_dismiss(move || {
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_link_notice("".into());
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_link_card_accepted(move || {
            if let Some(ui) = ui_weak.upgrade() {
                link_card_accepted(&ui, &shell);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_link_card_declined(move || {
            if let Some(ui) = ui_weak.upgrade() {
                clear_link_card(&ui, &mut shell.borrow_mut());
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
                // The button confirms in place (docs/66 D2).
                copy_text(&gawk_engine::room_link(
                    &sh.cfg.resolve_app_url(),
                    &code,
                    None,
                ));
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
        // End on the crash's Paused page (docs/64 D11): there is no session
        // to stop — back to plain Ready, and no question at the next launch.
        ui.on_resume_dismiss(move || {
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_crash_resume(false);
                ui.set_paused(false);
                ui.set_code("".into());
                ui.set_join_link("".into());
                let mut sh = shell.borrow_mut();
                sh.cfg.was_live = false;
                save_config(&mut sh);
                refresh_ready(&ui, &sh.cfg, sh.pending_create);
            }
        });
    }
    {
        // The Windows picker's tab, opened on next time — across launches.
        let shell = shell.clone();
        ui.on_picker_view_changed(move |tab| {
            let mut sh = shell.borrow_mut();
            sh.cfg.picker_view = picker_view_key(tab).into();
            save_config(&mut sh);
        });
    }
    {
        // The Windows picker's row click (or Share this / Switch):
        // remembered, and while live, switched to at once (docs/64 D12).
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_source_picked(move || {
            if let Some(ui) = ui_weak.upgrade() {
                let live = {
                    let mut sh = shell.borrow_mut();
                    if let Some(key) = current_source_key(&ui) {
                        sh.cfg.last_source = key;
                        save_config(&mut sh);
                    }
                    sh.state == UiState::Live
                };
                if live {
                    log::info!("source changed while live: restarting on the same code");
                    restart_media(&ui, &shell);
                }
            }
        });
    }
    {
        ui.on_open_link(move |link| open_in_browser(link.as_str()));
    }
    {
        // Dismissing hides the notice until the app restarts; nothing is
        // stored, so the next launch shows it again.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_dismiss_update(move || {
            shell.borrow_mut().update.dismiss();
            if let Some(ui) = ui_weak.upgrade() {
                show_update(&ui, None);
                show_install(&ui, &shell.borrow().update);
            }
        });
    }
    {
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_install_update(move || {
            if let Some(ui) = ui_weak.upgrade() {
                install_now(&ui, &shell);
            }
        });
    }
    {
        // The Settings button: an explicit ask, so it runs whatever the
        // opt-out and the 15-minute gate say.
        let shell = shell.clone();
        let ui_weak = ui_weak.clone();
        ui.on_check_for_updates(move || {
            let mut sh = shell.borrow_mut();
            // A check already in flight (the launch one) answers this click.
            if sh.update.request_manual() && !spawn_update_check(&sh, true) {
                sh.update.checking = false;
            }
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_update_checking(sh.update.checking);
                ui.set_update_status(if sh.update.checking {
                    "Checking…".into()
                } else {
                    "Couldn't start the check.".into()
                });
                show_install(&ui, &sh.update);
            }
        });
    }
    ui.on_network_notice_help(|| open_in_browser(NETWORK_HELP_URL));
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
        UiState::Paused => "Paused",
    }
}

fn start_broadcast(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, resume: bool) {
    let mut sh = shell.borrow_mut();
    if sh.state != UiState::Idle {
        return;
    }
    let edit = sh.edit_server.clone();
    sh.edit_server = read_settings(ui, &mut sh.cfg, &edit);
    save_config(&mut sh);
    refresh_captions(ui, &sh.cfg);

    // The reclaim identity: only on the Resume button, the persisted token
    // only with the ID it was minted for, and both only to the server they
    // were minted on (review of #452).
    let (broadcast_id, resume_token) = if resume {
        let (id, token) = sh.cfg.resume_identity();
        if id.is_empty() && !sh.cfg.last_broadcast_id.is_empty() {
            log::info!("the last broadcast was on another server; starting a new one");
        }
        (id, token)
    } else {
        (String::new(), String::new())
    };

    // The platform decides mode + target BEFORE the session dial, so a
    // missing selection is an instant, local error.
    let prepared = {
        let sh = &mut *sh;
        match prepare(ui, sh) {
            Ok(p) => p,
            Err(text) => {
                ui.set_error_text(text.into());
                return;
            }
        }
    };
    sh.capture_mode = prepared.capture_mode;
    {
        let sh = &mut *sh;
        if sh.platform.remember(&mut sh.cfg) {
            save_config(sh);
        }
    }
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
    sh.loss = LossMonitor::new();
    sh.loss_notice = Notice::None;
    sh.network = None;
    ui.set_network_notice("".into());
    sh.last_error.clear();
    sh.first_viewer_seen = false;
    ui.set_error_text("".into());
    ui.set_can_mint(false);
    ui.set_busy(true);
    ui.set_live(false);
    ui.set_state_label("Starting…".into());
    ui.set_copied_note("".into());
    ui.set_summary_visible(false);
    ui.set_crash_resume(false);
    ui.set_paused(false);
    ui.set_room_left("".into());
    sh.left_room = None;
    ui.set_link_notice("".into());
    sh.foreign_telemetry = false;
    ui.set_server_note("".into());
    sh.paused_since = None;
    sh.paused_total = std::time::Duration::ZERO;
    sh.restarting = false;
    sh.restart_again = false;
    sh.reclaim_quiet = false;
    sh.resuming_from_pause = false;
    sh.pending_error = None;
    sh.reclaim_refused = None;
    // A build still finishing for an earlier broadcast is not this one's.
    sh.build_gen += 1;
    show_sharing(ui, &prepared.source, prepared.source_is_window);
    ui.set_upload_host(host_of(&sh.cfg.resolve_relay_url()).into());
    ui.set_live_quality("".into());
    ui.set_live_quality_detail("".into());
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
    let (room_code, attach, creator) = start_room(&sh.cfg, room.as_ref());
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
    loop {
        let incoming = shell.borrow().inbox.as_ref().map(|rx| rx.try_recv());
        match incoming {
            Some(Ok(inc)) => handle_incoming(ui, shell, inc),
            _ => break,
        }
    }
    // D4: a link that arrived while starting applies once the start settles.
    let queued = {
        let mut sh = shell.borrow_mut();
        if sh.state == UiState::Starting {
            None
        } else {
            sh.pending_link.take()
        }
    };
    if let Some(link) = queued {
        apply_broadcast_link(ui, shell, link);
    }
}

/// How soon after launch the first link through the inbox counts as the
/// launch's own (macOS, docs/68 D10): the Apple Event arrives within the
/// first turns of the event loop.
const COLD_LINK_WINDOW: std::time::Duration = std::time::Duration::from_secs(3);

/// A second launch or a link reached this instance (R66, docs/68 D6).
fn handle_incoming(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, inc: Incoming) {
    // macOS: a viewer link that arrives first, right after launch, is the
    // launch's own; it opens the browser and the app quits (D3).
    let cold = shell
        .borrow_mut()
        .cold_until
        .take()
        .is_some_and(|t| std::time::Instant::now() < t);
    let cold_url = cold
        .then(|| cold_viewer_url(&inc.request, &shell.borrow().cfg))
        .flatten();
    if let Some(url) = cold_url {
        log::info!("a viewer link at launch: opening the browser and quitting");
        open_in_browser(&url);
        let _ = slint::quit_event_loop();
        return;
    }
    match inc.request {
        Request::Raise => {
            log::info!("a second launch: raising the window");
            shell.borrow_mut().platform.raise_window(ui, inc.activation);
        }
        Request::Open(link) => open_link(ui, shell, link, Some(inc.activation)),
    }
}

/// A broadcast link's parts, as a state applies them (docs/68 D4).
#[derive(Debug, Clone, PartialEq, Eq)]
struct BroadcastLink {
    room: Option<String>,
    nick: Option<String>,
    /// The server the link names: its `relay=` origin, `None` for the
    /// default fleet (D1).
    relay: Option<String>,
    dropped: Vec<Dropped>,
}

/// What the link card asks (docs/68 D4, D5); its answer acts on it.
#[derive(Debug, Clone, PartialEq, Eq)]
enum LinkCard {
    /// Live or paused: join this room, and take this nickname, now.
    JoinLive {
        room: Option<String>,
        nick: Option<String>,
    },
    /// Idle: a server no profile has. "Add and switch".
    AddServer { origin: String },
}

/// A link reached the window (D3): a later launch's, with its activation
/// token in `raise`, or this launch's own (`raise` is `None`: the window is
/// about to show anyway). A viewer link goes to the browser without raising
/// the window; a broadcast link raises it and fills in the form; a rejected
/// one says why.
fn open_link(
    ui: &MainWindow,
    shell: &Rc<RefCell<Shell>>,
    raw: Result<String, ()>,
    raise: Option<Option<String>>,
) {
    let raise_window = |activation: Option<Option<String>>| {
        if let Some(token) = activation {
            shell.borrow_mut().platform.raise_window(ui, token);
        }
    };
    let parsed = raw
        .map_err(|()| "the launch named more than one link".to_string())
        .and_then(|s| link::parse(&s).map_err(|e| e.to_string()));
    let parsed = match parsed {
        Ok(p) => p,
        Err(why) => {
            log::info!("a link was rejected: {why}");
            raise_window(raise);
            ui.set_page(0);
            ui.set_link_notice(format!("This link couldn't be opened: {why}.").into());
            return;
        }
    };
    // D13: the kind only, never a broadcast ID.
    log::info!("a {} link arrived", parsed.link.kind());
    match parsed.link {
        Link::Broadcast { room, nick, relay } => {
            raise_window(raise);
            ui.set_page(0);
            apply_broadcast_link(
                ui,
                shell,
                BroadcastLink {
                    room,
                    nick,
                    relay,
                    dropped: parsed.dropped,
                },
            );
        }
        viewer @ (Link::Watch { .. } | Link::Room { .. }) => {
            let app_url = shell.borrow().cfg.resolve_app_url();
            open_in_browser(&viewer.to_https(&app_url));
        }
    }
}

/// The page a cold start's viewer link opens (docs/68 D3): such a launch
/// opens the browser and exits without showing its window. `None` for
/// every other launch.
fn cold_viewer_url(request: &Request, cfg: &Config) -> Option<String> {
    let Request::Open(Ok(raw)) = request else {
        return None;
    };
    match link::parse(raw).ok()?.link {
        Link::Broadcast { .. } => None,
        viewer => Some(viewer.to_https(&cfg.resolve_app_url())),
    }
}

/// D4: a broadcast link fills in the form while idle, waits while a start
/// settles (the latest wins), and asks first while live or paused.
fn apply_broadcast_link(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, link: BroadcastLink) {
    let state = shell.borrow().state;
    match state {
        UiState::Starting => {
            shell.borrow_mut().pending_link = Some(link);
        }
        // The crash's "resume?" offer is a paused broadcast without a
        // session: its server and room stay put too (review of #452).
        UiState::Idle if ui.get_crash_resume() => apply_link_live(ui, shell, link),
        UiState::Idle => apply_link_idle(ui, shell, link),
        UiState::Live | UiState::Paused => apply_link_live(ui, shell, link),
    }
}

fn apply_link_idle(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, link: BroadcastLink) {
    let mut filled = Vec::new();
    // D5 first, so the room's stored key is looked up on the link's server.
    let found = shell.borrow().cfg.server_for_origin(link.relay.as_deref());
    match found {
        None => {
            if shell.borrow().cfg.selected_profile().is_some() {
                select_server(ui, &mut shell.borrow_mut(), DEFAULT_SERVER_NAME);
                filled.push("the default server".to_string());
            }
        }
        Some(Ok(name)) => {
            let selected = shell
                .borrow()
                .cfg
                .selected_profile()
                .map(|p| p.name.clone());
            if selected.as_deref() != Some(name.as_str()) {
                filled.push(format!("server {name}"));
                select_server(ui, &mut shell.borrow_mut(), &name);
            }
        }
        Some(Err(())) => {
            // Until the click, the room and nickname apply on the current
            // server (D5).
            let origin = link.relay.clone().unwrap_or_default();
            show_link_card(ui, &mut shell.borrow_mut(), LinkCard::AddServer { origin });
        }
    }
    if let Some(code) = &link.room {
        choose_room(
            ui,
            shell,
            RoomInput {
                code: code.clone(),
                grant: None,
            },
        );
        filled.push(format!("room {code}"));
    }
    if let Some(nick) = &link.nick {
        set_link_nickname(ui, &mut shell.borrow_mut(), nick);
        filled.push(format!("nickname {nick}"));
    }
    ui.set_link_notice(link_notice(&filled, &link.dropped).into());
}

fn apply_link_live(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, link: BroadcastLink) {
    let mut sh = shell.borrow_mut();
    let here = sh.cfg.server_key();
    let there = link
        .relay
        .clone()
        .or_else(|| config::relay_origin(gawk_engine::defaults::RELAY_URL))
        .unwrap_or_default();
    // Never touches the broadcast's server (D4). A room on another server
    // is not the room of that code here, so nothing joins either.
    if there != here {
        let what = match &link.room {
            Some(code) => format!("This link is for room {code} on {}.", host_of(&there)),
            None => format!("This link is for {}.", host_of(&there)),
        };
        ui.set_link_notice(format!("{what} End the broadcast to switch server.").into());
        return;
    }
    // The room this broadcast is already in needs no join.
    let current = sh.room.as_ref().map(|s| s.code.clone());
    let room = link.room.filter(|c| {
        !current
            .as_deref()
            .is_some_and(|cur| cur.eq_ignore_ascii_case(c))
    });
    let nick = link.nick.filter(|n| *n != sh.cfg.nickname);
    ui.set_link_notice(link_notice(&[], &link.dropped).into());
    if room.is_some() || nick.is_some() {
        show_link_card(ui, &mut sh, LinkCard::JoinLive { room, nick });
    }
}

/// Puts the card up, replacing any earlier one: the latest link wins.
fn show_link_card(ui: &MainWindow, sh: &mut Shell, card: LinkCard) {
    let (title, body, accept, decline) = match &card {
        LinkCard::JoinLive {
            room: Some(code),
            nick,
        } => (
            format!("Join room {code} now?"),
            match nick {
                Some(n) => format!("Your broadcast joins the room now, as {n}."),
                None => "Your broadcast joins the room now.".to_string(),
            },
            "Join".to_string(),
            "Not now".to_string(),
        ),
        LinkCard::JoinLive { room: None, nick } => (
            format!("Use the nickname {} now?", nick.as_deref().unwrap_or("")),
            "The link renames your stream.".to_string(),
            "Use it".to_string(),
            "Not now".to_string(),
        ),
        LinkCard::AddServer { origin } => (
            format!("This link uses the server {}", host_of(origin)),
            "Add it and switch to it? It's saved with no publish secret; you're asked for one if it needs it.".to_string(),
            "Add and switch".to_string(),
            format!(
                "Keep {}",
                sh.cfg
                    .selected_profile()
                    .map_or("the default server".to_string(), |p| p.name.clone())
            ),
        ),
    };
    ui.set_link_card_title(title.into());
    ui.set_link_card_body(body.into());
    ui.set_link_card_accept(accept.into());
    ui.set_link_card_decline(decline.into());
    sh.link_card = Some(card);
}

fn clear_link_card(ui: &MainWindow, sh: &mut Shell) -> Option<LinkCard> {
    ui.set_link_card_title("".into());
    ui.set_link_card_body("".into());
    sh.link_card.take()
}

/// The card's first button.
fn link_card_accepted(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let card = clear_link_card(ui, &mut shell.borrow_mut());
    match card {
        Some(LinkCard::JoinLive { room, nick }) => {
            if let Some(n) = &nick {
                let mut sh = shell.borrow_mut();
                set_link_nickname(ui, &mut sh, n);
                sh.nick_timer.stop();
                flush_nickname(&mut sh);
            }
            if let Some(code) = room {
                log::info!("joining the room a link named");
                choose_room(ui, shell, RoomInput { code, grant: None });
            }
        }
        Some(LinkCard::AddServer { origin }) => {
            let mut sh = shell.borrow_mut();
            // Only while idle: a broadcast's server never changes under it.
            if sh.state != UiState::Idle {
                return;
            }
            let name = sh.cfg.add_server(&host_of(&origin), &origin);
            log::info!("added a server from a link");
            select_server(ui, &mut sh, &name);
        }
        None => {}
    }
}

/// Selects the server `name` names, as the Settings list does.
fn select_server(ui: &MainWindow, sh: &mut Shell, name: &str) {
    sh.cfg.selected_server = name.to_owned();
    save_config(sh);
    refresh_captions(ui, &sh.cfg);
    refresh_ready(ui, &sh.cfg, sh.pending_create);
    restart_probe(ui, sh);
}

/// D4, §8: the nickname goes into the field as well as the config —
/// `read_settings` reads the field back at Start.
fn set_link_nickname(ui: &MainWindow, sh: &mut Shell, nick: &str) {
    ui.set_room_nickname(nick.into());
    sh.cfg.nickname = nick.to_owned();
    save_config(sh);
}

/// The quiet line after a link (D4): what it filled in, then what it left
/// out. Empty when it did neither.
fn link_notice(filled: &[String], dropped: &[Dropped]) -> String {
    let mut out = String::new();
    if !filled.is_empty() {
        out = format!("From the link: {}.", filled.join(", "));
    }
    if !dropped.is_empty() {
        let names: Vec<String> = dropped
            .iter()
            .map(|d| match d.why {
                DropReason::Secret => format!("{} (links never carry secrets)", d.param),
                DropReason::Unknown => format!("{} (not known here)", d.param),
                DropReason::Invalid => format!("{} (not valid)", d.param),
            })
            .collect();
        if !out.is_empty() {
            out.push(' ');
        }
        out.push_str(&format!("Left out: {}.", names.join(", ")));
    }
    out
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
            adopt_media(ui, &mut sh, media);
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
        ShellMsg::Restarted { build, media } => {
            let mut sh = shell.borrow_mut();
            // An End (or a failure) while the media was building, or a
            // newer build asked for since: the session this pipeline was
            // built for has moved on.
            if build != sh.build_gen
                || sh.state != UiState::Live
                || sh.session.is_none()
                || !sh.restarting
            {
                log::warn!("restarted media came up after the broadcast moved on; discarding it");
                media.shutdown();
                return;
            }
            adopt_media(ui, &mut sh, media);
            sh.restarting = false;
            // Re-prime the relay's caches on whichever leg is up now; the
            // Resumed handler does the same for a leg that comes up later.
            if let Some(m) = sh.media() {
                m.force_idr();
            }
            log::info!("restarted on the same code");
            let again = std::mem::take(&mut sh.restart_again);
            drop(sh);
            if again {
                restart_media(ui, shell);
            }
        }
        ShellMsg::RestartFailed { build, failure: f } => {
            let mut sh = shell.borrow_mut();
            // A failure for a broadcast that moved on is not this one's.
            if build != sh.build_gen {
                log::warn!("a media build for an earlier broadcast failed; ignoring it");
                return;
            }
            sh.restarting = false;
            sh.restart_again = false;
            let app_url = sh.cfg.resolve_app_url();
            let text = message(&f, &app_url);
            log::error!("restart failed: {}", first_line(&text));
            // Nothing to send: the broadcast ends, and Ready says why.
            sh.pending_error = Some(text);
            if let Some(session) = sh.session.clone() {
                sh.rt.spawn(async move { session.stop().await });
            }
        }
        ShellMsg::Probed { url, result } => {
            let mut sh = shell.borrow_mut();
            sh.probe.in_flight = false;
            // A probe of a server that is no longer selected says nothing:
            // the selected one is probed next, at once.
            if url != sh.cfg.resolve_relay_url() {
                sh.probe.next_at = None;
                return;
            }
            sh.probe.next_at = Some(std::time::Instant::now() + PROBE_INTERVAL);
            if sh.probe.result.as_ref() != Some(&result) {
                log::info!("probe of the relay: {result:?}");
            }
            sh.probe.url = url;
            sh.probe.result = Some(result);
            render_probe(ui, &sh);
        }
        ShellMsg::Tested { url, result } => {
            let sh = shell.borrow();
            if url != edit_relay_url(&sh.cfg, &sh.edit_server) {
                return;
            }
            log::info!("test connection: {result:?}");
            let (state, text) = test_result_text(&result);
            ui.set_test_state(state);
            ui.set_test_result(text.into());
        }
        ShellMsg::Engine(ev) => handle_engine_event(ui, shell, ev),
        ShellMsg::UpdateChecked { outcome, manual } => {
            let mut sh = shell.borrow_mut();
            match &outcome {
                Outcome::Answered(Some(u)) => log::info!(
                    "update available: current {}, latest {}, {}",
                    version::RELEASE,
                    u.version,
                    u.release_url
                ),
                Outcome::Answered(None) => log::info!("update check: nothing newer"),
                Outcome::Unreachable(e) => log::info!("update check got no answer: {e}"),
            }
            let found = apply_update_outcome(&mut sh.cfg, &outcome, now_unix());
            if found.is_some() {
                save_config(&mut sh);
            }
            if sh.update.finish(found, manual) {
                ui.set_update_status(update_status_text(&outcome).into());
            }
            ui.set_update_checking(false);
            show_update(ui, sh.update.shown.as_ref());
            show_install(ui, &sh.update);
            maybe_stage(ui, &mut sh);
        }
        ShellMsg::UpdateStaged { version, result } => {
            let mut sh = shell.borrow_mut();
            sh.update.staging = false;
            match result {
                Ok(ready) => {
                    log::info!("update v{}: verified, ready to install", ready.version);
                    sh.update.ready = Some(ready);
                }
                Err(e) => {
                    log::warn!("update v{version}: not installable in place: {e}");
                    sh.update.given_up = Some(version);
                    sh.update.note = DOWNLOAD_FAILED_NOTE.into();
                }
            }
            show_install(ui, &sh.update);
        }
    }
}

fn handle_engine_event(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, ev: EngineEvent) {
    match ev {
        EngineEvent::Announce { broadcast_id } => {
            log::info!("announce: broadcast id {broadcast_id}");
            let mut sh = shell.borrow_mut();
            sh.broadcast_id = broadcast_id.clone();
            sh.cfg.last_broadcast_id = broadcast_id.clone();
            // A broadcast's server never changes while it runs.
            sh.cfg.last_broadcast_server = sh.cfg.server_key();
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
            let mut sh = shell.borrow_mut();
            let effective = sh.cfg.effective_telemetry_url(Some(&url));
            log::info!("relay advertised telemetry ingest {url}; reporting to {effective:?}");
            // docs/40 D16, docs/64 D3: on a non-default server, diagnostics
            // going to its operator's ingest is said on the strip.
            sh.foreign_telemetry =
                sh.cfg.selected_profile().is_some() && effective.as_deref() == Some(url.as_str());
            ui.set_server_note(
                if sh.foreign_telemetry {
                    FOREIGN_DIAGNOSTICS_NOTE
                } else {
                    ""
                }
                .into(),
            );
            sh.reporter.set_url(effective);
        }
        EngineEvent::Resuming { attempt } => {
            log::info!("resuming (attempt {attempt})");
            let sh = shell.borrow();
            sh.reporter.event("resuming", "");
            // A reclaim the app asked for (a resume from pause, a quick
            // restart) is not narrated (docs/64 OD4): only a second attempt
            // — something is actually wrong — shows the reconnect.
            let quiet = sh.reclaim_quiet && attempt == 1;
            drop(sh);
            if !quiet {
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
        }
        EngineEvent::Resumed => {
            log::info!("resumed");
            let mut sh = shell.borrow_mut();
            sh.reporter.event("resumed", "");
            sh.reclaim_quiet = false;
            sh.resuming_from_pause = false;
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
        EngineEvent::ReclaimRefused { status } => {
            log::warn!("the relay refused the reclaim with status {status}");
            shell.borrow_mut().reclaim_refused = Some(status);
        }
        EngineEvent::Paused => {
            log::info!("the publish leg is closed; the session holds the code");
            // No publisher, no count: the pill would state a stale fact.
            ui.set_watching_known(false);
        }

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
                let here = sh.cfg.server_key();
                sh.cfg.remember_room(&here, &s.code, &key, now_unix());
                save_config(&mut sh);
                refresh_ready(ui, &sh.cfg, sh.pending_create);
            }
            let paused = sh.state == UiState::Paused;
            let roster = roster_view(&s, &sh.broadcast_id, &app_url, s.creator, paused);
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
                let code = sh.left_room.as_ref().map(|l| l.code.clone());
                drop(sh);
                reset_room_ui(ui);
                // docs/64 D6: the Room row offers the room back.
                ui.set_room_left(code.unwrap_or_default().into());
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
        sh.cfg.room_creator_token.clear();
        sh.cfg.room_server.clear();
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
fn choose_room(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, input: RoomInput) {
    let mut sh = shell.borrow_mut();
    let (attach, creator) = store_room_choice(&mut sh.cfg, &input);
    sh.pending_create = false;
    // A room chosen replaces the one just left (docs/64 D6).
    sh.left_room = None;
    ui.set_room_left("".into());
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

/// The room a start joins and what it presents: (code, attach key, creator
/// token hex). docs/68 D5a, the choke point: stored credentials go only to
/// the server they were stored for; otherwise none do, and the relay's own
/// prompt asks. A grant in a stored room link is the user's, for this
/// server.
fn start_room(cfg: &Config, room: Option<&RoomInput>) -> (String, String, String) {
    let Some(RoomInput { code, grant }) = room else {
        return (String::new(), String::new(), String::new());
    };
    let (stored_attach, stored_creator) = cfg.pending_room_credentials();
    if stored_attach.is_empty()
        && stored_creator.is_empty()
        && (!cfg.room_attach_secret.is_empty() || !cfg.room_creator_token.is_empty())
    {
        log::info!("the room's stored credentials belong to another server; not presented");
    }
    let attach = match grant {
        Some(RoomGrant::Attach(k)) => k.clone(),
        _ => stored_attach,
    };
    let creator = match grant {
        Some(RoomGrant::Creator(hex)) => hex.clone(),
        _ => stored_creator,
    };
    (code.clone(), attach, creator)
}

/// Records the chosen room as the one the next broadcast joins, and
/// returns its (attach key, creator token hex).
///
/// `room` is stored in the clear, so it holds only the code: a pasted
/// link's grant goes to the wrapped `roomAttachSecret` or
/// `roomCreatorToken` (review of #381).
fn store_room_choice(cfg: &mut Config, input: &RoomInput) -> (String, String) {
    // docs/68 D5a: a key on file is reused only if it was stored for the
    // selected server, and what is stored now is bound to it.
    let here = cfg.server_key();
    let attach = match &input.grant {
        Some(RoomGrant::Attach(k)) => k.clone(),
        _ => cfg
            .room_attach_key(&here, &input.code)
            .unwrap_or_default()
            .to_owned(),
    };
    let creator = match &input.grant {
        Some(RoomGrant::Creator(hex)) => hex.clone(),
        _ => String::new(),
    };
    cfg.room = input.code.clone();
    cfg.room_attach_secret = attach.clone();
    cfg.room_creator_token = creator.clone();
    cfg.room_server = here;
    (attach, creator)
}

/// "Create a new room" (docs/60 D8), for the config: the new room replaces
/// the stored one whether live or not, so the next go-live doesn't return
/// to the old room. A create is one-shot, so nothing takes its place.
/// Returns whether it is a pending create (not live).
fn store_room_create(cfg: &mut Config, live: bool) -> bool {
    cfg.room.clear();
    cfg.room_attach_secret.clear();
    cfg.room_creator_token.clear();
    cfg.room_server.clear();
    !live
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
}

/// The error an ending shows, and whether Ready offers a new code instead
/// (docs/64 D9). A resume from pause the relay refused with 404 means the
/// relay let the paused code go: said as that, with a new code one click
/// away.
fn ending_error(
    error: Option<String>,
    resuming_from_pause: bool,
    refused: Option<u16>,
    code: &str,
) -> (Option<String>, bool) {
    // Only a 404 is "gone": 401/403/409/451 keep their own reason, and a
    // relay unreachable for the whole window is a loss, not an expiry.
    if error.is_some() && resuming_from_pause && refused == Some(404) {
        return (
            Some(format!(
                "The server no longer holds {code}: a paused broadcast is kept for a few \
                 minutes. Go live for a new code."
            )),
            true,
        );
    }
    (error, false)
}

fn end_broadcast(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, error: Option<String>) {
    let mut sh = shell.borrow_mut();
    let was_live = matches!(sh.state, UiState::Live | UiState::Paused);
    // A resume from pause the relay refused: the paused code is gone
    // (docs/64 D9). Said as that, with a new code one click away.
    let pending = sh.pending_error.take();
    let (error, paused_code_gone) = ending_error(
        error.or(pending),
        sh.resuming_from_pause,
        sh.reclaim_refused.take(),
        &sh.broadcast_id,
    );
    // The summary card (docs/64 D10), read before the session goes: time
    // live, not time paused.
    let summary = sh.live_since.map(|since| {
        let st = merged_stats(&sh);
        let secs = live_secs(
            since.elapsed(),
            sh.paused_total,
            sh.paused_since.map(|p| p.elapsed()),
        );
        let (_, h, fps) = sh.last_sent.unwrap_or((st.width, st.height, st.fps));
        summary_rows(
            sh.peak_viewers,
            st.bytes_sent + st.audio_bytes_sent,
            secs,
            h,
            fps,
        )
    });
    sh.live_since = None;
    sh.paused_since = None;
    sh.paused_total = std::time::Duration::ZERO;
    sh.restarting = false;
    sh.restart_again = false;
    sh.reclaim_quiet = false;
    sh.resuming_from_pause = false;
    sh.pending_error = None;
    sh.last_sent = None;
    sh.left_room = None;
    sh.foreign_telemetry = false;
    sh.quality_timer.stop();
    // A build still running for this broadcast finishes for nobody.
    sh.build_gen += 1;
    // Back on Ready: the header's status looks at the relay again now.
    sh.probe.next_at = None;
    // Ended inside the app: no resume question at the next launch (D12).
    sh.cfg.was_live = false;
    save_config(&mut sh);
    sh.state = UiState::Idle;
    sh.session = None;
    sh.media_info = None;
    if let Some(m) = sh.media.take() {
        m.shutdown();
    }
    // Ending from Paused has no media to shut down: the platform lets go of
    // what a pause handed back (review of #423).
    {
        let sh = &mut *sh;
        sh.platform.broadcast_ended(ui);
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

    // A clean end shows the summary card, with its undo; an error shows the
    // error card on the plain Ready page.
    match (&summary, &error) {
        (Some(rows), None) => {
            ui.set_summary_rows(ModelRc::new(VecModel::from(rows.clone())));
            ui.set_summary_visible(true);
        }
        _ => ui.set_summary_visible(false),
    }

    ui.set_busy(false);
    ui.set_live(false);
    ui.set_paused(false);
    ui.set_paused_elapsed("".into());
    ui.set_room_left("".into());
    ui.set_server_note("".into());
    ui.set_live_quality("".into());
    ui.set_live_quality_detail("".into());
    ui.set_resuming(false);
    ui.set_state_label("Not broadcasting".into());
    ui.set_status_line("".into());
    ui.set_watching(0);
    ui.set_watching_known(false);
    ui.set_live_elapsed("".into());
    ui.set_connection_line("".into());
    ui.set_encode_line("".into());
    ui.set_audio_line("".into());
    ui.set_audio_hint(false);
    ui.set_show_thumbnail(false);
    ui.set_minimized_hint("".into());
    ui.set_uplink_warning("".into());
    ui.set_network_notice("".into());
    if let Some(e) = &error {
        ui.set_error_text(e.clone().into());
        ui.set_can_mint(paused_code_gone);
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

// --- R62 (docs/64): pause, the quick restart, the probe -----------------------

/// How long a quality change while live settles before it applies: a
/// click, a drag or a typed size becomes one restart (docs/64 D13).
const QUALITY_SETTLE: std::time::Duration = std::time::Duration::from_millis(1200);

/// Seconds live: the broadcast's age minus its pauses.
fn live_secs(
    elapsed: std::time::Duration,
    paused_total: std::time::Duration,
    current_pause: Option<std::time::Duration>,
) -> u64 {
    elapsed
        .saturating_sub(paused_total)
        .saturating_sub(current_pause.unwrap_or_default())
        .as_secs()
}

/// Live's Sharing row (docs/64 D7): what is being sent.
fn show_sharing(ui: &MainWindow, source: &str, window: bool) {
    ui.set_sharing_title(source.into());
    ui.set_sharing_detail(
        if window {
            "Window"
        } else if source.is_empty() {
            ""
        } else {
            "Display"
        }
        .into(),
    );
    ui.set_sharing_window(window);
}

/// Takes a built pipeline into the running broadcast: the caches it
/// settles, and the lines that describe it.
fn adopt_media(ui: &MainWindow, sh: &mut Shell, media: Box<dyn Media>) {
    let info = media.info().clone();
    // Cache the accepted encoder for next launch (D9).
    if sh.cfg.last_good_encoder != info.encoder {
        sh.cfg.last_good_encoder = info.encoder.clone();
        save_config(sh);
    }
    // And, on Linux, the audio cascade's winner (docs/58 D7).
    if let Some(src) = media.audio_source_to_cache()
        && sh.cfg.last_good_audio_source != src
    {
        sh.cfg.last_good_audio_source = src;
        save_config(sh);
    }
    let (_, _, fps, bps) = sh.cfg.resolve_rung();
    ui.set_encode_line(
        format!(
            "{} — {} · {} · {}×{}@{}",
            info.family, info.encoder, info.capture_path, info.width, info.height, fps
        )
        .into(),
    );
    let (quality, detail) = live_quality_rows(info.height, fps, bps);
    ui.set_live_quality(quality.into());
    ui.set_live_quality_detail(detail.into());
    ui.set_show_thumbnail(info.show_thumbnail);
    sh.last_sent = Some((info.width, info.height, fps));
    sh.media_info = Some(info);
    sh.media = Some(media);
}

/// Pause (docs/64 D9): the media goes (keeping the platform's source), the
/// session closes its publish leg and holds the code, and the room stays.
fn pause_broadcast(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let mut guard = shell.borrow_mut();
    let sh = &mut *guard;
    if sh.state != UiState::Live || sh.restarting {
        return;
    }
    let Some(session) = sh.session.clone() else {
        return;
    };
    sh.quality_timer.stop();
    if let Some(m) = sh.media.take()
        && let Some(kept) = m.shutdown_keep_source()
    {
        sh.platform.source_returned(ui, kept);
    }
    sh.media_info = None;
    session.pause();
    sh.state = UiState::Paused;
    sh.paused_since = Some(std::time::Instant::now());
    sh.reporter.event("paused", "");
    log::info!("paused (broadcast id {:?})", sh.broadcast_id);
    drop(guard);
    ui.set_paused(true);
    ui.set_live(false);
    ui.set_state_label("Paused".into());
    ui.set_paused_elapsed(format_elapsed(0).into());
    ui.set_resuming(false);
    ui.set_status_line("".into());
    ui.set_audio_hint(false);
    ui.set_minimized_hint("".into());
    ui.set_uplink_warning("".into());
    ui.set_network_notice("".into());
    ui.set_error_text("".into());
}

/// Resume from a pause: what the rows say now (they could all change), on
/// the same code, in the same room.
fn resume_from_pause(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let mut guard = shell.borrow_mut();
    let sh = &mut *guard;
    if sh.state != UiState::Paused {
        return;
    }
    let Some(session) = sh.session.clone() else {
        return;
    };
    let prepared = match prepare(ui, sh) {
        Ok(p) => p,
        Err(text) => {
            // Still paused; the page says what to choose.
            ui.set_error_text(text.into());
            return;
        }
    };
    if let Some(since) = sh.paused_since.take() {
        sh.paused_total += since.elapsed();
    }
    sh.state = UiState::Live;
    sh.resuming_from_pause = true;
    sh.reporter.event("unpaused", "");
    log::info!("resuming from pause");
    drop(guard);
    ui.set_error_text("".into());
    ui.set_paused(false);
    ui.set_paused_elapsed("".into());
    ui.set_live(true);
    ui.set_state_label("Live".into());
    republish(ui, shell, session, prepared);
}

/// Whether a broadcast is on air: starting or live, including while a
/// restart or a resume builds its new media and there is none — not Idle,
/// not Paused. The platforms read it when their own picker hands them a new
/// source: on air, the pick must switch the broadcast (a restart request),
/// because the running build already took the old one (review of #423).
/// Paused, Resume takes the pick and nothing restarts.
pub fn on_air(ui: &MainWindow) -> bool {
    ui.get_busy() && !ui.get_paused()
}

/// The quick restart (docs/64 D8): the running broadcast switches to what
/// the source and quality settings say now, on the same code. Not
/// narrated; a request during one runs after it.
pub fn restart_media(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let mut guard = shell.borrow_mut();
    let sh = &mut *guard;
    if sh.state != UiState::Live {
        return;
    }
    if sh.restarting {
        sh.restart_again = true;
        return;
    }
    let Some(session) = sh.session.clone() else {
        return;
    };
    sh.quality_timer.stop();
    if let Some(m) = sh.media.take()
        && let Some(kept) = m.shutdown_keep_source()
    {
        sh.platform.source_returned(ui, kept);
    }
    sh.media_info = None;
    let prepared = match prepare(ui, sh) {
        Ok(p) => p,
        Err(text) => {
            // Nothing to capture any more: the broadcast ends, saying why.
            log::error!("restart has no source: {text}");
            sh.pending_error = Some(text);
            sh.rt.spawn(async move { session.stop().await });
            return;
        }
    };
    sh.reporter.event("restart", "");
    log::info!("restarting on the same code");
    drop(guard);
    republish(ui, shell, session, prepared);
}

/// The shared half of a resume from pause and a quick restart: a new
/// lineage on the sender, a fresh publish leg under the same identity, and
/// the new media built off the GUI thread.
fn republish(
    ui: &MainWindow,
    shell: &Rc<RefCell<Shell>>,
    session: Arc<Session>,
    prepared: Prepared,
) {
    let mut guard = shell.borrow_mut();
    let sh = &mut *guard;
    sh.capture_mode = prepared.capture_mode;
    if sh.platform.remember(&mut sh.cfg) {
        save_config(sh);
    }
    if let Some(key) = current_source_key(ui) {
        sh.cfg.last_source = key;
        save_config(sh);
    }
    show_sharing(ui, &prepared.source, prepared.source_is_window);
    // The watchdogs start over with the new media.
    sh.uplink = gawk_engine::uplink::UplinkMonitor::new();
    sh.uplink_warned = false;
    sh.loss = LossMonitor::new();
    sh.loss_notice = Notice::None;
    ui.set_uplink_warning("".into());
    ui.set_network_notice("".into());
    sh.restarting = true;
    sh.reclaim_quiet = true;
    // A new pipeline is a new lineage on the same broadcast: its codec and
    // audio format describe it, not the last one (docs/64 D8).
    session.sender().new_lineage();
    session.republish();
    sh.build_gen += 1;
    let build = sh.build_gen;
    let tx = sh.msg_tx.clone();
    let clock: Arc<dyn gawk_engine::clock::Clock> = sh.clock.clone();
    let rt = sh.rt.handle().clone();
    let env = MediaEnv {
        sender: session.sender(),
        clock,
        rt,
    };
    drop(guard);
    std::thread::spawn(move || {
        // catch_unwind, as at Start: a panic in the bring-up must end the
        // broadcast visibly, not leave it live with no media.
        let built =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| (prepared.build)(env)));
        let msg = match built {
            Ok(Ok(media)) => ShellMsg::Restarted { build, media },
            Ok(Err(failure)) => ShellMsg::RestartFailed { build, failure },
            Err(_) => ShellMsg::RestartFailed {
                build,
                failure: StartFailure::Capture(
                    "the media pipeline crashed while restarting (a bug — the details are in the debug log)"
                        .into(),
                ),
            },
        };
        let _ = tx.send(msg);
    });
}

/// Whether the header's probe should run now.
fn probe_due(p: &ProbeState, now: std::time::Instant) -> bool {
    !p.in_flight && p.next_at.is_none_or(|t| t <= now)
}

/// Probes the selected relay in the background (docs/64 D1).
fn run_probe(ui: &MainWindow, sh: &mut Shell) {
    let url = sh.cfg.resolve_relay_url();
    if url != sh.probe.url {
        sh.probe.url = url.clone();
        sh.probe.result = None;
        render_probe(ui, sh);
    }
    sh.probe.in_flight = true;
    let origin = sh.cfg.resolve_origin();
    let tx = sh.msg_tx.clone();
    sh.rt.spawn(async move {
        let result = gawk_engine::probe::probe(&url, &origin, false).await;
        let _ = tx.send(ShellMsg::Probed { url, result });
    });
}

/// The selected server changed, or Try again was pressed: back to
/// "Checking…", and a probe as soon as nothing else is in flight.
fn restart_probe(ui: &MainWindow, sh: &mut Shell) {
    sh.probe.url = sh.cfg.resolve_relay_url();
    sh.probe.result = None;
    sh.probe.next_at = None;
    render_probe(ui, sh);
    if sh.state == UiState::Idle && !sh.probe.in_flight {
        run_probe(ui, sh);
    }
}

/// The header's status line, the unreachable banner's host and the server
/// list's note, from the probe.
fn render_probe(ui: &MainWindow, sh: &Shell) {
    let (state, rtt) = match &sh.probe.result {
        None => (0, String::new()),
        Some(ProbeResult::Ok { rtt_ms, .. }) => (1, format!("{rtt_ms} ms")),
        Some(ProbeResult::Failed) => (2, String::new()),
    };
    ui.set_probe_state(state);
    ui.set_probe_rtt(rtt.into());
    ui.set_probe_host(host_of(&sh.cfg.resolve_relay_url()).into());
    seed_server_list(ui, &sh.cfg, &probe_note(&sh.probe));
}

/// The relay the Edit server page's Test connection dials: the edited
/// profile's (empty until it has an address), or the pinned default's.
fn edit_relay_url(cfg: &Config, edit: &str) -> String {
    match custom_profiles(cfg).into_iter().find(|p| p.name == edit) {
        Some(p) if p.url.trim().is_empty() => String::new(),
        Some(p) => config::resolve_relay_url(&p.url),
        None => config::resolve_relay_url(""),
    }
}

/// Test connection's result belongs to the address it tested: an edit that
/// moves the address clears it — and re-enables the button a test still
/// running had disabled, since that test's late answer is then discarded as
/// stale (review of #423).
fn clear_test_if_moved(ui: &MainWindow, before: &str, after: &str) {
    if before != after {
        ui.set_test_state(0);
        ui.set_test_result("".into());
    }
}

/// Test connection's line: (2 reachable | 3 can't reach, the sentence). The
/// relay's own name is its operator's claim, so it is quoted beside the
/// facts, never in place of the address (docs/40 F6).
fn test_result_text(r: &ProbeResult) -> (i32, String) {
    match r {
        ProbeResult::Ok {
            rtt_ms,
            name: Some(n),
        } => (2, format!("Reachable · {rtt_ms} ms · calls itself “{n}”")),
        ProbeResult::Ok { rtt_ms, name: None } => (2, format!("Reachable · {rtt_ms} ms")),
        ProbeResult::Failed => (
            3,
            "Can't reach this server. Check the address, and that UDP to its port isn't blocked."
                .into(),
        ),
    }
}

/// What Rejoin needs of a room just left: its code and the grant it was
/// joined with — the creator token, else the attach key.
fn left_room(code: &str, key_used: &str, grant: Option<&RoomGrant>) -> Option<LeftRoom> {
    if code.is_empty() {
        return None;
    }
    let creator = match grant {
        Some(RoomGrant::Creator(hex)) => hex.clone(),
        _ => String::new(),
    };
    let attach = match grant {
        Some(RoomGrant::Attach(k)) => k.clone(),
        _ => key_used.to_owned(),
    };
    Some(LeftRoom {
        code: code.to_owned(),
        attach,
        creator,
    })
}

/// Rejoin's room choice, as if the link were pasted again.
fn rejoin_input(l: &LeftRoom) -> RoomInput {
    let grant = if !l.creator.is_empty() {
        Some(RoomGrant::Creator(l.creator.clone()))
    } else if !l.attach.is_empty() {
        Some(RoomGrant::Attach(l.attach.clone()))
    } else {
        None
    };
    RoomInput {
        code: l.code.clone(),
        grant,
    }
}

/// The 1 Hz working tick while broadcasting: stats rows, telemetry sample,
/// thumbnail, minimized hint, audio line, pipeline failure surfacing.
fn tick(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let log_health;
    {
        let mut sh = shell.borrow_mut();
        update_preview(ui, &mut sh);
        if sh.state == UiState::Idle {
            // R47: an update found while live downloads once idle again.
            maybe_stage(ui, &mut sh);
            // Idle: the header's status (docs/64 D1), on its own schedule.
            if probe_due(&sh.probe, std::time::Instant::now()) {
                run_probe(ui, &mut sh);
            }
            return;
        }
        if sh.state == UiState::Paused {
            // Paused: nothing is sent, so there is nothing to measure — the
            // badge's clock is all that moves.
            if let Some(since) = sh.paused_since {
                ui.set_paused_elapsed(format_elapsed(since.elapsed().as_secs()).into());
            }
            return;
        }
        sh.stats_countdown = sh.stats_countdown.saturating_sub(1);
        if sh.stats_countdown > 0 {
            return;
        }
        sh.stats_countdown = 4; // 4 × 250 ms = 1 s
        if sh.restarting {
            // A quick restart has no media for a moment: the watchdogs would
            // read that as a failing uplink. Only the clock moves.
            if let Some(since) = sh.live_since {
                let secs = live_secs(since.elapsed(), sh.paused_total, None);
                ui.set_live_elapsed(format_elapsed(secs).into());
            }
            return;
        }
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

    // Loss in the air (docs/57 D7): the platform says what the broadcast
    // leaves on, the policy says whether viewers are paying for it.
    {
        let mut guard = shell.borrow_mut();
        let sh = &mut *guard;
        let relay = sh.session.as_ref().and_then(|s| s.relay_address());
        let facts = relay.and_then(|a| sh.platform.network_facts(a));
        if facts != sh.network {
            log::info!("network: {facts:?}");
            sh.network = facts;
        }
        let was_raised = sh.loss.raised();
        let notice = sh.loss.observe(&st, facts);
        if sh.loss.raised() != was_raised {
            let (lost, secs) = sh.loss.window_loss();
            let (level, what) = if sh.loss.raised() {
                (log::Level::Warn, "raised")
            } else {
                (log::Level::Info, "cleared")
            };
            log::log!(
                level,
                "uplink loss {what}: {lost} packets lost in the last {secs} s ({} lost of {} sent this broadcast), {facts:?}",
                st.uplink_packets_lost,
                st.uplink_packets_sent,
            );
        }
        sh.loss_notice = notice;
        // The bandwidth line speaks first: when the upload can't keep up,
        // that is the remedy to read. Neither can be dismissed: while
        // viewers are losing video, the broadcaster sees it (docs/57 OD7).
        let show = notice != Notice::None && !sh.uplink_warned;
        // One line in the alert slot, with Help beside it (docs/66 D4).
        ui.set_network_notice(if show { notice.short_text() } else { "" }.into());
    }

    // The remainder needs the shell only for the pipeline's widgets.
    let sh = shell.borrow();
    if log_health {
        log::info!(
            "health: capture {} fps, encode {:.1} fps, sent {:.1} fps, keyframe streams {} sent / {} superseded / {} failed, frames dropped at send {}, packets lost {} of {}, viewers {}, audio {}",
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
            st.uplink_packets_lost,
            st.uplink_packets_sent,
            st.viewer_count,
            st.audio_state
        );
    }

    ui.set_stats_rows(ModelRc::new(VecModel::from(stat_rows(
        &st, &sh.loss, sh.network,
    ))));
    if let Some(since) = sh.live_since {
        let secs = live_secs(since.elapsed(), sh.paused_total, None);
        ui.set_live_elapsed(format_elapsed(secs).into());
    }
    let bytes = st.bytes_sent + st.audio_bytes_sent;
    let rate_bps = bytes.saturating_sub(sh.last_bytes) * 8;
    ui.set_connection_line(format!("{:.1} Mbps", rate_bps as f64 / 1e6).into());
    ui.set_connection_ok(!sh.uplink_warned && sh.loss_notice == Notice::None);
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
            st.capture_restarts = p.capture_restarts();
            st.share_mode = p.share_mode().unwrap_or_default().to_owned();
            st.audio_app = p.audio_app().unwrap_or_default();
        }
    }
    if st.audio_state.is_empty() {
        st.audio_state = "off".into();
    }
    st
}

fn percent(part: u64, whole: u64) -> f64 {
    if whole == 0 {
        0.0
    } else {
        part as f64 * 100.0 / whole as f64
    }
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

fn stat_rows(
    st: &gawk_engine::stats::Stats,
    loss: &LossMonitor,
    network: Option<NetworkFacts>,
) -> Vec<StatRow> {
    let row = |label: &str, value: String| StatRow {
        label: label.into(),
        value: value.into(),
    };
    let na = || "n/a".to_string();
    let mut rows = base_stat_rows(st, loss, network, &row, &na);
    // Linux only (docs/58 D6, D16): the portal path reports its share mode,
    // and a rebuilt capture is the one place a viewer's freeze is recorded.
    if !st.share_mode.is_empty() {
        rows.insert(
            3,
            row("Capture rebuilds", format!("{}", st.capture_restarts)),
        );
        if !st.audio_app.is_empty() {
            rows.push(row("App audio", st.audio_app.clone()));
        }
    }
    rows
}

fn base_stat_rows(
    st: &gawk_engine::stats::Stats,
    loss: &LossMonitor,
    network: Option<NetworkFacts>,
    row: &dyn Fn(&str, String) -> StatRow,
    na: &dyn Fn() -> String,
) -> Vec<StatRow> {
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
            "Packets lost",
            if st.uplink_packets_available {
                let (lost, secs) = loss.window_loss();
                format!(
                    "{lost} in the last {secs} s · {} of {} ({:.2} %)",
                    st.uplink_packets_lost,
                    st.uplink_packets_sent,
                    percent(st.uplink_packets_lost, st.uplink_packets_sent)
                )
            } else {
                na()
            },
        ),
        row(
            "Network",
            match network {
                Some(n) => format!(
                    "{}{}{}",
                    if n.wifi { "Wi-Fi" } else { "not Wi-Fi" },
                    if n.vpn { " (via VPN)" } else { "" },
                    if n.wifi {
                        if n.awdl_up {
                            " · AWDL up"
                        } else {
                            " · AWDL down"
                        }
                    } else {
                        ""
                    }
                ),
                None => na(),
            },
        ),
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
    // A test records the page rather than opening a browser.
    #[cfg(test)]
    tests::OPENED.with(|o| o.borrow_mut().push(url.to_owned()));
    // Not `cmd /c start`: cmd reads `&` in a link's query as a command
    // separator (docs/68 §12). url.dll hands the URL to the shell verbatim.
    #[cfg(all(windows, not(test)))]
    let _ = std::process::Command::new("rundll32.exe")
        .args(["url.dll,FileProtocolHandler", url])
        .spawn();
    #[cfg(all(target_os = "macos", not(test)))]
    let _ = std::process::Command::new("open").arg(url).spawn();
    #[cfg(all(unix, not(target_os = "macos"), not(test)))]
    let _ = std::process::Command::new("xdg-open").arg(url).spawn();
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{MonitorRow, WindowRow};
    use i_slint_backend_testing::ElementHandle;
    use slint::platform::{PointerEventButton, WindowEvent};
    use std::cell::Cell;
    use std::time::Duration;

    /// The default fleet's server key, which room credentials bind to.
    const S: &str = "https://api.gawk.ioio.fi:4433";

    thread_local! {
        /// What `open_in_browser` was asked to open, in a test.
        pub(super) static OPENED: RefCell<Vec<String>> = const { RefCell::new(Vec::new()) };
    }

    fn opened() -> Vec<String> {
        OPENED.with(|o| o.borrow_mut().drain(..).collect())
    }

    // --- R66: gawk:// links (docs/68 D3–D7) ---------------------------------

    /// A later launch's link, as the instance endpoint delivers it.
    fn warm(ui: &MainWindow, shell: &Rc<RefCell<Shell>>, link: &str) {
        handle_incoming(
            ui,
            shell,
            Incoming {
                request: Request::Open(Ok(link.into())),
                activation: Some("tok".into()),
            },
        );
    }

    fn friend_cfg() -> Config {
        Config {
            servers: vec![ServerProfile {
                name: "Friend".into(),
                url: "https://relay.friend.example".into(),
                publish_secret: "s".into(),
            }],
            ..Default::default()
        }
    }

    fn shell_with(cfg: Config) -> (Rc<RefCell<Shell>>, Rc<RefCell<Vec<String>>>) {
        let (shell, _, calls) = counting_shell();
        shell.borrow_mut().cfg = cfg;
        (shell, calls)
    }

    // D4, Idle: the room becomes the pending one, the nickname fills the
    // field and the config, nothing starts, and a notice says what changed.
    #[test]
    fn an_idle_broadcast_link_fills_in_the_form_and_raises_the_window() {
        let ui = window();
        let (shell, calls) = shell_with(Config::default());
        ui.set_page(2);
        warm(
            &ui,
            &shell,
            "gawk://broadcast?room=lan-party&nick=Juho&rt=a%3Ak",
        );
        let sh = shell.borrow();
        assert_eq!(sh.state, UiState::Idle);
        assert!(sh.session.is_none());
        assert_eq!(sh.cfg.room, "lan-party");
        assert_eq!(sh.cfg.room_attach_secret, "", "a link never carries a key");
        assert_eq!(sh.cfg.nickname, "Juho");
        assert_eq!(ui.get_room_nickname(), "Juho");
        assert_eq!(ui.get_room_pending(), "lan-party");
        assert_eq!(ui.get_page(), 0);
        assert_eq!(*calls.borrow(), ["raise(tok)"]);
        let notice = ui.get_link_notice().to_string();
        assert!(
            notice.contains("room lan-party") && notice.contains("nickname Juho"),
            "{notice}"
        );
        assert!(
            notice.contains("rt (links never carry secrets)"),
            "{notice}"
        );
        assert_eq!(ui.get_link_card_title(), "");
    }

    // §8: read_settings reads the nickname field at Start, so a link that
    // set only the config would lose it.
    #[test]
    fn the_links_nickname_survives_a_later_start() {
        let ui = window();
        let (shell, _) = shell_with(Config::default());
        warm(&ui, &shell, "gawk://broadcast?nick=Juho");
        start_broadcast(&ui, &shell, false);
        assert_eq!(shell.borrow().cfg.nickname, "Juho");
    }

    // D4, Starting: the link waits for the start to settle, and the latest
    // one wins.
    #[test]
    fn a_link_while_starting_waits_and_the_latest_wins() {
        let ui = window();
        let (shell, _) = shell_with(Config::default());
        shell.borrow_mut().state = UiState::Starting;
        warm(&ui, &shell, "gawk://broadcast?room=first");
        warm(&ui, &shell, "gawk://broadcast?room=second&nick=Two");
        pump_messages(&ui, &shell);
        assert_eq!(shell.borrow().cfg.room, "");
        shell.borrow_mut().state = UiState::Idle;
        pump_messages(&ui, &shell);
        let sh = shell.borrow();
        assert_eq!(
            (sh.cfg.room.as_str(), sh.cfg.nickname.as_str()),
            ("second", "Two")
        );
        assert!(sh.pending_link.is_none());
    }

    // D4, Live/Paused: nothing changes without a click; Join applies the
    // room and the nickname, Not now leaves everything alone.
    #[test]
    fn a_link_while_live_asks_before_joining() {
        let ui = window();
        for state in [UiState::Live, UiState::Paused] {
            let (shell, calls) = shell_with(Config::default());
            shell.borrow_mut().state = state;
            warm(&ui, &shell, "gawk://broadcast?room=lan-party&nick=Juho");
            assert_eq!(shell.borrow().cfg.room, "");
            assert_eq!(shell.borrow().cfg.nickname, "");
            assert_eq!(ui.get_link_card_title(), "Join room lan-party now?");
            assert!(ui.get_link_card_body().contains("as Juho"));
            assert_eq!(*calls.borrow(), ["raise(tok)"]);

            link_card_accepted(&ui, &shell);
            let sh = shell.borrow();
            assert_eq!(
                (sh.cfg.room.as_str(), sh.cfg.nickname.as_str()),
                ("lan-party", "Juho")
            );
            assert_eq!(ui.get_room_nickname(), "Juho");
            assert_eq!(ui.get_link_card_title(), "");
            assert!(sh.link_card.is_none());
        }

        let (shell, _) = shell_with(Config::default());
        shell.borrow_mut().state = UiState::Live;
        warm(&ui, &shell, "gawk://broadcast?room=lan-party");
        clear_link_card(&ui, &mut shell.borrow_mut());
        assert_eq!(ui.get_link_card_title(), "");
        assert_eq!(shell.borrow().cfg.room, "");
    }

    #[test]
    fn a_nickname_only_link_while_live_asks_too() {
        let ui = window();
        let (shell, _) = shell_with(Config::default());
        shell.borrow_mut().state = UiState::Live;
        warm(&ui, &shell, "gawk://broadcast?nick=Juho");
        assert_eq!(ui.get_link_card_title(), "Use the nickname Juho now?");
        assert_eq!(shell.borrow().cfg.nickname, "");
        link_card_accepted(&ui, &shell);
        assert_eq!(shell.borrow().cfg.nickname, "Juho");
    }

    // D4: a live broadcast's server never changes; a link for another server
    // says so and joins nothing.
    #[test]
    fn a_link_for_another_server_while_live_only_says_so() {
        let ui = window();
        let (shell, _) = shell_with(friend_cfg());
        shell.borrow_mut().state = UiState::Live;
        warm(
            &ui,
            &shell,
            "gawk://broadcast?room=lan-party&relay=https%3A%2F%2Frelay.friend.example",
        );
        assert_eq!(ui.get_link_card_title(), "");
        assert!(
            ui.get_link_notice()
                .contains("End the broadcast to switch server")
        );
        let sh = shell.borrow();
        assert!(sh.cfg.selected_profile().is_none());
        assert_eq!(sh.cfg.room, "");
    }

    // Review of #452: while the crash's "resume?" offer is up, the paused
    // broadcast's server must not change under it, or Resume would present
    // its reclaim token to another relay. A link follows the Paused rules.
    #[test]
    fn a_link_never_switches_server_under_a_crash_resume_offer() {
        let ui = window();
        let (shell, _) = shell_with(Config {
            was_live: true,
            last_broadcast_id: "K7XQ2M".into(),
            last_resume_token: "aa11".into(),
            last_broadcast_server: S.into(),
            ..friend_cfg()
        });
        ui.set_crash_resume(true);
        ui.set_paused(true);
        warm(
            &ui,
            &shell,
            "gawk://broadcast?room=lan-party&relay=https%3A%2F%2Frelay.friend.example",
        );
        warm(
            &ui,
            &shell,
            "gawk://broadcast?relay=https%3A%2F%2Fnew.example",
        );
        assert_eq!(ui.get_link_card_title(), "");
        assert!(ui.get_link_notice().contains("switch server"));
        let sh = shell.borrow();
        assert!(sh.cfg.selected_profile().is_none(), "still the default");
        assert_eq!(sh.cfg.servers.len(), 1, "nothing added");
        assert_eq!(sh.cfg.room, "");
        assert_eq!(sh.cfg.resume_identity().0, "K7XQ2M");
        drop(sh);

        // The same server's link asks before changing the room Resume rejoins.
        warm(&ui, &shell, "gawk://broadcast?room=lan-party");
        assert_eq!(ui.get_link_card_title(), "Join room lan-party now?");
        assert_eq!(shell.borrow().cfg.room, "");
    }

    // D5: no relay= means the default fleet; a saved server's origin selects
    // it; an unknown one asks, and the room and nickname apply meanwhile.
    #[test]
    fn a_links_server_selects_the_default_a_saved_one_or_asks() {
        let ui = window();
        let (shell, _) = shell_with(Config {
            selected_server: "Friend".into(),
            ..friend_cfg()
        });
        warm(&ui, &shell, "gawk://broadcast?room=abc");
        assert!(
            shell.borrow().cfg.selected_profile().is_none(),
            "default selected"
        );
        assert!(ui.get_link_notice().contains("the default server"));

        warm(
            &ui,
            &shell,
            "gawk://broadcast?relay=https%3A%2F%2FRelay.Friend.example%2F",
        );
        assert_eq!(shell.borrow().cfg.selected_server, "Friend");
        assert_eq!(ui.get_link_card_title(), "");

        warm(
            &ui,
            &shell,
            "gawk://broadcast?room=other&nick=N&relay=https%3A%2F%2Fnew.example%3A4433",
        );
        assert_eq!(
            ui.get_link_card_title(),
            "This link uses the server new.example:4433"
        );
        assert_eq!(ui.get_link_card_decline(), "Keep Friend");
        {
            let sh = shell.borrow();
            assert_eq!(
                sh.cfg.selected_server, "Friend",
                "nothing switches without a click"
            );
            assert_eq!(
                (sh.cfg.room.as_str(), sh.cfg.nickname.as_str()),
                ("other", "N")
            );
            assert_eq!(sh.cfg.servers.len(), 1);
        }
        clear_link_card(&ui, &mut shell.borrow_mut());
        assert_eq!(shell.borrow().cfg.servers.len(), 1);

        warm(
            &ui,
            &shell,
            "gawk://broadcast?relay=https%3A%2F%2Fnew.example%3A4433",
        );
        link_card_accepted(&ui, &shell);
        let sh = shell.borrow();
        let added = sh.cfg.selected_profile().expect("switched");
        assert_eq!(
            (
                added.name.as_str(),
                added.url.as_str(),
                added.publish_secret.as_str()
            ),
            ("new.example:4433", "https://new.example:4433", "")
        );
    }

    // D5a: the two-link sequence. A first link adds and switches to a
    // hostile server; a second names the user's static room there. The key
    // stored for the room on the default fleet is never presented.
    #[test]
    fn a_rooms_key_never_follows_a_link_to_another_server() {
        let ui = window();
        let mut cfg = Config::default();
        cfg.remember_room(S, "lan-party", "k3y", 1);
        let (shell, _) = shell_with(cfg);
        let evil = "relay=https%3A%2F%2Fevil.example";
        warm(&ui, &shell, &format!("gawk://broadcast?{evil}"));
        link_card_accepted(&ui, &shell);
        assert_eq!(shell.borrow().cfg.server_key(), "https://evil.example");
        warm(
            &ui,
            &shell,
            &format!("gawk://broadcast?room=lan-party&{evil}"),
        );
        let sh = shell.borrow();
        let room = parse_room_input(&sh.cfg.room);
        assert_eq!(
            start_room(&sh.cfg, room.as_ref()).1,
            "",
            "no key to evil.example"
        );
        assert_eq!(
            sh.cfg.room_attach_key(S, "lan-party"),
            Some("k3y"),
            "still S's"
        );
    }

    // D5a: a manual switch presents nothing either; the matching server does.
    #[test]
    fn a_rooms_key_goes_only_to_the_server_it_was_stored_for() {
        let mut cfg = friend_cfg();
        cfg.remember_room(S, "lan-party", "k3y", 1);
        store_room_choice(
            &mut cfg,
            &RoomInput {
                code: "lan-party".into(),
                grant: None,
            },
        );
        let room = parse_room_input(&cfg.room);
        assert_eq!(start_room(&cfg, room.as_ref()).1, "k3y");
        cfg.selected_server = "Friend".into();
        assert_eq!(
            start_room(&cfg, room.as_ref()),
            ("lan-party".into(), "".into(), "".into())
        );
        // Chosen again on Friend: the key on file is the default's, not Friend's.
        let (attach, _) = store_room_choice(
            &mut cfg,
            &RoomInput {
                code: "lan-party".into(),
                grant: None,
            },
        );
        assert_eq!(attach, "");
    }

    // D3: a viewer link goes to the browser and leaves the window where it
    // is; a cold one never shows the window at all.
    #[test]
    fn viewer_links_open_the_browser_without_raising_the_window() {
        let ui = window();
        let (shell, calls) = shell_with(Config::default());
        opened();
        warm(
            &ui,
            &shell,
            "gawk://watch/abc234?relay=https%3A%2F%2Fr.example",
        );
        warm(&ui, &shell, "gawk://room/lan-party?nick=Juho");
        assert_eq!(
            opened(),
            [
                "https://gawk.ioio.fi/#/view/ABC234?relay=https%3A%2F%2Fr.example",
                "https://gawk.ioio.fi/#/room/lan-party?nick=Juho",
            ]
        );
        assert!(calls.borrow().is_empty(), "no raise: {:?}", calls.borrow());
        assert_eq!(shell.borrow().cfg.room, "");

        let cfg = Config {
            app_url: "https://gawk.example.org/".into(),
            ..Default::default()
        };
        let open = |s: &str| Request::Open(Ok(s.into()));
        assert_eq!(
            cold_viewer_url(&open("gawk://watch/ABC234"), &cfg).as_deref(),
            Some("https://gawk.example.org/#/view/ABC234")
        );
        assert_eq!(
            cold_viewer_url(&open("gawk://room/lan-party"), &cfg).as_deref(),
            Some("https://gawk.example.org/#/room/lan-party")
        );
        assert_eq!(
            cold_viewer_url(&open("gawk://broadcast?room=abc"), &cfg),
            None
        );
        assert_eq!(cold_viewer_url(&open("gawk://watch/nope"), &cfg), None);
        assert_eq!(cold_viewer_url(&Request::Open(Err(())), &cfg), None);
        assert_eq!(cold_viewer_url(&Request::Raise, &cfg), None);
    }

    // D3, G7: a rejected link shows the window with the reason and changes
    // nothing.
    #[test]
    fn a_rejected_link_says_why_and_changes_nothing() {
        let ui = window();
        let (shell, calls) = shell_with(Config::default());
        warm(&ui, &shell, "gawk://settings?room=abc");
        assert_eq!(
            ui.get_link_notice(),
            "This link couldn't be opened: this app doesn't know what it asks for."
        );
        handle_incoming(
            &ui,
            &shell,
            Incoming {
                request: Request::Open(Err(())),
                activation: None,
            },
        );
        assert!(ui.get_link_notice().contains("more than one link"));
        assert_eq!(*calls.borrow(), ["raise(tok)", "raise()"]);
        assert_eq!(shell.borrow().cfg, Config::default());
    }

    // D3, D10: on macOS the launch's own link comes through the inbox. A
    // viewer link first thing after launch opens the browser and quits,
    // without a raise; after that, viewer links are warm.
    #[test]
    fn a_viewer_link_first_after_a_macos_launch_is_a_cold_start() {
        let ui = window();
        let (shell, calls) = shell_with(Config::default());
        opened();
        shell.borrow_mut().cold_until =
            Some(std::time::Instant::now() + std::time::Duration::from_secs(60));
        warm(&ui, &shell, "gawk://watch/ABC234");
        assert_eq!(opened(), ["https://gawk.ioio.fi/#/view/ABC234"]);
        assert!(shell.borrow().cold_until.is_none());
        assert!(calls.borrow().is_empty());

        // A broadcast link first is applied as usual, and ends the window.
        shell.borrow_mut().cold_until =
            Some(std::time::Instant::now() + std::time::Duration::from_secs(60));
        warm(&ui, &shell, "gawk://broadcast?room=abc");
        assert_eq!(shell.borrow().cfg.room, "abc");
        assert!(shell.borrow().cold_until.is_none());
    }

    // OD1: a second launch with no link raises the window, and only that.
    #[test]
    fn a_second_launch_raises_the_window() {
        let ui = window();
        let (shell, calls) = shell_with(Config::default());
        handle_incoming(
            &ui,
            &shell,
            Incoming {
                request: Request::Raise,
                activation: None,
            },
        );
        assert_eq!(*calls.borrow(), ["raise()"]);
        assert_eq!(ui.get_link_notice(), "");
    }

    // D6: the launch's own link fills in the form without a raise (the
    // window is about to show).
    #[test]
    fn the_launch_link_applies_without_a_raise() {
        let ui = window();
        let (shell, calls) = shell_with(Config::default());
        open_link(&ui, &shell, Ok("gawk://broadcast?room=abc".into()), None);
        assert_eq!(shell.borrow().cfg.room, "abc");
        assert!(calls.borrow().is_empty());
    }

    #[test]
    fn the_notice_names_what_was_filled_and_left_out() {
        assert_eq!(link_notice(&[], &[]), "");
        let d = |p: &str, why| Dropped {
            param: p.into(),
            why,
        };
        assert_eq!(
            link_notice(
                &["room abc".into()],
                &[d("rt", DropReason::Secret), d("fps", DropReason::Unknown)]
            ),
            "From the link: room abc. Left out: rt (links never carry secrets), fps (not known here)."
        );
        assert_eq!(
            link_notice(&[], &[d("relay", DropReason::Invalid)]),
            "Left out: relay (not valid)."
        );
    }

    /// A MainWindow on Slint's testing backend (no display), per thread.
    fn window() -> MainWindow {
        i_slint_backend_testing::init_no_event_loop();
        MainWindow::new().unwrap()
    }

    fn window_row(title: &str) -> WindowRow {
        WindowRow {
            hwnd: 1,
            pid: 1,
            title: title.into(),
            icon: slint::Image::default(),
            has_icon: false,
        }
    }

    // docs/64 D14 (BUGS.md): the picker tab being LOOKED AT decided what a
    // start captured. Choose a window, open the picker, look at Whole
    // display, go Back: the window must still be the source.
    #[test]
    fn looking_at_the_other_picker_tab_keeps_the_chosen_source() {
        let ui = window();
        ui.set_windows(ModelRc::new(VecModel::from(vec![
            window_row("Discord"),
            window_row("Counter-Strike 2"),
        ])));
        ui.set_monitors(ModelRc::new(VecModel::from(vec![MonitorRow {
            hmonitor: 1,
            label: "Display 1".into(),
        }])));
        ui.set_picker_tab(0);
        ui.set_selected_window(1);
        ui.set_selected_monitor(-1);

        ui.invoke_view_picker_tab(1);

        assert_eq!(
            current_source_key(&ui).as_deref(),
            Some("window:Counter-Strike 2")
        );
    }

    fn picker_fixture() -> MainWindow {
        let ui = window();
        ui.set_windows(ModelRc::new(VecModel::from(vec![
            window_row("Discord"),
            window_row("Counter-Strike 2"),
        ])));
        ui.set_monitors(ModelRc::new(VecModel::from(vec![MonitorRow {
            hmonitor: 1,
            label: "Display 1".into(),
        }])));
        ui
    }

    // The picker opens on Apps and games until the user has looked at a
    // tab, even when the preselected source is a display.
    #[test]
    fn the_picker_opens_on_apps_by_default() {
        let ui = picker_fixture();
        seed_settings(&ui, &Config::default());
        apply_default_source(&ui, &Config::default());
        assert_eq!(ui.get_picker_tab(), 1, "the fallback source is a display");

        ui.invoke_open_picker();

        assert_eq!(ui.get_page(), 1);
        assert_eq!(ui.get_draft_tab(), 0);
    }

    // It then opens on the tab last looked at, saved across launches.
    #[test]
    fn the_picker_reopens_on_the_tab_last_looked_at() {
        let ui = picker_fixture();
        let (shell, _) = shell_with(Config::default());
        wire_callbacks(&ui, &shell);
        ui.invoke_open_picker();
        ui.invoke_view_picker_tab(1);
        ui.set_page(0);

        ui.invoke_open_picker();
        assert_eq!(ui.get_draft_tab(), 1);
        assert_eq!(shell.borrow().cfg.picker_view, "display");

        ui.invoke_view_picker_tab(0);
        assert_eq!(shell.borrow().cfg.picker_view, "");

        // A later launch reads it back (a second window: the backend is
        // per thread).
        let next = MainWindow::new().unwrap();
        let cfg = Config {
            picker_view: "display".into(),
            ..Default::default()
        };
        seed_settings(&next, &cfg);
        next.invoke_open_picker();
        assert_eq!(next.get_draft_tab(), 1);
    }

    // A refresh with the picker open keeps the tab being looked at.
    #[test]
    fn a_picker_refresh_keeps_the_tab_in_view() {
        let ui = picker_fixture();
        seed_settings(&ui, &Config::default());
        apply_default_source(&ui, &Config::default());
        ui.invoke_open_picker();
        assert_eq!(ui.get_draft_tab(), 0);

        apply_default_source(&ui, &Config::default());

        assert_eq!(ui.get_draft_tab(), 0);
    }

    // A row click chooses it at once; no second press of Share this.
    #[test]
    fn clicking_a_picker_row_chooses_it() {
        let ui = picker_fixture();
        let (shell, _) = shell_with(Config::default());
        wire_callbacks(&ui, &shell);
        apply_default_source(&ui, &Config::default());
        ui.invoke_open_picker();

        ui.invoke_pick_window(1);

        assert_eq!(ui.get_page(), 0);
        assert_eq!(
            current_source_key(&ui).as_deref(),
            Some("window:Counter-Strike 2")
        );
        assert_eq!(shell.borrow().cfg.last_source, "window:Counter-Strike 2");

        ui.invoke_open_picker();
        ui.invoke_view_picker_tab(1);
        ui.invoke_pick_monitor(0);

        assert_eq!(ui.get_page(), 0);
        assert_eq!(
            current_source_key(&ui).as_deref(),
            Some("display:Display 1")
        );
    }

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
        let urls = server_urls(&cfg, "");
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

    // Review of #381: creating a room while live left the old room as the
    // pending one, so the next go-live (and a crash resume) went back to it.
    #[test]
    fn creating_a_room_replaces_the_stored_one_idle_or_live() {
        for live in [false, true] {
            let mut cfg = Config {
                room: "old-room".into(),
                room_attach_secret: "k".into(),
                room_creator_token: "ab".repeat(16),
                ..Default::default()
            };
            let pending = store_room_create(&mut cfg, live);
            assert_eq!(pending, !live, "a pending create only while idle");
            assert_eq!(
                (
                    cfg.room.as_str(),
                    cfg.room_attach_secret.as_str(),
                    cfg.room_creator_token.as_str()
                ),
                ("", "", ""),
                "live {live}: the old room must not be rejoined"
            );
        }
    }

    // Review finding on #381: a pasted link's `?rt=` grant is a credential.
    // `room` is stored in the clear, so it must hold only the code; the
    // attach key and the creator token go to their wrapped fields.
    #[test]
    fn a_pasted_links_grant_never_reaches_the_file_in_the_clear() {
        use gawk_engine::config::{load, save};
        struct Wrap;
        impl config::Credentials for Wrap {
            fn wrap(&self, v: &str) -> String {
                if v.is_empty() {
                    String::new()
                } else {
                    format!("wrapped:{}", v.chars().rev().collect::<String>())
                }
            }
            fn unwrap(&self, s: &str) -> String {
                s.strip_prefix("wrapped:")
                    .map_or_else(|| s.to_owned(), |v| v.chars().rev().collect())
            }
        }
        let dir = std::env::temp_dir().join(format!("gawk-shell-grant-{}", std::process::id()));
        let path = dir.join("broadcast.json");
        let token = "5a".repeat(16);
        for (raw, secret) in [
            (
                "https://gawk.ioio.fi/#/room/lan-party?rt=a%3Ak3ysecret".to_string(),
                "k3ysecret".to_string(),
            ),
            (
                format!("https://gawk.ioio.fi/#/room/K7XQ2M?rt=c:{token}"),
                token.clone(),
            ),
        ] {
            let mut cfg = Config::default();
            let input = parse_room_input(&raw).unwrap();
            let (attach, creator) = store_room_choice(&mut cfg, &input);
            assert_eq!(cfg.room, input.code, "room holds the code only");
            assert!(attach == secret || creator == secret);
            save(&path, &cfg, &Wrap).unwrap();
            let on_disk = std::fs::read_to_string(&path).unwrap();
            assert!(!on_disk.contains(&secret), "grant in the clear: {on_disk}");
            // The grant survives the round trip, for the next go-live.
            let (loaded, _) = load(&path, &Wrap);
            assert!(
                loaded.room_attach_secret == secret || loaded.room_creator_token == secret,
                "{loaded:?}"
            );
        }
        std::fs::remove_dir_all(&dir).ok();
    }

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

    // docs/64 D3: the strip names a non-default server and its host, and
    // never renders on the pinned default.
    #[test]
    fn the_strip_names_a_non_default_server_and_nothing_else() {
        let mut cfg = cfg_with_two_customs();
        assert_eq!(server_strip(&cfg), None, "the default shows no strip");
        cfg.selected_server = "Juho's homelab".into();
        let (name, host) = server_strip(&cfg).unwrap();
        assert_eq!(name, "Juho's homelab");
        assert!(!host.is_empty() && !host.contains("://"), "{host}");
        // A nameless profile is named by its host.
        cfg.selected_server = "  ".into();
        assert_eq!(
            server_strip(&cfg),
            Some(("other.example:4433".into(), "other.example:4433".into()))
        );
        assert_eq!(host_of("https://gawk.ioio.fi/#/x"), "gawk.ioio.fi");
    }

    // docs/64 D15: the selected server's row carries the probe's finding;
    // the others carry their address alone.
    #[test]
    fn the_selected_servers_row_carries_the_probe() {
        let mut cfg = cfg_with_two_customs();
        cfg.selected_server = "Juho's homelab".into();
        let urls = server_urls(&cfg, "41 ms");
        assert!(!urls[0].contains("41 ms"));
        assert!(urls[1].ends_with(" · 41 ms"), "{}", urls[1]);
        assert!(!urls[2].contains("41 ms"));
        let mut probe = ProbeState::default();
        assert_eq!(probe_note(&probe), "");
        probe.result = Some(ProbeResult::Failed);
        assert_eq!(probe_note(&probe), "Can't reach");
        probe.result = Some(ProbeResult::Ok {
            rtt_ms: 24,
            name: None,
        });
        assert_eq!(probe_note(&probe), "24 ms");
    }

    // docs/64 D1: the probe runs at once when nothing is known, again
    // after its interval, and never twice at a time.
    #[test]
    fn the_probe_runs_when_due_and_one_at_a_time() {
        let now = std::time::Instant::now();
        let mut p = ProbeState::default();
        assert!(probe_due(&p, now), "nothing known: at once");
        p.in_flight = true;
        assert!(!probe_due(&p, now), "one at a time");
        p.in_flight = false;
        p.next_at = Some(now + PROBE_INTERVAL);
        assert!(!probe_due(&p, now));
        assert!(probe_due(&p, now + PROBE_INTERVAL));
    }

    // docs/64 D15: Test connection dials the EDITED server — the default's
    // pinned address, a custom one's own, nothing for one without an
    // address — and quotes the relay's name beside the facts.
    #[test]
    fn test_connection_dials_the_edited_server_and_says_what_it_found() {
        let mut cfg = cfg_with_two_customs();
        cfg.selected_server = "Juho's homelab".into();
        assert_eq!(
            edit_relay_url(&cfg, DEFAULT_SERVER_NAME),
            config::resolve_relay_url("")
        );
        assert_eq!(
            edit_relay_url(&cfg, "  "),
            config::resolve_relay_url("https://other.example:4433")
        );
        let blank = cfg.add_custom_server();
        assert_eq!(edit_relay_url(&cfg, &blank), "");

        assert_eq!(
            test_result_text(&ProbeResult::Ok {
                rtt_ms: 41,
                name: Some("Homelab relay".into())
            }),
            (2, "Reachable · 41 ms · calls itself “Homelab relay”".into())
        );
        assert_eq!(
            test_result_text(&ProbeResult::Ok {
                rtt_ms: 41,
                name: None
            }),
            (2, "Reachable · 41 ms".into())
        );
        assert_eq!(test_result_text(&ProbeResult::Failed).0, 3);
    }

    // docs/64 D15: Edit writes to the server it shows — which need not be
    // the selected one — and a rename follows the selection only when it
    // renames the selected server.
    #[test]
    fn edit_never_switches_servers() {
        let ui = window();
        let mut cfg = cfg_with_two_customs();
        cfg.selected_server = DEFAULT_SERVER_NAME.into();
        seed_settings(&ui, &cfg);
        seed_edit_fields(&ui, &cfg, "Juho's homelab");
        assert!(ui.get_server_is_custom());
        ui.set_set_server_name("Homelab".into());
        ui.set_set_secret("s3cret".into());
        let edited = read_settings(&ui, &mut cfg, "Juho's homelab");
        assert_eq!(edited, "Homelab");
        assert_eq!(
            cfg.selected_server, DEFAULT_SERVER_NAME,
            "still the default"
        );
        let p = cfg.servers.iter().find(|p| p.name == "Homelab").unwrap();
        assert_eq!(p.publish_secret, "s3cret");

        // The default's page shows the default's own secret.
        cfg.set_default_secret("official");
        cfg.selected_server = "Homelab".into();
        seed_edit_fields(&ui, &cfg, DEFAULT_SERVER_NAME);
        assert!(!ui.get_server_is_custom());
        assert_eq!(ui.get_set_secret(), "official");
    }

    /// A platform that captures nothing and counts the hooks the shell calls.
    #[derive(Default)]
    struct Counting {
        ended: Rc<std::cell::Cell<u32>>,
        /// The preview and start hooks, in call order.
        calls: Rc<RefCell<Vec<String>>>,
    }

    impl Platform for Counting {
        fn hooks(&self) -> Hooks {
            Hooks {
                notify: |_, _, _| {},
                creds: || Box::new(config::Plaintext),
            }
        }
        fn init_window(&mut self, _ui: &MainWindow) {}
        fn prepare_start(&mut self, _ui: &MainWindow, _cfg: &Config) -> Result<Prepared, String> {
            self.calls.borrow_mut().push("prepare".into());
            Err("nothing to share in a test".into())
        }
        fn broadcast_ended(&mut self, _ui: &MainWindow) {
            self.ended.set(self.ended.get() + 1);
        }
        fn raise_window(&mut self, _ui: &MainWindow, activation: Option<String>) {
            self.calls
                .borrow_mut()
                .push(format!("raise({})", activation.unwrap_or_default()));
        }
        /// A source is always chosen: a picture whenever one is wanted.
        fn preview(&mut self, _ui: &MainWindow, wanted: bool) -> PreviewFrame {
            self.calls.borrow_mut().push(format!("preview({wanted})"));
            if wanted {
                PreviewFrame::New((2, 1, vec![0; 8]))
            } else {
                PreviewFrame::Hidden
            }
        }
        fn as_any_mut(&mut self) -> &mut dyn Any {
            self
        }
    }

    /// A shell with no config file, on a platform that counts its hooks.
    fn test_shell() -> (Rc<RefCell<Shell>>, Rc<std::cell::Cell<u32>>) {
        let (shell, ended, _) = counting_shell();
        (shell, ended)
    }

    #[allow(clippy::type_complexity)]
    fn counting_shell() -> (
        Rc<RefCell<Shell>>,
        Rc<std::cell::Cell<u32>>,
        Rc<RefCell<Vec<String>>>,
    ) {
        let platform = Counting::default();
        let ended = platform.ended.clone();
        let calls = platform.calls.clone();
        let _ = HOOKS.set(platform.hooks());
        let mut shell = build_shell(Box::new(platform), Config::default(), None, None, None);
        // No relay probe from a test tick.
        shell.probe.in_flight = true;
        (Rc::new(RefCell::new(shell)), ended, calls)
    }

    // docs/65 D2, D4: idle on the main page, the card shows the platform's
    // preview; on any other page none is wanted and the card hides it.
    #[test]
    fn ready_shows_the_preview_only_on_the_main_page() {
        let ui = window();
        let (shell, _, calls) = counting_shell();
        tick(&ui, &shell);
        assert!(ui.get_has_preview());
        assert_eq!(ui.get_preview().size().width, 2);
        ui.set_page(2);
        tick(&ui, &shell);
        assert!(!ui.get_has_preview());
        assert_eq!(*calls.borrow(), ["preview(true)", "preview(false)"]);
    }

    // docs/65 D5: a start stops the preview before the platform resolves
    // what to capture, so the two never share the source.
    #[test]
    fn a_start_stops_the_preview_before_it_prepares() {
        let ui = window();
        let (shell, _, calls) = counting_shell();
        tick(&ui, &shell);
        calls.borrow_mut().clear();
        start_broadcast(&ui, &shell, false);
        assert_eq!(*calls.borrow(), ["preview(false)", "prepare"]);
        assert!(!ui.get_has_preview());
    }

    // Review of #423: a media build carries its generation, and one that
    // finishes after its broadcast moved on (an End, a new Go live) is
    // dropped — it must not stop the new broadcast or leave its error for
    // the next End.
    #[test]
    fn a_build_for_a_broadcast_that_moved_on_is_dropped() {
        let ui = window();
        let (shell, _) = test_shell();
        shell.borrow_mut().build_gen = 2;
        handle_message(
            &ui,
            &shell,
            ShellMsg::RestartFailed {
                build: 1,
                failure: StartFailure::Capture("the old build's failure".into()),
            },
        );
        assert_eq!(shell.borrow().pending_error, None);
        // The current build's failure is still the broadcast's.
        handle_message(
            &ui,
            &shell,
            ShellMsg::RestartFailed {
                build: 2,
                failure: StartFailure::Capture("this build's failure".into()),
            },
        );
        assert!(shell.borrow().pending_error.is_some());
    }

    // Review of #423: only a 404 says the relay let a paused code go; any
    // other refusal keeps its own error, and offers no new code (minting
    // would fail the same way after a 401 or a 451).
    #[test]
    fn only_a_404_reads_as_the_paused_code_gone() {
        let err = || Some("lost the connection to the relay and could not resume: x".to_string());
        let (text, mint) = ending_error(err(), true, Some(404), "HT4M9R");
        assert!(
            text.unwrap()
                .starts_with("The server no longer holds HT4M9R")
        );
        assert!(mint);
        for status in [401, 403, 409, 451] {
            assert_eq!(
                ending_error(err(), true, Some(status), "HT4M9R"),
                (err(), false)
            );
        }
        // Unreachable for the whole window: no status, the real error.
        assert_eq!(ending_error(err(), true, None, "HT4M9R"), (err(), false));
        // A 404 on a loss while live is the engine's own sentence.
        assert_eq!(
            ending_error(err(), false, Some(404), "HT4M9R"),
            (err(), false)
        );
        assert_eq!(ending_error(None, true, None, "HT4M9R"), (None, false));
    }

    // Review of #423: an ending tells the platform, so a source handed back
    // at a pause (Linux's portal grant) is let go when End comes from
    // Paused, where there is no media to shut down.
    // Review of #423: one rule for "on air" that both system-picker
    // platforms read when a pick lands — a restart or resume building its
    // media is on air with no media; Paused and Idle are not.
    #[test]
    fn on_air_is_starting_or_live_media_or_not() {
        let ui = window();
        assert!(!on_air(&ui), "idle");
        ui.set_busy(true);
        assert!(on_air(&ui), "starting");
        ui.set_live(true);
        assert!(on_air(&ui), "live, or a restart building its media");
        ui.set_live(false);
        ui.set_paused(true);
        assert!(!on_air(&ui), "paused: Resume takes the pick");
        ui.set_busy(false);
        assert!(!on_air(&ui), "the crash's Paused page");
    }

    #[test]
    fn an_ending_tells_the_platform() {
        let ui = window();
        let (shell, ended) = test_shell();
        shell.borrow_mut().state = UiState::Paused;
        end_broadcast(&ui, &shell, None);
        assert_eq!(ended.get(), 1);
    }

    // Review of #423: a test still running when the relay address is edited
    // must not leave the button stuck on "Testing…" — its answer is for the
    // old address and is dropped, so the edit clears the test.
    #[test]
    fn editing_the_address_clears_a_test_of_the_old_one() {
        let ui = window();
        ui.set_test_state(1);
        clear_test_if_moved(&ui, "https://a.example:4433", "https://a.example:4433");
        assert_eq!(ui.get_test_state(), 1, "same address: the test runs on");
        clear_test_if_moved(&ui, "https://a.example:4433", "https://b.example:4433");
        assert_eq!(ui.get_test_state(), 0, "the button works again");
        ui.set_test_state(2);
        ui.set_test_result("Reachable · 24 ms".into());
        clear_test_if_moved(&ui, "https://b.example:4433", "https://c.example:4433");
        assert_eq!(
            (ui.get_test_state(), ui.get_test_result().as_str()),
            (0, "")
        );
    }

    // A launch with a custom server selected: the shell edits the default
    // until an Edit is clicked, so an unrelated settings change (a quality
    // click) must not carry the custom server's secret into the default's.
    #[test]
    fn a_settings_change_at_launch_leaves_the_default_secret_alone() {
        let ui = window();
        let mut cfg = cfg_with_two_customs();
        cfg.selected_server = "Juho's homelab".into();
        cfg.servers
            .iter_mut()
            .find(|p| p.name == "Juho's homelab")
            .unwrap()
            .publish_secret = "homelab-secret".into();
        seed_settings(&ui, &cfg);
        read_settings(&ui, &mut cfg, DEFAULT_SERVER_NAME);
        assert_eq!(cfg.default_secret(), "default-secret", "the default's own");
        assert_eq!(cfg.resolve_publish_secret(), "homelab-secret");
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
        cfg.remember_room(S, "older", "", 10 * day);
        cfg.remember_room(S, "TuhisRoom", "", 11 * day);
        cfg.remember_room(S, "newest", "", 12 * day);
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
        let v = roster_view(&s, "K7XQ2M", "https://gawk.ioio.fi", false, false);
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
        let v = roster_view(&s, "K7XQ2M", "https://gawk.ioio.fi", true, false);
        let removable: Vec<bool> = v.rows.iter().map(|r| r.removable).collect();
        assert_eq!(removable, [false, true, true]);
    }

    // docs/64 D9: a pause keeps the room, and our own away row says what
    // it is — not "their stream is paused".
    #[test]
    fn our_row_says_paused_while_we_are() {
        let mut s = room_with_people();
        let ours = s
            .attachments
            .iter_mut()
            .find(|a| a.broadcast_id == "K7XQ2M")
            .unwrap();
        ours.live = false;
        let v = roster_view(&s, "K7XQ2M", "x", false, true);
        let row = v.rows.iter().find(|r| r.you).unwrap();
        assert_eq!(row.detail, "Paused");
        let v = roster_view(&s, "K7XQ2M", "x", false, false);
        let row = v.rows.iter().find(|r| r.you).unwrap();
        assert_eq!(row.detail, "Away", "a network loss is not a pause");
    }

    // docs/64 D6: Rejoin joins the room just left with the grant it was
    // joined with — the creator token first, else the attach key.
    #[test]
    fn rejoin_uses_the_grant_the_room_was_joined_with() {
        assert!(
            left_room("", "k", None).is_none(),
            "no code, nothing to rejoin"
        );
        let creator = left_room("P9HDZ3", "", Some(&RoomGrant::Creator("ab".repeat(16)))).unwrap();
        assert!(matches!(
            rejoin_input(&creator).grant,
            Some(RoomGrant::Creator(ref h)) if h == &"ab".repeat(16)
        ));
        let keyed = left_room("TuhisRoom", "hunter2", None).unwrap();
        let input = rejoin_input(&keyed);
        assert_eq!(input.code, "TuhisRoom");
        assert!(matches!(input.grant, Some(RoomGrant::Attach(ref k)) if k == "hunter2"));
        let open = left_room("P9HDZ3", "", None).unwrap();
        assert!(rejoin_input(&open).grant.is_none());
    }

    #[test]
    fn watcher_lines_read_naturally() {
        let mut s = room_with_people();
        s.people.retain(|p| p.streaming || p.nickname == "jussi");
        assert_eq!(
            roster_view(&s, "K7XQ2M", "x", false, false).watchers,
            "jussi is watching"
        );
        s.people.retain(|p| p.streaming);
        let v = roster_view(&s, "K7XQ2M", "x", false, false);
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

    // docs/64 D9–D10: the clock and the summary count time live, not time
    // paused.
    #[test]
    fn time_live_leaves_the_pauses_out() {
        use std::time::Duration;
        let s = |n| Duration::from_secs(n);
        assert_eq!(live_secs(s(600), s(0), None), 600);
        assert_eq!(live_secs(s(600), s(120), None), 480);
        assert_eq!(live_secs(s(600), s(120), Some(s(60))), 420);
        assert_eq!(live_secs(s(10), s(120), None), 0, "never negative");
    }

    #[test]
    fn quality_rows_name_the_size_rate_and_cap() {
        let cfg = Config::default();
        assert_eq!(
            quality_rows(&cfg),
            ("1080p · 60 fps".into(), "up to 12 Mbps".into())
        );
        assert_eq!(
            live_quality_rows(1080, 60, 12_000_000),
            (
                "1080p · 60 fps".into(),
                "H.264 hardware · up to 12 Mbps".into()
            )
        );
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
        let rows = summary_rows(7, 84_000_000, 60, 1080, 60);
        let pairs: Vec<(&str, &str)> = rows
            .iter()
            .map(|r| (r.label.as_str(), r.value.as_str()))
            .collect();
        assert_eq!(
            pairs,
            [
                ("live", "1 minute"),
                ("most watching", "7"),
                ("average upload", "11.2 Mbps"),
                ("sent", "1080p60"),
            ]
        );
        assert_eq!(summary_rows(0, 5, 0, 1, 1)[2].value, "n/a");
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

    // docs/47 D3/D6: the setting and the env each turn the check off, and a
    // check GitHub answered today is not repeated.
    #[test]
    fn the_update_check_runs_only_when_on_and_due() {
        let now = 1_790_000_000;
        let cfg = Config::default();
        assert_eq!(update_check_skip(&cfg, false, now, false), None);
        assert!(
            update_check_skip(&cfg, true, now, false).is_some(),
            "env opt-out"
        );
        let off = Config {
            disable_update_check: true,
            ..Default::default()
        };
        assert!(
            update_check_skip(&off, false, now, false).is_some(),
            "setting"
        );
        assert!(
            update_check_skip(&off, false, now, true).is_some(),
            "setting, refetch"
        );
        let recent = Config {
            last_update_check: update::format_rfc3339(now - 60),
            ..Default::default()
        };
        assert!(
            update_check_skip(&recent, false, now, false).is_some(),
            "not due"
        );
        // R47: a remembered update this build could install asks again.
        assert_eq!(
            update_check_skip(&recent, false, now, true),
            None,
            "refetch"
        );
        assert!(
            update_check_skip(&recent, true, now, true).is_some(),
            "env still wins"
        );
    }

    #[test]
    fn an_answer_restarts_the_window_and_caches_what_it_found() {
        let now = 1_790_000_000;
        let found = Update {
            version: "2.1.0".into(),
            release_url: "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v2.1.0"
                .into(),
            files: None,
            deb: None,
        };

        let mut cfg = Config::default();
        let shown = apply_update_outcome(&mut cfg, &Outcome::Answered(Some(found.clone())), now);
        assert_eq!(shown, Some(Some(found.clone())));
        assert_eq!(cfg.last_update_check, update::format_rfc3339(now));
        assert_eq!(cfg.update_version, "2.1.0");
        assert_eq!(cfg.update_url, found.release_url);
        // A relaunch inside the window shows it from the cache.
        assert_eq!(
            update::cached(&cfg.update_version, &cfg.update_url, "2.0.0"),
            Some(found.clone())
        );

        // "Nothing newer" clears the cache, so a stale notice cannot return.
        let got = apply_update_outcome(&mut cfg, &Outcome::Answered(None), now + 1000);
        assert_eq!(got, Some(None));
        assert_eq!(cfg.last_update_check, update::format_rfc3339(now + 1000));
        assert!(cfg.update_version.is_empty() && cfg.update_url.is_empty());

        // No answer changes nothing: the stamp and the cache stay.
        let mut cfg = Config {
            last_update_check: "2026-09-01T00:00:00Z".into(),
            update_version: "2.1.0".into(),
            update_url: found.release_url.clone(),
            ..Default::default()
        };
        let got = apply_update_outcome(&mut cfg, &Outcome::Unreachable("offline".into()), now);
        assert_eq!(got, None);
        assert_eq!(cfg.last_update_check, "2026-09-01T00:00:00Z");
        assert_eq!(cfg.update_version, "2.1.0");
    }

    fn release(v: &str) -> Update {
        Update {
            version: v.into(),
            release_url: format!(
                "https://github.com/Tuhis/gawk/releases/tag/gawk-broadcast-desktop/v{v}"
            ),
            files: None,
            deb: None,
        }
    }

    // PR #415 review: a notice dismissed while the launch fetch is in
    // flight must not come back when that fetch finds the same release.
    #[test]
    fn a_dismissed_notice_stays_hidden_when_the_launch_check_finds_it_again() {
        let mut st = UpdateState {
            shown: Some(release("2.1.0")), // from the cache
            checking: true,                // the launch fetch
            ..Default::default()
        };
        st.dismiss();
        assert_eq!(st.shown, None);
        st.finish(Some(Some(release("2.1.0"))), false);
        assert_eq!(st.shown, None, "dismissed until restart");
        // A newer release than the dismissed one still shows.
        st.finish(Some(Some(release("2.2.0"))), false);
        assert_eq!(st.shown, Some(release("2.2.0")));
    }

    // Asking explicitly overrides a dismissal: the answer is what was asked.
    #[test]
    fn check_now_shows_a_dismissed_release_again() {
        let mut st = UpdateState {
            shown: Some(release("2.1.0")),
            ..Default::default()
        };
        st.dismiss();
        assert!(st.request_manual());
        assert!(st.finish(Some(Some(release("2.1.0"))), true));
        assert_eq!(st.shown, Some(release("2.1.0")));
    }

    // PR #415 review: Check now during the launch fetch must not be a
    // silent no-op — the in-flight answer is reported as the button's.
    #[test]
    fn check_now_during_the_launch_check_reports_that_checks_answer() {
        let mut st = UpdateState {
            checking: true, // the launch fetch
            ..Default::default()
        };
        assert!(!st.request_manual(), "no second request");
        assert!(st.checking);
        let report = st.finish(Some(None), false);
        assert!(report, "the button asked, so the row reports");
        assert!(!st.checking);
        // The next automatic answer is not the button's.
        st.checking = true;
        assert!(!st.finish(Some(None), false));
    }

    #[test]
    fn no_answer_keeps_the_notice() {
        let mut st = UpdateState {
            shown: Some(release("2.1.0")),
            checking: true,
            ..Default::default()
        };
        st.finish(None, false);
        assert_eq!(st.shown, Some(release("2.1.0")));
    }

    #[test]
    fn the_settings_button_says_what_the_check_found() {
        let found = Update {
            version: "2.1.0".into(),
            release_url: String::new(),
            files: None,
            deb: None,
        };
        assert_eq!(
            update_status_text(&Outcome::Answered(Some(found))),
            "Version 2.1.0 is available."
        );
        assert_eq!(
            update_status_text(&Outcome::Answered(None)),
            format!("You have the latest version (v{}).", version::RELEASE)
        );
        assert!(update_status_text(&Outcome::Unreachable("x".into())).contains("Couldn't reach"));
    }

    // ---- R47 (docs/48 D4, D9) -------------------------------------------

    fn signed(v: &str) -> Update {
        let remote = |name: &str| update::Remote {
            name: name.into(),
            url: format!(
                "https://github.com/Tuhis/gawk/releases/download/gawk-broadcast-desktop/v{v}/{name}"
            ),
            size: 1,
        };
        Update {
            files: Some(Box::new(update::ReleaseFiles {
                asset: remote("gawk-broadcast-linux-x86_64.tar.gz"),
                sums: remote("SHA256SUMS"),
                sig: remote("SHA256SUMS.minisig"),
            })),
            deb: Some(format!("gawk-broadcast_{v}_amd64.deb")),
            ..release(v)
        }
    }

    fn installable(shown: Option<Update>) -> UpdateState {
        UpdateState {
            shown,
            target: Some(InstallTarget {
                exe: PathBuf::from("/opt/gawk/gawk-broadcast-linux"),
                layout: Layout::LinuxTarball,
                args: Vec::new(),
            }),
            ..Default::default()
        }
    }

    #[test]
    fn a_signed_update_downloads_only_while_idle_and_once() {
        let mut st = installable(Some(signed("2.1.0")));
        assert_eq!(st.to_stage(false), None, "never while starting or live");
        assert_eq!(st.to_stage(true), Some(signed("2.1.0")));
        st.staging = true;
        assert_eq!(st.to_stage(true), None, "one download at a time");
        st.staging = false;
        st.ready = Some(Ready {
            version: "2.1.0".into(),
            binary: PathBuf::from("/opt/gawk/.gawk-update/gawk-broadcast-linux"),
        });
        assert!(st.ready_for_shown());
        assert_eq!(st.to_stage(true), None, "already ready");
        // A newer release replaces the ready one: download it, hide the button.
        st.shown = Some(signed("2.2.0"));
        assert!(!st.ready_for_shown());
        assert_eq!(st.to_stage(true), Some(signed("2.2.0")));
    }

    #[test]
    fn nothing_downloads_without_files_a_target_or_after_giving_up() {
        assert_eq!(
            installable(Some(release("2.1.0"))).to_stage(true),
            None,
            "a remembered update has no file list"
        );
        let macos = UpdateState {
            shown: Some(signed("2.1.0")),
            ..Default::default()
        };
        assert_eq!(macos.to_stage(true), None, "no install target");
        let mut st = installable(Some(signed("2.1.0")));
        st.given_up = Some("2.1.0".into());
        assert_eq!(st.to_stage(true), None);
        st.shown = Some(signed("2.2.0"));
        assert!(
            st.to_stage(true).is_some(),
            "a newer release is tried afresh"
        );
        let mut dismissed = installable(Some(signed("2.1.0")));
        dismissed.dismiss();
        assert_eq!(
            dismissed.to_stage(true),
            None,
            "a dismissed notice downloads nothing"
        );
    }

    /// The notice's offer: the button only for the staged release, a
    /// "downloading" line while the install is on its way, and the release
    /// page otherwise.
    #[test]
    fn the_notice_offers_what_this_copy_can_do() {
        assert_eq!(UpdateState::default().phase(), 0, "nothing shown");
        // A remembered update at launch: the re-fetch is on its way to the
        // file list, unless it gets no answer.
        let mut st = installable(Some(release("2.1.0")));
        st.checking = true;
        assert_eq!(st.phase(), 1);
        st.finish(None, false);
        assert_eq!(st.phase(), 0, "offline: the release page");
        // Answered with the signed files: downloading, then ready.
        st.finish(Some(Some(signed("2.1.0"))), false);
        st.staging = true;
        assert_eq!(st.phase(), 1);
        st.staging = false;
        st.ready = Some(Ready {
            version: "2.1.0".into(),
            binary: PathBuf::from("/opt/gawk/.gawk-update/gawk-broadcast-linux"),
        });
        assert_eq!(st.phase(), 2);
        // A newer release than the ready one: no button until it is staged.
        st.shown = Some(signed("2.2.0"));
        assert_eq!(st.phase(), 0);
        st.staging = true;
        assert_eq!(st.phase(), 1);

        // No in-place install here (macOS), or given up: the release page,
        // even while a check runs.
        let macos = UpdateState {
            shown: Some(release("2.1.0")),
            checking: true,
            ..Default::default()
        };
        assert_eq!(macos.phase(), 0);
        let mut gave_up = installable(Some(release("2.1.0")));
        gave_up.given_up = Some("2.1.0".into());
        gave_up.checking = true;
        assert_eq!(gave_up.phase(), 0);
    }

    /// Check now retries what this run gave up on, so a download cut off
    /// once is not stuck behind the release page until a restart. An
    /// automatic check does not.
    #[test]
    fn check_now_retries_a_failed_download() {
        let mut st = installable(Some(signed("2.1.0")));
        st.given_up = Some("2.1.0".into());
        st.note = DOWNLOAD_FAILED_NOTE.into();
        st.finish(Some(Some(signed("2.1.0"))), false);
        assert_eq!(st.to_stage(true), None, "the automatic check keeps it");
        assert!(st.request_manual());
        st.finish(Some(Some(signed("2.1.0"))), true);
        assert_eq!(st.to_stage(true), Some(signed("2.1.0")));
        assert_eq!(st.note, "", "the old failure no longer applies");
    }

    /// The swap worked but the new build did not start. It is on disk, so
    /// nothing downloads again this run, Check now included, and the note
    /// keeps saying to start the app again. A second Windows swap would
    /// fail on the running `<exe>.old` and call the update not installed.
    #[test]
    fn an_installed_update_is_not_downloaded_again() {
        let mut st = installable(Some(signed("2.1.0")));
        st.relaunch_failed("2.1.0");
        let note = st.note.clone();
        assert!(st.request_manual());
        st.finish(Some(Some(signed("2.1.0"))), true);
        assert_eq!(st.to_stage(true), None, "Check now leaves it installed");
        assert_eq!(st.note, note, "and still says to start the app again");
        st.finish(Some(Some(signed("2.2.0"))), false);
        assert_eq!(st.to_stage(true), None, "nor a newer one, until a restart");
        st.shown = Some(release("2.2.0"));
        st.checking = true;
        assert_eq!(st.phase(), 0, "no download is on its way");
    }

    #[test]
    fn a_deb_install_is_told_the_package_and_the_command() {
        assert_eq!(
            deb_note(&signed("2.1.0")),
            "Installed from the .deb: download gawk-broadcast_2.1.0_amd64.deb, then run \
             sudo apt install ./gawk-broadcast_2.1.0_amd64.deb"
        );
        // No .deb on the release (a soft attach): the plain R45 line.
        assert_eq!(deb_note(&release("2.1.0")), "");
    }

    /// Live, with a code and a link, shown so the copy targets lay out and
    /// take pointer events. The counters count copy-code and copy-link calls.
    fn live_with_code() -> (MainWindow, Rc<Cell<u32>>, Rc<Cell<u32>>) {
        let ui = window();
        ui.window().set_size(slint::LogicalSize::new(480.0, 900.0));
        ui.set_live(true);
        ui.set_busy(true);
        ui.set_code("ABC123".into());
        ui.set_code_chars(code_chars("ABC123"));
        ui.set_join_link("https://gawk.ioio.fi/ABC123".into());
        let codes = Rc::new(Cell::new(0));
        let links = Rc::new(Cell::new(0));
        let c = codes.clone();
        ui.on_copy_code(move || c.set(c.get() + 1));
        let l = links.clone();
        ui.on_copy_link(move || l.set(l.get() + 1));
        ui.show().unwrap();
        (ui, codes, links)
    }

    const CODE: &str = "Copy code ABC123";
    const LINK: &str = "Copy link https://gawk.ioio.fi/ABC123";

    fn target(ui: &MainWindow, label: &str) -> ElementHandle {
        ElementHandle::find_by_accessible_label(ui, label)
            .next()
            .unwrap_or_else(|| panic!("no element labelled {label:?}"))
    }

    fn shows(ui: &MainWindow, label: &str) -> bool {
        ElementHandle::find_by_accessible_label(ui, label)
            .next()
            .is_some()
    }

    fn hover(ui: &MainWindow, label: &str) {
        let e = target(ui, label);
        let (p, s) = (e.absolute_position(), e.size());
        ui.window().dispatch_event(WindowEvent::PointerMoved {
            position: slint::LogicalPosition::new(p.x + s.width / 2.0, p.y + s.height / 2.0),
        });
    }

    // The code and the link copied only on a second click: the FocusScope
    // over the TouchArea took the first press to grab focus.
    #[test]
    fn one_click_copies_the_code_and_the_link() {
        let (ui, codes, links) = live_with_code();
        target(&ui, CODE).mock_single_click(PointerEventButton::Left);
        assert_eq!(codes.get(), 1, "the first click on the code copies it");
        target(&ui, LINK).mock_single_click(PointerEventButton::Left);
        assert_eq!(links.get(), 1, "the first click on the link copies it");
    }

    // "Copied" never went away: the flash Timer was `running: false`, and
    // restart() only restarts a timer that is already running.
    #[test]
    fn the_copied_tip_clears_after_a_moment() {
        let (ui, _, _) = live_with_code();
        for label in [CODE, LINK] {
            target(&ui, label).mock_single_click(PointerEventButton::Left);
            assert!(shows(&ui, "Copied"), "{label}: a click shows Copied");
            ui.window().dispatch_event(WindowEvent::PointerExited);
            i_slint_backend_testing::mock_elapsed_time(Duration::from_millis(1300));
            assert!(!shows(&ui, "Copied"), "{label}: Copied clears after 1.2 s");
            assert!(
                !shows(&ui, "Click to copy"),
                "{label}: no tip once the pointer has left"
            );
        }
    }

    // The link said "Click to copy" on hover; the code said nothing.
    #[test]
    fn hovering_the_code_or_the_link_offers_click_to_copy() {
        let (ui, _, _) = live_with_code();
        for label in [CODE, LINK] {
            ui.window().dispatch_event(WindowEvent::PointerExited);
            assert!(!shows(&ui, "Click to copy"), "{label}: no tip before hover");
            hover(&ui, label);
            assert!(shows(&ui, "Click to copy"), "{label}: hover shows the tip");
        }
    }
}
