//! The macOS platform for the shared shell (docs/54 D11): the system
//! content picker behind the Share card, the ScreenCaptureKit + VideoToolbox
//! pipeline behind `Media`, plaintext credentials in a mode-0600 file (D12).
//! Settings, rooms, the session lifecycle and stats are the shell's.

use crate::pipeline::{self, Pipeline};
use gawk_capture::fit::fit_within;
use gawk_capture::sck::StreamSettings;
use gawk_capture::sck_picker::{Picked, Picker, PickerEvent};
use gawk_engine::config::{self, Config};
use gawk_ui::MainWindow;
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

/// The rung box fitted to the picked content (docs/54 D9).
fn stream_settings(cfg: &Config, picked: &Picked) -> StreamSettings {
    let (box_w, box_h, fps, _) = cfg.resolve_rung();
    let (width, height) = fit_within(picked.width, picked.height, box_w, box_h);
    StreamSettings { width, height, fps }
}

impl Platform for Mac {
    fn hooks(&self) -> Hooks {
        Hooks { notify, creds }
    }

    fn init_window(&mut self, ui: &MainWindow) {
        ui.set_system_picker(true);
        // Consolas is Windows-only; Menlo ships with every macOS.
        ui.set_mono_font("Menlo".into());
    }

    fn prepare_start(&mut self, _ui: &MainWindow, cfg: &Config) -> Result<Prepared, String> {
        let Some(picked) = self.picked.clone() else {
            return Err("Choose what to share first.".into());
        };
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
                        p.repick(&picked);
                    }
                    self.picked = Some(picked);
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
        // Active only while on screen or live, so an idle app never shows
        // the system's screen-sharing indicator; live, the menu-bar control
        // can re-pick too (D4).
        self.set_active(self.presenting || media.is_some());
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

/// Notifications are MB5's (`UNUserNotificationCenter`, which needs the
/// bundle identifier); until then they go to the debug log.
fn notify(summary: &str, body: &str, critical: bool) {
    if critical {
        log::warn!("notification: {summary}: {body}");
    } else {
        log::info!("notification: {summary}: {body}");
    }
}
