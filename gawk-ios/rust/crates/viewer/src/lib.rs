//! The viewer core (R65, docs/67 D13–D16): the wire contract `gawk-app`
//! reads, in Rust, against the shared `gawk-wire` parsers. The engine is
//! broadcaster-only, so this is new code rather than reuse.
//!
//! Datagrams go through [`reassembly`] into whole frames, keyframes and
//! deltas meet in decode order in [`reorder`], and [`jitter`] and
//! [`playout`] decide how far behind live to present them.

pub mod jitter;
pub mod pipeline;
pub mod playout;
pub mod reassembly;
pub mod reconnect;
pub mod reorder;
pub mod timesync;
