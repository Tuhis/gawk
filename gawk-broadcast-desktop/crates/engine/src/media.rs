//! Media types crossing the source→engine seam, and the audio-lane
//! constants inherited verbatim from R25 (docs/38 D8; Go:
//! gawk-broadcast/internal/engine/audio.go). The bitrate is a constant, not
//! a setting.

/// One encoded video access unit.
#[derive(Debug, Clone)]
pub struct AccessUnit {
    pub data: Vec<u8>,
    /// Microseconds on the session clock (docs/38 D7).
    pub timestamp_us: u64,
    /// True when the AU contains an IDR — rides a reliable uni stream.
    pub keyframe: bool,
}

/// One encoded Opus packet.
#[derive(Debug, Clone)]
pub struct AudioPacket {
    pub data: Vec<u8>,
    /// Microseconds on the SAME session clock video stamps with.
    pub timestamp_us: u64,
}

/// What the audio lane advertises in its AudioConfig.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AudioFormat {
    pub codec: String,
    pub sample_rate: u32,
    pub channels: u8,
    pub bitrate_bps: u32,
    /// Which capture path produced it (diagnostics only).
    pub source: String,
}

pub const AUDIO_CODEC: &str = "opus";
pub const AUDIO_SAMPLE_RATE: u32 = 48_000;
pub const AUDIO_CHANNELS: u8 = 2;
pub const AUDIO_BITRATE_BPS: u32 = 128_000;
pub const AUDIO_FRAME_MS: u64 = 20;
/// The AudioConfig re-send cadence: piggybacked on the packet flow, at most
/// once per this many ms (1 Hz — repetition is the whole loss-tolerance
/// story; audio has no keyframe to embed the config in).
pub const AUDIO_CONFIG_RESEND_MS: u64 = 1000;
