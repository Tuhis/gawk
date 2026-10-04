//! The viewer core (R65, docs/67 D13–D16): the wire contract `gawk-app`
//! reads, in Rust, against the shared `gawk-wire` parsers. The engine is
//! broadcaster-only, so this is new code rather than reuse.
//!
//! IO1 lays the crate down so the workspace and both iOS targets build; IO4
//! fills it.

pub mod reassembly;
pub mod reorder;
