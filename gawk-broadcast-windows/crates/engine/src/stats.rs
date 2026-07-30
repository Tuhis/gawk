//! Session counters, mirroring the Go engine's `Stats` — and, like it,
//! serialized with the BROWSER's lowerCamelCase field names. That is
//! load-bearing rather than cosmetic: this struct feeds the diagnostics
//! JSON (WB6) and the R28 telemetry batches (WB7), and everything
//! downstream — the schema's broadcaster fields, the rollup's curated
//! series, the dashboard — matches on the browser's names. The Go engine
//! shipped the mistake once: untagged, it reported `EncoderFps` where every
//! reader looked for `encoderFps` — stored faithfully, readable by nothing.
//! The key-set test below is the pin.
//!
//! Availability flags are explicit fields (`…Available` + value), exactly
//! as the Go struct spells them: an absent number and a zero must stay
//! distinguishable to readers. One honest improvement over the Linux
//! engine: capture fps is REAL here — we own capture (docs/38 D12) — where
//! the Go engine's `captureFpsAvailable` is permanently false because the
//! GStreamer child owns it.

use serde::Serialize;

fn is_zero_u32(v: &u32) -> bool {
    *v == 0
}
fn is_zero_u8(v: &u8) -> bool {
    *v == 0
}

#[derive(Debug, Clone, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct Stats {
    /// The encoder actually in use (e.g. "NVIDIA H264 Encoder MFT"), ""
    /// before one is chosen.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub encoder: String,
    /// The SPS-derived codec string sent to viewers ("avc1.42E02A").
    #[serde(skip_serializing_if = "String::is_empty")]
    pub codec: String,
    /// The session's fixed rung — the browser's `target*` names, which is
    /// what R28's target rules read.
    #[serde(rename = "targetWidth", skip_serializing_if = "is_zero_u32")]
    pub width: u32,
    #[serde(rename = "targetHeight", skip_serializing_if = "is_zero_u32")]
    pub height: u32,
    #[serde(rename = "targetFps", skip_serializing_if = "is_zero_u32")]
    pub fps: u32,
    #[serde(rename = "targetBitrateBps", skip_serializing_if = "is_zero_u32")]
    pub bitrate_bps: u32,

    pub capture_fps_available: bool,
    pub capture_fps: f64,
    /// How frames reach the encoder: "zero-copy" or "tone-mapped" (WB3).
    #[serde(skip_serializing_if = "String::is_empty")]
    pub capture_path: String,

    pub encoded_frames: u64,
    pub keyframes: u64,

    /// EMA (α = 0.3) of the wall-clock keyframe spacing — measured and
    /// displayed, never assumed. Available is false until two keyframes.
    pub keyframe_interval_available: bool,
    pub keyframe_interval_ms: f64,

    /// The level the RELAY advertised (never a local preference).
    pub parity_level: u8,
    pub parity_chunks_sent: u64,
    pub parity_bytes_sent: u64,

    pub sent_frames: u64,
    pub encoder_fps: f64,
    pub sent_fps: f64,

    pub datagrams_sent: u64,
    pub bytes_sent: u64,
    pub configs_sent: u64,

    pub keyframe_streams_sent: u64,
    pub keyframe_streams_failed: u64,
    pub keyframe_streams_superseded: u64,
    pub keyframe_bytes_sent: u64,

    pub frames_dropped_at_send: u64,

    pub time_sync_available: bool,
    pub time_sync_rtt_ms: f64,
    pub time_sync_offset_us: i64,

    pub viewer_count_available: bool,
    pub viewer_count: u32,

    pub resumes: u64,
    pub resuming: bool,

    /// "off" | "unavailable" | "active" | "error" — a machine with no
    /// usable audio source is NOT an error, and this vocabulary says so.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub audio_state: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub audio_source: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub audio_codec: String,
    #[serde(skip_serializing_if = "is_zero_u32")]
    pub audio_sample_rate: u32,
    #[serde(skip_serializing_if = "is_zero_u8")]
    pub audio_channels: u8,
    #[serde(skip_serializing_if = "is_zero_u32")]
    pub audio_bitrate_bps: u32,

    pub audio_packets_sent: u64,
    pub audio_bytes_sent: u64,
    pub audio_configs_sent: u64,
    pub audio_packets_dropped: u64,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeSet;

    // The key-set pin, mirroring the Go stats_test.go: every serialized key
    // is a browser name, spelled exactly. A drift here means telemetry that
    // is stored faithfully and readable by nothing.
    #[test]
    fn serialized_keys_are_the_browser_names() {
        let full = Stats {
            encoder: "e".into(),
            codec: "avc1.42E02A".into(),
            width: 1920,
            height: 1080,
            fps: 60,
            bitrate_bps: 16_000_000,
            capture_path: "zero-copy".into(),
            audio_state: "active".into(),
            audio_source: "process-loopback".into(),
            audio_codec: "opus".into(),
            audio_sample_rate: 48_000,
            audio_channels: 2,
            audio_bitrate_bps: 128_000,
            ..Default::default()
        };
        let v = serde_json::to_value(&full).unwrap();
        let keys: BTreeSet<String> = v.as_object().unwrap().keys().cloned().collect();
        let want: BTreeSet<String> = [
            "encoder",
            "codec",
            "targetWidth",
            "targetHeight",
            "targetFps",
            "targetBitrateBps",
            "captureFpsAvailable",
            "captureFps",
            "capturePath",
            "encodedFrames",
            "keyframes",
            "keyframeIntervalAvailable",
            "keyframeIntervalMs",
            "parityLevel",
            "parityChunksSent",
            "parityBytesSent",
            "sentFrames",
            "encoderFps",
            "sentFps",
            "datagramsSent",
            "bytesSent",
            "configsSent",
            "keyframeStreamsSent",
            "keyframeStreamsFailed",
            "keyframeStreamsSuperseded",
            "keyframeBytesSent",
            "framesDroppedAtSend",
            "timeSyncAvailable",
            "timeSyncRttMs",
            "timeSyncOffsetUs",
            "viewerCountAvailable",
            "viewerCount",
            "resumes",
            "resuming",
            "audioState",
            "audioSource",
            "audioCodec",
            "audioSampleRate",
            "audioChannels",
            "audioBitrateBps",
            "audioPacketsSent",
            "audioBytesSent",
            "audioConfigsSent",
            "audioPacketsDropped",
        ]
        .into_iter()
        .map(String::from)
        .collect();
        assert_eq!(keys, want);
    }

    // Empty-vs-zero honesty: optional strings and the rung vanish when
    // unset; availability-paired numbers stay present with their flag.
    #[test]
    fn unset_optionals_are_absent_not_zero() {
        let v = serde_json::to_value(Stats::default()).unwrap();
        let obj = v.as_object().unwrap();
        for absent in [
            "encoder",
            "codec",
            "targetWidth",
            "audioState",
            "audioCodec",
        ] {
            assert!(!obj.contains_key(absent), "{absent} should be absent");
        }
        assert_eq!(obj["captureFpsAvailable"], false);
        assert_eq!(obj["keyframeIntervalAvailable"], false);
        assert_eq!(obj["viewerCountAvailable"], false);
        assert_eq!(obj["timeSyncAvailable"], false);
    }
}
