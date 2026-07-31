//! Audio capture + Opus encode (WB5, docs/38 D8): WASAPI process loopback
//! (mode 1 — the Windows-only capability this milestone exists for) and
//! endpoint loopback (mode 2), feeding libopus under R25's contract,
//! verbatim: 48 kHz/stereo forced, 128 kbps constant, 20 ms frames, no
//! DTX, no FEC, restricted-low-delay.
//!
//! Portable halves — the TOC verification (R25 Decision 10), the 20 ms
//! framer with its QPC/arrival stamping fallback (V-4), the level meter
//! behind the D8 silence hint, and the Opus encoder wrapper itself (libopus
//! is portable) — all test on any host. Only the WASAPI plumbing is
//! `#[cfg(windows)]`.

pub mod framer;
pub mod level;
pub mod opusenc;
pub mod toc;

#[cfg(windows)]
pub mod wasapi;

/// Which capture the session runs — the picker's mode decides.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AudioMode {
    /// Mode 1: just this process tree's audio (D8).
    ProcessLoopback { pid: u32 },
    /// Mode 2: whole-system audio off the default render endpoint.
    SystemLoopback,
    /// Audio disabled by the user.
    Off,
}

/// The advertised format, fixed by R25: the constants live in
/// `gawk_engine::media` and this is the only constructor, so a drifting
/// format cannot be advertised.
pub fn advertised_format(source: &str) -> gawk_engine::media::AudioFormat {
    gawk_engine::media::AudioFormat {
        codec: gawk_engine::media::AUDIO_CODEC.into(),
        sample_rate: gawk_engine::media::AUDIO_SAMPLE_RATE,
        channels: gawk_engine::media::AUDIO_CHANNELS,
        bitrate_bps: gawk_engine::media::AUDIO_BITRATE_BPS,
        source: source.into(),
    }
}
