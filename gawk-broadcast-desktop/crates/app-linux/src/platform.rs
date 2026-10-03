//! The Linux platform for the shared shell (docs/58 D9): the portal Share
//! card, the whose-audio card after a window pick, and plaintext
//! credentials in the Go app's mode-0600 file (D10, the Linux rule since
//! docs/19 D19). Settings, rooms, the session lifecycle and stats are the
//! shell's.

use crate::audio::AudioPlan;
use crate::pipeline::{self, Pipeline};
use gawk_audio::pwctl::{Notice, PwCtl};
use gawk_audio::pwgraph::{App, Event};
use gawk_capture::fit::fit_within;
use gawk_capture::portal::{self, Grant, Picked, SourceKind};
use gawk_encode::{gst, gst_policy};
use gawk_engine::config::{self, Config};
use gawk_ui::fit::{Placement, Rect};
use gawk_ui::preview::{PreviewFrame, PreviewSlot, PreviewSource};
use gawk_ui::shell::{Hooks, Media, Platform, Prepared, Shell, Thumb};
use gawk_ui::{AudioAppRow, MainWindow};
use slint::winit_030::WinitWindowAccessor;
use slint::{ComponentHandle, ModelRc, VecModel};
use std::any::Any;
use std::cell::RefCell;
use std::os::fd::AsRawFd;
use std::rc::Rc;
use std::sync::mpsc;

/// The desktop entry's app ID (R44, docs/53): the Wayland `app_id` must match
/// it for the compositor to find the icon (V-11).
pub const APP_ID: &str = "fi.ioio.gawk.broadcast";

/// Whose sound goes out with a shared window (docs/39 D5).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Choice {
    App(String),
    System,
    NoAudio,
}

pub struct Linux {
    picked: Option<Grant>,
    picking: bool,
    picks_tx: mpsc::Sender<Picked>,
    picks: mpsc::Receiver<Picked>,
    /// The control plane, running while a window is picked and idle so the
    /// card's list is live; it moves into the pipeline at Start.
    ctl: Option<PwCtl>,
    apps: Vec<App>,
    choice: Choice,
    /// The user touched the choice for this pick: preselection stops.
    chosen_by_user: bool,
    /// The config's `audioApp` (the last-used binary, preselected) and
    /// `audioDevice` (the pin that skips the step), read at launch and
    /// kept current by [`Platform::remember`].
    last_app: String,
    device_pin: String,
    /// A pick landed while live: the shell switches the broadcast to it on
    /// the same code (docs/64 D12).
    restart_requested: bool,
    /// Ready's preview of the held grant (docs/65 D3), keyed by `pick_gen`.
    /// It reads the grant's PipeWire remote, so it stops before the grant
    /// is taken or let go (D5).
    preview: PreviewSlot<u64>,
    /// Bumped whenever `picked` becomes a different grant.
    pick_gen: u64,
}

/// The GStreamer preview, as the shell's preview source.
struct GstPreview(gst::Preview);

impl PreviewSource for GstPreview {
    fn take(&self) -> Option<Thumb> {
        self.0.take()
    }
}

/// What the Share card and Live's Sharing row call a grant.
fn grant_summary(grant: &Grant) -> String {
    match grant.size {
        Some((w, h)) if w > 0 => format!("{} · {w}×{h}", grant.kind.label()),
        _ => grant.kind.label().to_string(),
    }
}

impl Linux {
    pub fn new() -> Self {
        let (cfg, _) = config::default_path()
            .map(|p| config::load(&p, &config::Plaintext))
            .unwrap_or_default();
        Self::with_config(&cfg)
    }

    /// Seeded from `cfg`'s `audioApp` and `audioDevice`.
    fn with_config(cfg: &Config) -> Self {
        let (picks_tx, picks) = mpsc::channel();
        Self {
            picked: None,
            picking: false,
            picks_tx,
            picks,
            ctl: None,
            apps: Vec::new(),
            choice: Choice::System,
            chosen_by_user: false,
            last_app: cfg.audio_app.clone(),
            device_pin: cfg.audio_device.clone(),
            restart_requested: false,
            preview: PreviewSlot::default(),
            pick_gen: 0,
        }
    }

    /// "Choose what to share…" / "Change…" — the desktop's own picker
    /// (docs/58 D3). Re-picking drops the held grant first: every pick is
    /// its own portal session.
    pub fn choose(&mut self) {
        if self.picking {
            return;
        }
        self.picking = true;
        let tx = self.picks_tx.clone();
        portal::pick(move |p| {
            let _ = tx.send(p);
        });
    }

