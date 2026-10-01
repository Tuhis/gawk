//! The macOS platform for the shared shell (docs/54 D11): the system
//! content picker behind the Share card, the ScreenCaptureKit + VideoToolbox
//! pipeline behind `Media`, plaintext credentials in a mode-0600 file (D12).
//! Settings, rooms, the session lifecycle and stats are the shell's.

use crate::pipeline::{self, Pipeline};
use gawk_capture::fit::encode_size;
use gawk_capture::sck::StreamSettings;
use gawk_capture::sck_picker::{Picked, Picker, PickerEvent};
use gawk_engine::config::{self, Config};
use gawk_engine::lossnotice::NetworkFacts;
use gawk_ui::MainWindow;
use gawk_ui::preview::{PreviewFrame, PreviewSlot, PreviewSource};
use gawk_ui::shell::{Hooks, Media, Platform, Prepared, Shell};
use std::any::Any;
use std::cell::RefCell;
use std::rc::Rc;
use std::sync::mpsc;

pub struct Mac {
    picker: Picker,
    picked: Option<Picked>,
    events: mpsc::Receiver<PickerEvent>,
    /// The picker is on screen (see `Picker`: active only while presenting
    /// or live).
    presenting: bool,
    active: bool,
    /// A pick landed while on air with no media — a restart or a resume
    /// building its pipeline, which already took the old pick: the shell
    /// switches the broadcast to it (review of #423).
    restart_requested: bool,
    /// Ready's preview of the pick (docs/65 D3), keyed by `pick_gen`.
    preview: PreviewSlot<u64>,
    /// Bumped on every pick: a new pick is a new preview.
    pick_gen: u64,
    /// The shell wanted a preview on its last ask: the picker stays active
    /// with a pick held, so the indicator does not flicker between ticks.
    preview_wanted: bool,
    /// A pipeline existed at the last tick.
    has_media: bool,
}

impl Mac {
    /// On the main thread: the picker is UI.
    pub fn new() -> Self {
        let (tx, events) = mpsc::channel();
        let picker = Picker::new(move |ev| {
            let _ = tx.send(ev);
        });
        Self {
            picker,
            picked: None,
            events,
            presenting: false,
            active: false,
            restart_requested: false,
            preview: PreviewSlot::default(),
            pick_gen: 0,
            preview_wanted: false,
            has_media: false,
        }
    }

    /// "Choose what to share…" / "Change…": re-picking while live targets
    /// the running stream (D4).
    pub fn choose(&mut self, media: Option<&dyn Media>) {
        self.presenting = true;
        match live_capture(media) {
            Some(capture) => self.picker.present_for(capture),
            None => self.picker.present(),
        }
        self.active = true;
    }

    fn previewing(&self) -> bool {
        self.preview_wanted && self.picked.is_some()
    }

    fn set_active(&mut self, active: bool) {
        if self.active != active {
            self.picker.set_active(active);
            self.active = active;
        }
    }
}

fn live_capture(media: Option<&dyn Media>) -> Option<&gawk_capture::sck::Capture> {
    media?
        .as_any()
        .downcast_ref::<Pipeline>()
        .and_then(|p| p.capture())
}

/// The picked content fitted into the rung box, never upscaled (docs/54 D9).
fn stream_settings(cfg: &Config, picked: &Picked) -> StreamSettings {
    let (box_w, box_h, fps, _) = cfg.resolve_rung();
    let (width, height) = encode_size(picked.width, picked.height, box_w, box_h);
    StreamSettings { width, height, fps }
}

impl Platform for Mac {
    fn hooks(&self) -> Hooks {
        Hooks {
            notify: crate::notify::notify,
            creds,
        }
    }

    fn init_window(&mut self, ui: &MainWindow) {
        ui.set_system_picker(true);
        ui.set_native_menu(true);
        // Consolas is Windows-only; Menlo ships with every macOS.
        ui.set_mono_font("Menlo".into());
    }

    /// Window fit (R64, docs/66 D15): the window's frame and its screen's
    /// visible frame, without the menu bar and the Dock.
    fn placement(&self, ui: &MainWindow) -> Option<gawk_ui::fit::Placement> {
        crate::place::placement(ui)
    }

