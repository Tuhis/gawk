//! Compiles the shared window, moved here from the Windows shell's build
//! script when the macOS shell arrived (docs/54 D1, D11).
//!
//! The build revision is deliberately NOT stamped here any more: each shell
//! stamps it and hands it over at startup. build_rev.rs says why.

fn main() {
    // Debug info is what the testing backend's ElementHandle finds elements
    // by, so the window tests can click and hover the real components. Dev
    // builds only: `PROFILE` is "release" for release and every profile that
    // inherits it (`ci`), so nothing shipped carries it.
    let debug_info = std::env::var("PROFILE").as_deref() == Ok("debug");
    let config = slint_build::CompilerConfiguration::new().with_debug_info(debug_info);
    slint_build::compile_with_config("main.slint", config).expect("slint compile");
}