    fn on_picked(&mut self, ui: &MainWindow, grant: Grant) {
        log::info!("picked {grant:?}");
        self.preview.stop();
        if let Some(old) = self.picked.take() {
            old.release();
        }
        self.pick_gen += 1;
        ui.set_share_summary(grant_summary(&grant).into());
        ui.set_share_window(grant.kind == SourceKind::Window);
        ui.set_error_text("".into());
        let window = grant.kind == SourceKind::Window;
        self.picked = Some(grant);
        self.chosen_by_user = false;
        self.apps.clear();
        if window && self.device_pin.trim().is_empty() {
            self.choice = if self.last_app.is_empty() {
                Choice::System
            } else {
                Choice::App(self.last_app.clone())
            };
            if self.ctl.is_none() {
                match PwCtl::start() {
                    Ok(c) => self.ctl = Some(c),
                    // D6's first row: a sentence and system audio, never a
                    // blocked start.
                    Err(e) => {
                        log::warn!("per-application audio is unavailable: {e}");
                        self.choice = Choice::System;
                    }
                }
            }
        } else {
            self.stop_ctl();
            self.choice = Choice::System;
        }
        self.render_audio(ui);
    }

    fn stop_ctl(&mut self) {
        if let Some(c) = self.ctl.take() {
            c.stop();
        }
        self.apps.clear();
    }

    /// The Sound row and the whose-audio sheet, from the current state.
    fn render_audio(&self, ui: &MainWindow) {
        let window = self
            .picked
            .as_ref()
            .is_some_and(|g| g.kind == SourceKind::Window);
        let pin = !self.device_pin.trim().is_empty();
        ui.set_whose_audio(window && !pin && self.ctl.is_some());
        let rows: Vec<AudioAppRow> = self
            .apps
            .iter()
            .map(|a| AudioAppRow {
                name: a.name.clone().into(),
                binary: a.binary.clone().into(),
                streams: a.streams as i32,
            })
            .collect();
        ui.set_audio_apps(ModelRc::new(VecModel::from(rows)));
        ui.set_audio_choice(choice_index(&self.choice, &self.apps));
        let pin = pin.then_some(self.device_pin.trim());
        ui.set_share_mode_label(
            sound_line(&self.choice, &self.apps, window, pin, self.ctl.is_some()).into(),
        );
    }

    /// The card's click, read back from the UI.
    pub fn choice_changed(&mut self, ui: &MainWindow) {
        self.choice = choice_from_index(ui.get_audio_choice(), &self.apps);
        self.chosen_by_user = true;
        self.render_audio(ui);
    }

    fn apply_apps(&mut self, ui: &MainWindow, apps: Vec<App>) {
        self.apps = apps;
        if let Some(c) = preselect(self.chosen_by_user, &self.last_app, &self.apps) {
            self.choice = c;
        }
        self.render_audio(ui);
    }
}

/// The sheet's selection for a choice: an index into `apps`, -1 for the
/// whole system (also an app not playing right now), -2 for no audio.
fn choice_index(choice: &Choice, apps: &[App]) -> i32 {
    match choice {
        Choice::App(b) => apps
            .iter()
            .position(|a| &a.binary == b)
            .map_or(-1, |i| i as i32),
        Choice::System => -1,
        Choice::NoAudio => -2,
    }
}

/// The inverse: what a click on the sheet means.
fn choice_from_index(i: i32, apps: &[App]) -> Choice {
    match i {
        -2 => Choice::NoAudio,
        i if i >= 0 => apps
            .get(i as usize)
            .map_or(Choice::System, |a| Choice::App(a.binary.clone())),
        _ => Choice::System,
    }
}

/// The last-used binary is preselected when it is playing (docs/39 D5),
/// until the user picks something themselves.
fn preselect(chosen_by_user: bool, last_app: &str, apps: &[App]) -> Option<Choice> {
    (!chosen_by_user && !last_app.is_empty() && apps.iter().any(|a| a.binary == last_app))
        .then(|| Choice::App(last_app.to_owned()))
}

