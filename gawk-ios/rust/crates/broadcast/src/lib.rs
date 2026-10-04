//! The broadcast side of the iOS app (R65, docs/67 D6–D12): ScreenCaptureKit
//! video and audio buffers in, the shared engine's publish session out, in
//! the app's own process (OD17). The debug-only test broadcast (D27) feeds it
//! a generated source instead.
//!
//! The policy modules are portable and tested on any host; the pipeline
//! that drives VideoToolbox is Apple-only, as `encode/vt.rs` is.

pub mod audio;
pub mod rotation;
pub mod rung;
