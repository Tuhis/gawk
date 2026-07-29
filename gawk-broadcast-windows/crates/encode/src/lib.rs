//! WB0 scaffold — implementation lands in its own chunk (docs/38 §8).
//!
//! Windows-only code will be cfg-gated behind `#[cfg(windows)]` with the
//! `windows` crate as a target-specific dependency; the crate compiles empty
//! elsewhere so the workspace builds and its portable tests run on any host.