/// The Sound row's line: what viewers will hear.
fn sound_line(
    choice: &Choice,
    apps: &[App],
    window: bool,
    device_pin: Option<&str>,
    per_app_available: bool,
) -> String {
    const SYSTEM: &str = "Everything you hear on this computer";
    if let Some(d) = device_pin {
        return format!("The audio device set in the config ({d})");
    }
    if !window {
        return SYSTEM.into();
    }
    if !per_app_available {
        return format!("{SYSTEM} (per-app audio unavailable)");
    }
    match choice {
        Choice::App(b) => match apps.iter().find(|a| &a.binary == b) {
            Some(a) => format!("{}'s sound", a.name),
            None => format!("{b}'s sound, once it plays"),
        },
        Choice::System => SYSTEM.into(),
        Choice::NoAudio => "No sound".into(),
    }
}

impl Platform for Linux {
    fn hooks(&self) -> Hooks {
        Hooks {
            notify: crate::notify::notify,
            creds,
        }
    }

    fn launch_log(&self) {
        log::info!(
            "session: {} on {}",
            std::env::var("XDG_SESSION_TYPE").unwrap_or_else(|_| "unknown".into()),
            std::env::var("XDG_CURRENT_DESKTOP").unwrap_or_else(|_| "unknown desktop".into())
        );
    }

    fn init_window(&mut self, ui: &MainWindow) {
        // Before the window shows: winit reads it when it creates the
        // surface.
        if let Err(e) = slint::set_xdg_app_id(APP_ID) {
            log::warn!("could not set the Wayland app id: {e}");
        }
        ui.set_system_picker(true);
        ui.set_native_menu(false);
        ui.set_share_empty_hint(
            "Your desktop shows its own picker. Choose one window or a whole screen.".into(),
        );
        // Consolas is Windows-only; DejaVu ships with every desktop distro,
        // and fontconfig falls back to the generic monospace without it.
        ui.set_mono_font("DejaVu Sans Mono".into());
    }

    fn prepare_start(&mut self, ui: &MainWindow, cfg: &Config) -> Result<Prepared, String> {
        // The shell already asked for none (docs/65 D5); this grant is the
        // broadcast's now either way.
        self.preview.stop();
        let Some(grant) = self.picked.take() else {
            return Err("Choose what to share first.".into());
        };
        // This start takes the latest pick, so any switch still asked for
        // is done by it.
        self.restart_requested = false;
        let (box_w, box_h, fps, bps) = cfg.resolve_rung();
        // The rung is a bounding box; the portal's size (compositor
        // coordinates) is the aspect to fit (docs/39 D2).
        let (width, height) = match grant.size {
            Some((w, h)) => fit_within(w, h, box_w, box_h),
            None => fit_within(0, 0, box_w, box_h),
        };
        let window = grant.kind == SourceKind::Window;
        let capture_mode = grant.kind.capture_mode();
        let source = grant_summary(&grant);
        let audio = if cfg.disable_audio {
            self.stop_ctl();
            AudioPlan::Off
        } else if !cfg.audio_device.trim().is_empty() || !window {
            self.stop_ctl();
            AudioPlan::System {
                device: cfg.audio_device.clone(),
                last_good: cfg.last_good_audio_source.clone(),
            }
        } else {
            match (self.choice.clone(), self.ctl.take()) {
                (Choice::App(binary), Some(ctl)) => AudioPlan::App {
                    ctl,
                    binary,
                    last_good: cfg.last_good_audio_source.clone(),
                },
                (Choice::NoAudio, ctl) => {
                    if let Some(c) = ctl {
                        c.stop();
                    }
                    AudioPlan::NoneByChoice
                }
                (_, ctl) => {
                    if let Some(c) = ctl {
                        c.stop();
                    }
                    AudioPlan::System {
                        device: String::new(),
                        last_good: cfg.last_good_audio_source.clone(),
                    }
                }
            }
        };
        let params = pipeline::Params {
            grant,
            width,
            height,
            fps,
            peak_bps: bps,
            encoder_pin: cfg.encoder.clone(),
            last_good_encoder: cfg.last_good_encoder.clone(),
            audio,
        };
        // The grant now belongs to this broadcast: the card asks again next
        // time (docs/19 D5 as reversed).
        ui.set_share_summary("".into());
        ui.set_whose_audio(false);
        ui.set_share_mode_label("".into());
        Ok(Prepared {
            capture_mode,
            source,
            source_is_window: window,
            build: Box::new(move |env| {
                Pipeline::build(params, env).map(|p| Box::new(p) as Box<dyn Media>)
            }),
        })
    }

