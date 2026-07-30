//! WGC capture, picker enumeration and the GPU convert/scale pass (WB3,
//! docs/38 D6).
//!
//! Portable halves — the picker's alt-tab filter and the drop-only fps gate
//! — are pure and test on any host. Everything that touches COM/WinRT is
//! `#[cfg(windows)]`: the crate compiles empty elsewhere so the workspace
//! builds and CI's portable tests run cross-platform.

pub mod gate;
pub mod picker;

#[cfg(windows)]
pub mod d3d;
#[cfg(windows)]
pub mod qpc;
#[cfg(windows)]
pub mod wgc;
