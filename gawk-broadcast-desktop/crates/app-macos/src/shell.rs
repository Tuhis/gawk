//! The macOS window: the shared `MainWindow` in system-picker mode (docs/54
//! D11). Nothing here touches a framework yet — that is MB2's
//! `capture::sck_picker` behind `choose-content`.

use gawk_engine::config::Config;
use gawk_ui::{MainWindow, refresh_captions, version};
use slint::ComponentHandle;

/// Shown on Start until capture and encode exist (MB2–MB3).
const NOT_YET: &str = "This build is the macOS shell only: capture and encode arrive in \
                       later R52 builds (docs/54 MB2–MB3). Nothing was sent.";

pub fn run() {
    let ui = MainWindow::new().expect("create window");
    ui.set_app_version(format!("v{}", version::display()).into());
    ui.set_system_picker(true);
    ui.set_share_picker_available(false);
    // Consolas is Windows-only; Menlo ships with every macOS.
    ui.set_mono_font("Menlo".into());

    // The empty state reads the defaults, as a first run would (docs/54 G8).
    // Loading and saving the D12 config file is MB5.
    refresh_captions(&ui, &Config::default());

    {
        let ui_weak = ui.as_weak();
        ui.on_start_broadcast(move || {
            if let Some(ui) = ui_weak.upgrade() {
                ui.set_error_text(NOT_YET.into());
            }
        });
    }

    ui.run().expect("run event loop");
}
