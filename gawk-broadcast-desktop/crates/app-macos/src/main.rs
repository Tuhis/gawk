//! gawk-broadcast for macOS (R52, docs/54): ScreenCaptureKit capture through
//! the system picker, VideoToolbox low-latency H.264, the shared engine and
//! the shared shell (`gawk_ui::shell`) — this crate is only the platform.
//!
//! Capture (MB2), encode (MB3), audio (MB4) and the macOS shell touches —
//! the Share card, the menu bar's Settings…, notifications (MB5) — are in.

#[cfg(target_os = "macos")]
mod link;
#[cfg(target_os = "macos")]
mod network;
#[cfg(target_os = "macos")]
mod notify;
#[cfg(target_os = "macos")]
mod pipeline;
#[cfg(target_os = "macos")]
mod place;
#[cfg(target_os = "macos")]
mod platform;
#[cfg(target_os = "macos")]
mod preview;

#[cfg(target_os = "macos")]
fn main() {
    // The commit build.rs stamped, for the version badge (crates/ui/build_rev.rs).
    gawk_ui::version::set_build_rev(option_env!("GAWK_BUILD_REV"));
    notify::init();
    // R66 (docs/68 D8, D10): links arrive as Apple Events, cold or warm, and
    // LaunchServices sends a second launch of the bundle to this process —
    // so there is no endpoint of our own, only the inbox the handler fills.
    let (tx, rx) = std::sync::mpsc::channel();
    let inbox = link::init(tx).then_some(rx);
    let args: Vec<String> = std::env::args().collect();
    let launch = gawk_ui::instance::Launch {
        request: gawk_ui::instance::Request::from_args(&args),
        inbox,
    };
    gawk_ui::shell::run(Box::new(platform::Mac::new()), platform::wire, launch);
}

// The Linux and msvc jobs build the whole workspace (docs/38 D18); this is
// what they compile for this crate. Every dependency is macOS-gated in
// Cargo.toml, so the stub costs them nothing.
#[cfg(not(target_os = "macos"))]
fn main() {
    eprintln!("gawk-broadcast-macos runs on macOS only (docs/54); Windows has gawk-broadcast.exe");
    std::process::exit(1);
}