    /// A pause or a quick restart gave the grant back (docs/64 D8, D9): it
    /// is the source again — unless a pick made while live already replaced
    /// it, and then it is let go. The whose-audio control plane went with
    /// the media, so a window grant starts a fresh one for the next start.
    fn source_returned(&mut self, ui: &MainWindow, source: Box<dyn Any + Send>) {
        let Ok(grant) = source.downcast::<Grant>() else {
            return;
        };
        if self.picked.is_some() {
            grant.release();
            return;
        }
        let window = grant.kind == SourceKind::Window;
        ui.set_share_summary(grant_summary(&grant).into());
        ui.set_share_window(window);
        self.picked = Some(*grant);
        self.pick_gen += 1;
        if window && self.device_pin.trim().is_empty() && self.ctl.is_none() {
            match PwCtl::start() {
                Ok(c) => self.ctl = Some(c),
                Err(e) => log::warn!("per-application audio is unavailable: {e}"),
            }
        }
        self.render_audio(ui);
    }

    fn take_restart_request(&mut self) -> bool {
        std::mem::take(&mut self.restart_requested)
    }

    /// The broadcast is over: every broadcast asks the picker again
    /// (docs/58), so a grant a pause handed back — or a pick made while on
    /// air — is released, the sharing indicator goes out, and the card is
    /// empty again (review of #423: End from Paused has no media whose
    /// shutdown would do it).
    fn broadcast_ended(&mut self, ui: &MainWindow) {
        self.preview.stop();
        if let Some(g) = self.picked.take() {
            g.release();
        }
        self.stop_ctl();
        self.restart_requested = false;
        self.choice = Choice::System;
        ui.set_share_summary("".into());
        ui.set_share_window(false);
        self.render_audio(ui);
        ui.set_share_mode_label("".into());
    }

    fn remember(&mut self, cfg: &mut Config) -> bool {
        if let Choice::App(b) = &self.choice
            && cfg.audio_app != *b
        {
            cfg.audio_app = b.clone();
            self.last_app = b.clone();
            return true;
        }
        false
    }

    fn tick(&mut self, ui: &MainWindow, media: Option<&dyn Media>) {
        while let Ok(p) = self.picks.try_recv() {
            self.picking = false;
            match p {
                Picked::Granted(g) => {
                    self.on_picked(ui, g);
                    // On air: Change on the Sharing row switches the
                    // broadcast to the new pick on the same code (docs/64
                    // D12). On air is the window's state, not the media's:
                    // a restart or a resume is on air with no media while
                    // its pipeline builds (review of #423). Paused, Resume
                    // uses the pick and nothing restarts.
                    if media.is_some() || gawk_ui::shell::on_air(ui) {
                        self.restart_requested = true;
                    }
                }
                Picked::Cancelled => {}
                Picked::Failed(text) => ui.set_error_text(text.into()),
            }
        }
        let mut apps = None;
        let mut died = None;
        if let Some(c) = &self.ctl {
            for n in c.drain() {
                match n {
                    Notice::Event(Event::Apps(a)) => apps = Some(a),
                    Notice::Fatal(e) => died = Some(e),
                    _ => {}
                }
            }
        }
        if let Some(e) = died {
            log::warn!("the PipeWire control plane died while idle: {e}");
            self.stop_ctl();
            self.choice = Choice::System;
            self.render_audio(ui);
        } else if let Some(a) = apps {
            self.apply_apps(ui, a);
        }
        // docs/64 D12 (revising docs/58 §6): Change works while live too —
        // the pick restarts the broadcast on the same code.
        ui.set_share_picker_available(true);
    }

    fn preview(&mut self, _ui: &MainWindow, wanted: bool) -> PreviewFrame {
        let want = (wanted && self.picked.is_some()).then_some(self.pick_gen);
        let grant = self.picked.as_ref().map(|g| (g.fd.as_raw_fd(), g.node_id));
        self.preview.update(want, |_| {
            let (fd, node) = grant.ok_or("nothing picked")?;
            gst::Preview::start(&gst_policy::preview_plan(fd, node))
                .map(|p| Box::new(GstPreview(p)) as Box<dyn PreviewSource>)
        })
    }

