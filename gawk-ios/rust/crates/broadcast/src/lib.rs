//! The broadcast side of the iOS app (R65, docs/67 D6–D12): ScreenCaptureKit
//! video and audio buffers in, the shared engine's publish session out, in
//! the app's own process (OD17). The debug-only test broadcast (D27) feeds it
//! a generated source instead.
//!
//! IO1 lays the crate down so the workspace, the shared-crate gating (D4) and
//! both iOS targets build; IO2 fills it.
