//! Media Foundation hardware H.264 encoder cascade (WB4, docs/38 D9).
//!
//! Two halves: [`h264`] is the pure, portable bitstream-inspection layer
//! (codec string from the SPS, IDR classification, the no-B-frames
//! assertion) that runs and tests on any host; the MFT enumeration, trial
//! probes and encoder session are Windows-only and will be cfg-gated behind
//! `#[cfg(windows)]` — the crate compiles without them elsewhere so the
//! workspace builds and its portable tests run anywhere.

pub mod h264;
