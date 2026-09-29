//! `gawk-broadcast-linux` — everything is in the library (`lib.rs`).

#[cfg(target_os = "linux")]
fn main() {
    gawk_broadcast_app_linux::run();
}

// The Windows cross-build and the macOS job build the whole workspace; this is
// what they compile for this crate. Every dependency is Linux-gated in
// Cargo.toml, so the stub costs them nothing.
#[cfg(not(target_os = "linux"))]
fn main() {
    eprintln!("gawk-broadcast-linux runs on Linux only (docs/58)");
    std::process::exit(1);
}
