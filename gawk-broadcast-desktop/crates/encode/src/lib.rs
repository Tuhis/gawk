//! Hardware H.264 encode for both desktop shells: the Media Foundation
//! cascade on Windows (WB4, docs/38 D9) and VideoToolbox's low-latency
//! session on macOS (R52, docs/54 D7).
//!
//! Two halves: [`h264`] is the pure, portable bitstream-inspection layer
//! (codec string from the SPS, IDR classification, the no-B-frames
//! assertion) that runs and tests on any host; the MFT enumeration, trial
//! probes and encoder session are Windows-only and will be cfg-gated behind
//! `#[cfg(windows)]` — the crate compiles without them elsewhere so the
//! workspace builds and its portable tests run anywhere.

pub mod cascade;
pub mod h264;
pub mod vt_policy;

#[cfg(target_os = "macos")]
pub mod vt;

#[cfg(windows)]
pub mod mft;
