//! The window both GUI shells show (docs/54 D11), compiled once here and
//! exported, plus the build's version and the window logic both shells run.
//! The Windows shell (`app-windows`) and the macOS one (`app-macos`) differ in
//! properties and one card, never in a forked `.slint` file — and because the
//! generated `MainWindow` is one type, code that seeds or reads it lives here
//! once instead of in each shell.
//!
//! Platform-free by construction: nothing in this crate may name a Windows
//! or Apple API.

use gawk_engine::config::{self, Config};

slint::include_modules!();

pub mod version;

/// The Settings card's two "where does this go" captions and the terms link,
/// from the config as it stands (docs/38 D13: blank resolves to the default
/// at use).
pub fn refresh_captions(ui: &MainWindow, cfg: &Config) {
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
