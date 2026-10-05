//! Screen capture for the three desktop shells. Windows: WGC, picker
//! enumeration and the GPU convert/scale pass (WB3, docs/38 D6). macOS:
//! ScreenCaptureKit through the system picker (R52, docs/54 D4). Linux: the
//! xdg-desktop-portal grant and the one-clock mapper; the frames themselves
//! are GStreamer's (`gawk_encode::gst`, R56, docs/58 D3/D4).
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

#[cfg(any(target_os = "macos", target_os = "ios"))]
pub mod host;
#[cfg(target_os = "macos")]
pub mod sck;
#[cfg(target_os = "macos")]
pub mod sck_picker;

#[cfg(target_os = "linux")]
pub mod portal;
#[cfg(target_os = "linux")]
pub mod pwclock;

#[cfg(windows)]
pub mod d3d;
#[cfg(windows)]
pub mod qpc;
#[cfg(windows)]
pub mod wgc;
