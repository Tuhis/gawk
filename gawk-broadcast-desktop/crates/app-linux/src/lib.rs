//! gawk-broadcast for Linux (R56, docs/58): the xdg-desktop-portal picker,
//! an in-process GStreamer pipeline running R14's hardware-only cascade, the
//! docs/39 app-audio tee on an in-process PipeWire connection, and the shared
//! engine and shell (`gawk_ui::shell`) — this crate is only the platform.
//!
//! A library behind a one-line `main.rs`, so its tests are compiled once:
//! in a binary crate the test harness and the never-run binary are both
//! instrumented, and coverage counts every line twice.

#[cfg(target_os = "linux")]
mod audio;
#[cfg(target_os = "linux")]
mod instance;
#[cfg(target_os = "linux")]
mod notify;
#[cfg(target_os = "linux")]
mod pipeline;
#[cfg(target_os = "linux")]
mod platform;
#[cfg(target_os = "linux")]
mod video;

// The audio crate's headless-PipeWire harness, shared by path (a test-only
// file, so no crate for it). Once per test binary: it owns one daemon and
// the process environment that points at it. Declared at the crate root
// because a `#[path]` inside an inline module resolves from a directory
// that does not exist.
#[cfg(all(test, target_os = "linux"))]
#[path = "../../audio/tests/support/pwtest.rs"]
mod pwtest;

/// Runs the app until its window closes.
#[cfg(target_os = "linux")]
pub fn run() {
    // The shell injects its identity; the target OS does not choose it
    // (docs/58 D2). First thing, before anything reads `defaults::this()`.
    gawk_engine::defaults::set_this(&gawk_engine::defaults::LINUX);
    // The commit build.rs stamped, for the version badge (crates/ui/build_rev.rs).
    gawk_ui::version::set_build_rev(option_env!("GAWK_BUILD_REV"));
    // Single instance (R66, docs/68 D8): a second launch hands its link,
    // or just a raise, to the running app over the session bus and exits
    // here. Lossy: a non-UTF-8 argument is never a link anyway.
    let args: Vec<String> = std::env::args_os()
        .map(|a| a.to_string_lossy().into_owned())
        .collect();
    let launch = gawk_ui::instance::launch(&args, Box::new(instance::DBus::session()));
    gawk_ui::shell::run(Box::new(platform::Linux::new()), platform::wire, launch);
}
