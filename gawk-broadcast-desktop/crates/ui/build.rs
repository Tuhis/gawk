//! Compiles the shared window, moved here from the Windows shell's build
//! script when the macOS shell arrived (docs/54 D1, D11).
//!
//! The build revision is deliberately NOT stamped here any more: each shell
//! stamps it and hands it over at startup. build_rev.rs says why.

fn main() {
    slint_build::compile("main.slint").expect("slint compile");
}
