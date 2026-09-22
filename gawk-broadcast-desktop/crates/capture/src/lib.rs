//! Screen capture for both desktop shells. Windows: WGC, picker enumeration
//! and the GPU convert/scale pass (WB3, docs/38 D6). macOS: ScreenCaptureKit
//! through the system picker (R52, docs/54 D4).
//!
//! Portable halves — the Windows picker's alt-tab filter, the drop-only fps
//! gate, the aspect fit and the ScreenCaptureKit frame policy — are pure and
//! test on any host. Everything that touches COM/WinRT is `#[cfg(windows)]`
//! and everything that touches ScreenCaptureKit is
//! `#[cfg(target_os = "macos")]`, so the workspace builds and CI's portable
//! tests run on every host.

pub mod fit;
pub mod gate;
pub mod picker;
pub mod sck_policy;

#[cfg(target_os = "macos")]
pub mod host;
#[cfg(target_os = "macos")]
pub mod sck;
#[cfg(target_os = "macos")]
pub mod sck_picker;

#[cfg(windows)]
pub mod d3d;
#[cfg(windows)]
pub mod qpc;
#[cfg(windows)]
pub mod wgc;
