//! gawk-broadcast for macOS (R52, docs/54): ScreenCaptureKit capture through
//! the system picker, VideoToolbox low-latency H.264, the shared engine.
//!
//! This is the MB1 shell: the shared window with the Share card in its empty
//! state, the version badge, and the menu bar ⌘Q quits through. Capture,
//! encode and audio arrive in MB2–MB4 and are not faked here — the picker
//! button is disabled and Start says what is missing, rather than a broadcast
//! that silently sends nothing.

#[cfg(target_os = "macos")]
mod shell;

#[cfg(target_os = "macos")]
fn main() {
    shell::run();
}

// The Linux and msvc jobs build the whole workspace (docs/38 D18); this is
// what they compile for this crate. Every dependency is macOS-gated in
// Cargo.toml, so the stub costs them nothing.
#[cfg(not(target_os = "macos"))]
fn main() {
    eprintln!("gawk-broadcast-macos runs on macOS only (docs/54); Windows has gawk-broadcast.exe");
    std::process::exit(1);
}