    /// Window fit (R64, docs/66 D14). Wayland tells a client nothing about
    /// where its window is, and winit 0.30 has no work area, so this says
    /// only how tall the monitor is, less room for its panels: the window is
    /// clamped to that at launch and never grows.
    fn placement(&self, ui: &MainWindow) -> Option<Placement> {
        let window = ui.window();
        let client = window.size().to_logical(window.scale_factor());
        let monitor_h = window
            .with_winit_window(|w| {
                w.current_monitor()
                    .map(|m| m.size().height as f32 / m.scale_factor() as f32)
            })
            .flatten()?;
        Some(Placement {
            frame: Rect::new(0.0, 0.0, client.width, client.height + TITLE_BAR),
            client_h: client.height,
            work: Rect::new(0.0, 0.0, client.width, monitor_h - PANELS),
            positioned: false,
            arranged: false,
        })
    }

    /// A second launch or a link (R66, docs/68 D6, D8). On X11 winit
    /// focuses the window (`_NET_ACTIVE_WINDOW`). On Wayland it can't: winit
    /// 0.30's `focus_window` is a no-op there, and an xdg-activation token
    /// is applied only when a window is created, never to one that exists
    /// (V-2). So on Wayland the docs/68 §8 fallback says where the launch
    /// went, with a notification.
    fn raise_window(&mut self, ui: &MainWindow, activation: Option<String>) {
        let _ = ui.show();
        let wayland = ui
            .window()
            .with_winit_window(|w| {
                w.set_minimized(false);
                w.focus_window();
                is_wayland(w)
            })
            .unwrap_or(false);
        if wayland {
            log::info!(
                "raise on Wayland: winit cannot apply an activation token to an open window ({})",
                if activation.is_some() {
                    "one was passed"
                } else {
                    "none was passed"
                }
            );
            crate::notify::notify(
                "gawk broadcast is already open",
                "Your link or launch went to the open window.",
                false,
            );
        }
    }

    fn as_any_mut(&mut self) -> &mut dyn Any {
        self
    }
}

/// True when winit runs this window on Wayland rather than X11.
fn is_wayland(w: &slint::winit_030::winit::window::Window) -> bool {
    use slint::winit_030::winit::raw_window_handle::{HasDisplayHandle, RawDisplayHandle};
    w.display_handle()
        .is_ok_and(|h| matches!(h.as_raw(), RawDisplayHandle::Wayland(_)))
}

/// What window fit allows for the desktop's panels and the window's title
/// bar, neither of which Wayland reports (docs/66 D14).
const PANELS: f32 = 64.0;
const TITLE_BAR: f32 = 40.0;

/// The Share card and whose-audio callbacks, wired once the window exists.
pub fn wire(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    {
        let shell = shell.clone();
        ui.on_choose_content(move || {
            shell.borrow_mut().platform_mut::<Linux>().choose();
        });
    }
    {
        let shell = shell.clone();
        let weak = ui.as_weak();
        ui.on_audio_choice_changed(move || {
            if let Some(ui) = weak.upgrade() {
                shell
                    .borrow_mut()
                    .platform_mut::<Linux>()
                    .choice_changed(&ui);
            }
        });
    }
}

/// docs/19 D19, docs/58 D10: plaintext in a mode-0600 file on Linux.
fn creds() -> Box<dyn config::Credentials> {
    Box::new(config::Plaintext)
}

#[cfg(test)]
mod tests {
    use super::*;
    use slint::Model;
    use std::os::fd::OwnedFd;
    use std::time::Duration;

    /// A MainWindow on Slint's testing backend (no display), per thread.
    fn window() -> MainWindow {
        i_slint_backend_testing::init_no_event_loop();
        MainWindow::new().unwrap()
    }

    fn grant(kind: SourceKind, size: Option<(u32, u32)>) -> Grant {
        let fd: OwnedFd = std::fs::File::open("/dev/null").unwrap().into();
        Grant::detached(fd, 42, kind, size)
    }

    fn cfg() -> Config {
        Config::default()
    }

    #[test]
    fn the_window_is_the_linux_share_card_with_no_menu_bar() {
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        p.init_window(&ui);
        assert!(ui.get_system_picker());
        assert!(!ui.get_native_menu(), "no in-window menu bar on Linux (D9)");
        assert!(
            ui.get_share_empty_hint()
                .contains("Your desktop shows its own picker")
        );
        assert_eq!(ui.get_mono_font(), "DejaVu Sans Mono");
        let err = p.prepare_start(&ui, &cfg()).err().unwrap();
        assert_eq!(err, "Choose what to share first.");
    }