    fn prepare_start(&mut self, _ui: &MainWindow, cfg: &Config) -> Result<Prepared, String> {
        let Some(picked) = self.picked.clone() else {
            return Err("Choose what to share first.".into());
        };
        // This start takes the latest pick: any switch asked for is done.
        self.restart_requested = false;
        let (_, _, _, bps) = cfg.resolve_rung();
        let params = pipeline::Params {
            stream: stream_settings(cfg, &picked),
            picked,
            peak_bitrate_bps: bps,
            last_good_encoder: (!cfg.last_good_encoder.is_empty())
                .then(|| cfg.last_good_encoder.clone()),
            audio: !cfg.disable_audio,
        };
        let capture_mode = params.picked.style.capture_mode();
        Ok(Prepared {
            capture_mode,
            source: params.picked.summary.clone(),
            source_is_window: capture_mode == "app",
            build: Box::new(move |env| {
                Pipeline::build(params, env).map(|p| Box::new(p) as Box<dyn Media>)
            }),
        })
    }

    fn tick(&mut self, ui: &MainWindow, media: Option<&dyn Media>) {
        while let Ok(ev) = self.events.try_recv() {
            self.presenting = false;
            match ev {
                PickerEvent::Picked(picked) => {
                    log::info!("picked {picked:?}");
                    ui.set_share_summary(picked.summary.clone().into());
                    ui.set_share_mode_label(picked.style.audio_scope().label().into());
                    ui.set_error_text("".into());
                    if let Some(p) = media.and_then(|m| m.as_any().downcast_ref::<Pipeline>()) {
                        // Live: the running capture switches in place
                        // (D4), so Live's Sharing row names the new pick.
                        p.repick(&picked);
                        ui.set_sharing_title(picked.summary.clone().into());
                        ui.set_sharing_window(picked.style.capture_mode() == "app");
                        ui.set_sharing_detail(
                            if picked.style.capture_mode() == "app" {
                                "Window"
                            } else {
                                "Display"
                            }
                            .into(),
                        );
                    } else if gawk_ui::shell::on_air(ui) {
                        // On air with no media: a restart or a resume is
                        // building on the old pick. Switch to this one once
                        // it is up (review of #423).
                        self.restart_requested = true;
                    }
                    self.picked = Some(picked);
                    self.pick_gen += 1;
                }
                PickerEvent::Cancelled => {}
                PickerEvent::Failed(text) => ui.set_error_text(text.into()),
            }
        }
        // "Use whole-system audio" (D6): a re-pick in display mode.
        if let Some(p) = media.and_then(|m| m.as_any().downcast_ref::<Pipeline>())
            && p.take_system_audio_request()
            && let Some(capture) = p.capture()
        {
            self.presenting = true;
            self.picker.present_display_for(capture);
        }
        // Active only while on screen, live or previewing a pick, so an app
        // with nothing picked never shows the system's screen-sharing
        // indicator (docs/65 OD1); live, the menu-bar control can re-pick
        // too (D4).
        self.has_media = media.is_some();
        self.set_active(self.presenting || self.has_media || self.previewing());
    }

    fn preview(&mut self, _ui: &MainWindow, wanted: bool) -> PreviewFrame {
        self.preview_wanted = wanted;
        let want = (wanted && self.picked.is_some()).then_some(self.pick_gen);
        if want.is_some() {
            self.set_active(true);
        }
        let picked = self.picked.clone();
        let frame = self.preview.update(want, |_| {
            let picked = picked.as_ref().ok_or("nothing picked")?;
            crate::preview::Preview::start(picked).map(|p| Box::new(p) as Box<dyn PreviewSource>)
        });
        self.set_active(self.presenting || self.has_media || self.previewing());
        frame
    }

    fn network_facts(&mut self, relay: std::net::SocketAddr) -> Option<NetworkFacts> {
        crate::network::probe(relay)
    }

    fn take_restart_request(&mut self) -> bool {
        std::mem::take(&mut self.restart_requested)
    }

    /// macOS keeps its pick across broadcasts (the Share card still shows
    /// it); only a switch still asked for is dropped with the broadcast.
    fn broadcast_ended(&mut self, _ui: &MainWindow) {
        self.restart_requested = false;
    }

    fn as_any_mut(&mut self) -> &mut dyn Any {
        self
    }
}

/// The "choose content" callback, wired once the window exists.
pub fn wire(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let shell = shell.clone();
    ui.on_choose_content(move || {
        let mut sh = shell.borrow_mut();
        let (mac, media) = sh.platform_and_media::<Mac>();
        mac.choose(media);
    });
}

/// docs/54 D12: macOS is Unix, so the Linux rule applies — plaintext in a
/// mode-0600 file, no Keychain (a Keychain ACL is bound to the signing
/// identity, so every dev build would prompt).
fn creds() -> Box<dyn config::Credentials> {
    Box::new(config::Plaintext)
}
