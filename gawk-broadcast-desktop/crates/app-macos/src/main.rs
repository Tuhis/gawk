//! gawk-broadcast for macOS (R52, docs/54): ScreenCaptureKit capture through
//! the system picker, VideoToolbox low-latency H.264, the shared engine.
//!
//! As of MB2 the shell picks content through the system picker and runs
//! the ScreenCaptureKit stream; encode and send arrive in MB3, audio in
//! MB4. Until then Start is a capture test and the header says so, rather
//! than a broadcast that silently sends nothing.

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