    #[test]
    fn a_screen_pick_is_whole_system_audio_and_asks_again_next_time() {
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        p.init_window(&ui);
        p.picks_tx
            .send(Picked::Granted(grant(
                SourceKind::Monitor,
                Some((1920, 1080)),
            )))
            .unwrap();
        p.tick(&ui, None);
        assert_eq!(ui.get_share_summary(), "Whole screen · 1920×1080");
        assert!(!ui.get_share_window());
        assert!(!ui.get_whose_audio(), "no whose-audio step for a screen");
        assert_eq!(
            ui.get_share_mode_label(),
            "Everything you hear on this computer"
        );
        assert!(ui.get_share_picker_available());

        let prepared = p.prepare_start(&ui, &cfg()).ok().unwrap();
        assert_eq!(prepared.capture_mode, "screen");
        assert!(!p.remember(&mut cfg()));
        // The grant is this broadcast's now: the card asks again.
        assert_eq!(ui.get_share_summary(), "");
        assert!(p.prepare_start(&ui, &cfg()).is_err());
    }

    #[test]
    fn picker_outcomes_are_silent_or_a_sentence() {
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        p.picking = true;
        p.picks_tx.send(Picked::Cancelled).unwrap();
        p.tick(&ui, None);
        assert!(!p.picking);
        assert_eq!(ui.get_error_text(), "", "a dismissed picker is silent");
        p.picks_tx
            .send(Picked::Failed("No screen-share portal found.".into()))
            .unwrap();
        p.tick(&ui, None);
        assert_eq!(ui.get_error_text(), "No screen-share portal found.");
    }

    /// The whose-audio step end to end against a real (headless) PipeWire:
    /// a window pick starts the control plane, the card lists the app that
    /// is playing, the last-used one is preselected, a click changes it,
    /// and Start hands the control plane to the broadcast and remembers
    /// the choice (docs/39 D5, docs/58 D9).
    #[test]
    fn a_window_pick_asks_whose_audio_and_remembers_the_answer() {
        let Some((d, _g)) = crate::pwtest::daemon() else {
            return;
        };
        let _game = d.emitter("card-game", 440, 2);
        let _other = d.emitter("card-music", 660, 2);
        let ui = window();
        let seeded = Config {
            audio_app: "card-music".into(),
            ..Config::default()
        };
        let mut p = Linux::with_config(&seeded);
        p.init_window(&ui);
        p.picks_tx
            .send(Picked::Granted(grant(
                SourceKind::Window,
                Some((1280, 720)),
            )))
            .unwrap();
        p.tick(&ui, None);
        assert_eq!(ui.get_share_summary(), "One window · 1280×720");
        assert!(ui.get_share_window());
        assert!(ui.get_whose_audio());
        crate::pwtest::wait_for("both apps on the card", || {
            if let Some(c) = &p.ctl {
                c.watch();
            }
            std::thread::sleep(Duration::from_millis(50));
            p.tick(&ui, None);
            let rows = ui.get_audio_apps();
            (0..rows.row_count())
                .filter_map(|i| rows.row_data(i))
                .filter(|r| r.binary == "card-game" || r.binary == "card-music")
                .count()
                == 2
        });
        // The last-used app is preselected, and the Sound row names it.
        assert_eq!(p.choice, Choice::App("card-music".into()));
        let rows = ui.get_audio_apps();
        let idx = ui.get_audio_choice();
        assert_eq!(rows.row_data(idx as usize).unwrap().binary, "card-music");

        // A click on the other app.
        let game = (0..rows.row_count())
            .find(|&i| rows.row_data(i).unwrap().binary == "card-game")
            .unwrap();
        ui.set_audio_choice(game as i32);
        p.choice_changed(&ui);
        assert_eq!(p.choice, Choice::App("card-game".into()));
        assert!(ui.get_share_mode_label().ends_with("'s sound"));

        let prepared = p.prepare_start(&ui, &cfg()).ok().unwrap();
        assert_eq!(prepared.capture_mode, "app");
        assert!(p.ctl.is_none(), "the control plane went to the broadcast");
        let mut c = cfg();
        assert!(p.remember(&mut c));
        assert_eq!(c.audio_app, "card-game");
        drop(prepared); // the unbuilt pipeline's control plane stops with it
        crate::pwtest::wait_for("no gawk objects", || d.gawk_objects().is_empty());
    }

