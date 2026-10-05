//! The iOS app's Rust core (R65, docs/67 D3): the one UniFFI surface the
//! SwiftUI app calls. Nothing tokio- or
//! objc2-shaped crosses it; Swift sees records, enums and callback
//! interfaces only.
//!
//! [`initialize_core`] must run first, before anything reads the engine's
//! identity.

uniffi::setup_scaffolding!();

#[cfg(any(target_os = "macos", target_os = "ios"))]
pub mod broadcast;
pub mod identity;
pub mod viewer;

/// What the core reports about itself to Swift: the build and the defaults
/// it resolves to (docs/67 D5, D23).
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct CoreInfo {
    /// The `gawk-ios` component version (`CFBundleShortVersionString`, D25).
    pub version: String,
    /// The telemetry `kind` and distribution name, `gawk-ios`.
    pub distribution: String,
    /// The Origin header every dial sends, `gawk://ios`.
    pub origin: String,
    /// The compiled-in default relay (CLAUDE.md: the official deployment is
    /// the default target).
    pub default_relay_url: String,
    /// The reference UI that join links point at.
    pub default_app_url: String,
}

/// Injects the iOS identity into the shared engine (docs/67 D5). Idempotent;
/// call it once at process start.
#[uniffi::export]
pub fn initialize_core() {
    gawk_engine::defaults::set_this(&identity::IOS);
}

/// The core's build and defaults, for the Settings screen and the tests.
#[uniffi::export]
pub fn core_info() -> CoreInfo {
    let this = gawk_engine::defaults::this();
    CoreInfo {
        version: env!("CARGO_PKG_VERSION").to_owned(),
        distribution: this.name.to_owned(),
        origin: this.origin.to_owned(),
        default_relay_url: gawk_engine::defaults::RELAY_URL.to_owned(),
        default_app_url: gawk_engine::defaults::APP_URL.to_owned(),
    }
}
