//! gawk-broadcast for macOS (R52, docs/54): ScreenCaptureKit capture through
//! the system picker, VideoToolbox low-latency H.264, the shared engine and
//! the shared shell (`gawk_ui::shell`) — this crate is only the platform.
//!
//! As of MB3 a Start is a real, video-only broadcast; audio is MB4's.

#[cfg(target_os = "macos")]
mod pipeline;
#[cfg(target_os = "macos")]
mod platform;

#[cfg(target_os = "macos")]
fn main() {
    gawk_ui::shell::run(Box::new(platform::Mac::new()), platform::wire);
}

// The Linux and msvc jobs build the whole workspace (docs/38 D18); this is
// what they compile for this crate. Every dependency is macOS-gated in
// Cargo.toml, so the stub costs them nothing.
#[cfg(not(target_os = "macos"))]
fn main() {
    eprintln!("gawk-broadcast-macos runs on macOS only (docs/54); Windows has gawk-broadcast.exe");
    std::process::exit(1);
}