    #[test]
    fn no_audio_by_choice_stops_the_control_plane() {
        let Some((_d, _g)) = crate::pwtest::daemon() else {
            return;
        };
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        p.picks_tx
            .send(Picked::Granted(grant(SourceKind::Window, None)))
            .unwrap();
        p.tick(&ui, None);
        assert!(p.ctl.is_some());
        assert_eq!(ui.get_share_summary(), "One window");
        ui.set_audio_choice(-2);
        p.choice_changed(&ui);
        assert_eq!(ui.get_share_mode_label(), "No sound");
        let prepared = p.prepare_start(&ui, &cfg()).ok().unwrap();
        assert_eq!(prepared.capture_mode, "app");
        assert!(
            p.ctl.is_none(),
            "stopped, not held for a silent broadcast (docs/39 F10)"
        );
        assert!(!p.remember(&mut cfg()));
    }

    #[test]
    fn the_device_pin_skips_the_whose_audio_step() {
        let ui = window();
        let pinned = Config {
            audio_device: "alsa.monitor".into(),
            ..Config::default()
        };
        let mut p = Linux::with_config(&pinned);
        p.picks_tx
            .send(Picked::Granted(grant(SourceKind::Window, None)))
            .unwrap();
        p.tick(&ui, None);
        assert!(p.ctl.is_none(), "no control plane under a device pin");
        assert!(!ui.get_whose_audio());
        assert!(ui.get_share_mode_label().contains("alsa.monitor"));
    }

    /// docs/64 D12 (revising docs/58 §6): a pick while live is the new
    /// source, and asks the shell once to switch to it on the same code. The
    /// running grant, handed back by the restart, is then let go.
    #[test]
    fn a_pick_while_live_switches_the_broadcast_to_it() {
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        struct Live;
        impl Media for Live {
            fn info(&self) -> &gawk_ui::shell::MediaInfo {
                unimplemented!()
            }
            fn force_idr(&self) {}
            fn take_thumbnail(&self) -> Option<gawk_ui::shell::Thumb> {
                None
            }
            fn capture_fps(&self) -> Option<f64> {
                None
            }
            fn audio_state(&self) -> String {
                "off".into()
            }
            fn audio_level(&self) -> f32 {
                0.0
            }
            fn audio_silence_hint(&self) -> bool {
                false
            }
            fn switch_audio_to_system(&self) {}
            fn minimized(&self) -> bool {
                false
            }
            fn take_failure(&self) -> Option<String> {
                None
            }
            fn shutdown(self: Box<Self>) {}
            fn as_any(&self) -> &dyn Any {
                self
            }
        }
        p.picks_tx
            .send(Picked::Granted(grant(
                SourceKind::Monitor,
                Some((1920, 1080)),
            )))
            .unwrap();
        p.tick(&ui, Some(&Live));
        assert!(ui.get_share_picker_available(), "Change works while live");
        assert_eq!(p.picked.as_ref().map(|g| g.node_id), Some(42));
        assert!(p.take_restart_request(), "the shell is asked to switch");
        assert!(!p.take_restart_request(), "once");

        // The restart hands the old grant back: the new pick stays.
        let old = grant(SourceKind::Window, Some((800, 600)));
        p.source_returned(&ui, Box::new(old));
        assert_eq!(p.picked.as_ref().map(|g| g.kind), Some(SourceKind::Monitor));
        let prepared = p.prepare_start(&ui, &cfg()).ok().unwrap();
        assert_eq!(prepared.source, "Whole screen · 1920×1080");
        assert!(!prepared.source_is_window);
    }

    /// Review of #423: a restart or a resume leaves the broadcast on air
    /// with no media while the new pipeline builds. A pick landing then
    /// must still switch the broadcast (not wait in `picked` to hijack a
    /// later, unrelated restart); one landing while paused waits for
    /// Resume; and a start takes the latest pick, so it settles any request.
    #[test]
    fn a_pick_while_on_air_without_media_still_switches() {
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        p.init_window(&ui);
        let pick = |p: &mut Linux, w: u32| {
            p.picks_tx
                .send(Picked::Granted(grant(SourceKind::Monitor, Some((w, 1080)))))
                .unwrap();
            p.tick(&ui, None);
        };

        // On air, the new media still building.
        ui.set_busy(true);
        ui.set_live(true);
        pick(&mut p, 1920);
        assert!(p.take_restart_request(), "the pick switches the broadcast");

        // Paused: Resume uses the pick; nothing restarts.
        ui.set_live(false);
        ui.set_paused(true);
        pick(&mut p, 2560);
        assert!(!p.take_restart_request());

        // A start takes whatever is picked now: no request survives it.
        ui.set_paused(false);
        ui.set_live(true);
        pick(&mut p, 3840);
        p.prepare_start(&ui, &cfg()).ok().unwrap();
        assert!(!p.take_restart_request());
    }

    /// Review of #423: End from Paused finds no media to shut down, so the
    /// grant a pause handed back is let go by the ending itself — the
    /// compositor's sharing indicator goes out, and the card asks again
    /// (docs/58: every broadcast asks).
    #[test]
    fn an_ending_lets_a_returned_grant_go() {
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        p.init_window(&ui);
        p.source_returned(
            &ui,
            Box::new(grant(SourceKind::Monitor, Some((1920, 1080)))),
        );
        assert_eq!(ui.get_share_summary(), "Whole screen · 1920×1080");
        p.broadcast_ended(&ui);
        assert!(p.picked.is_none(), "the grant is released");
        assert!(p.ctl.is_none());
        assert_eq!(ui.get_share_summary(), "");
        assert!(!ui.get_whose_audio());
        assert!(
            p.prepare_start(&ui, &cfg()).is_err(),
            "the next broadcast asks"
        );
    }

    /// docs/64 D9: a pause hands the grant back, and it is the source
    /// again — Resume asks the desktop's picker nothing.
    #[test]
    fn a_returned_grant_is_the_source_again() {
        let ui = window();
        let mut p = Linux::with_config(&cfg());
        p.init_window(&ui);
        p.picks_tx
            .send(Picked::Granted(grant(
                SourceKind::Monitor,
                Some((2560, 1440)),
            )))
            .unwrap();
        p.tick(&ui, None);
        let prepared = p.prepare_start(&ui, &cfg()).ok().unwrap();
        assert_eq!(prepared.source, "Whole screen · 2560×1440");
        assert_eq!(ui.get_share_summary(), "", "the grant is the broadcast's");
        assert!(!p.take_restart_request(), "an idle pick restarts nothing");

        p.source_returned(
            &ui,
            Box::new(grant(SourceKind::Monitor, Some((2560, 1440)))),
        );
        assert_eq!(ui.get_share_summary(), "Whole screen · 2560×1440");
        assert!(
            p.prepare_start(&ui, &cfg()).is_ok(),
            "no second pick needed"
        );
    }

    fn apps() -> Vec<App> {
        vec![
            App {
                binary: "hl2_linux".into(),
                name: "Half-Life 2".into(),
                streams: 2,
            },
            App {
                binary: "firefox".into(),
                name: "Firefox".into(),
                streams: 1,
            },
        ]
    }

    #[test]
    fn the_sheet_index_round_trips_the_choice() {
        let a = apps();
        for c in [
            Choice::App("firefox".into()),
            Choice::System,
            Choice::NoAudio,
        ] {
            assert_eq!(choice_from_index(choice_index(&c, &a), &a), c);
        }
        // An app that is not playing right now shows as the whole system
        // row being selected, and an out-of-range click is the whole system.
        assert_eq!(choice_index(&Choice::App("gone".into()), &a), -1);
        assert_eq!(choice_from_index(7, &a), Choice::System);
    }

    #[test]
    fn the_last_used_app_is_preselected_only_while_playing_and_unchosen() {
        let a = apps();
        assert_eq!(
            preselect(false, "hl2_linux", &a),
            Some(Choice::App("hl2_linux".into()))
        );
        assert_eq!(preselect(true, "hl2_linux", &a), None, "the user chose");
        assert_eq!(preselect(false, "quake", &a), None, "not playing");
        assert_eq!(preselect(false, "", &a), None);
    }

    #[test]
    fn the_sound_line_says_what_viewers_hear() {
        let a = apps();
        let app = Choice::App("hl2_linux".into());
        assert_eq!(
            sound_line(&app, &a, true, None, true),
            "Half-Life 2's sound"
        );
        assert_eq!(
            sound_line(&Choice::App("quake".into()), &a, true, None, true),
            "quake's sound, once it plays"
        );
        assert_eq!(
            sound_line(&Choice::NoAudio, &a, true, None, true),
            "No sound"
        );
        assert_eq!(
            sound_line(&app, &a, false, None, true),
            "Everything you hear on this computer",
            "a screen shares the whole system"
        );
        assert!(sound_line(&app, &a, true, None, false).contains("unavailable"));
        assert!(
            sound_line(&app, &a, true, Some("alsa.monitor"), true).contains("alsa.monitor"),
            "the device pin wins, and says so (docs/39 D3)"
        );
    }
}
