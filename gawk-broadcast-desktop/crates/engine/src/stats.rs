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

use crate::memory;
use serde::Serialize;

fn is_zero_u32(v: &u32) -> bool {
    *v == 0
}
fn is_zero_u8(v: &u8) -> bool {
    *v == 0
}
fn is_zero_u64(v: &u64) -> bool {
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

    /// QUIC's own packet counters, summed over every connection of the
    /// broadcast (resumes included): the one local view of leg-A loss
    /// (docs/57 D5). Loss in the air after a successful send moves nothing
    /// above; these see it through QUIC's ack-based loss detection.
    pub uplink_packets_available: bool,
    pub uplink_packets_sent: u64,
    pub uplink_packets_lost: u64,
    /// Datagrams the engine handed to QUIC that never left in a packet:
    /// offered minus quinn's `frame_tx.datagram`, per connection, summed
    /// over the broadcast (docs/75 D8). quinn drops the oldest datagram
    /// when its send buffer is full and says nothing, so this is the only
    /// count of those drops. It includes, momentarily, the datagrams still
    /// queued in that buffer when the stats were read — at most a buffer's
    /// worth, and they drain into `sent` by the next read; the stack does
    /// not expose the queue's length. Shares `uplinkPacketsAvailable`:
    /// both come from the same QUIC stack.
    pub uplink_datagrams_dropped: u64,

    /// R73 (docs/75 D8): how the current GOP's deltas leave — "datagram" or
    /// "carrier" — and the Delivery setting asked for ("reliable" |
    /// "lowest-latency"); they differ when the relay offers no carriers.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub uplink_mode: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub uplink_mode_requested: String,
    /// The QUIC congestion controller this connection runs: "cubic" | "bbr"
    /// (docs/75 D5).
    #[serde(skip_serializing_if = "String::is_empty")]
    pub uplink_congestion: String,
    /// Carriers opened, and reset before they were fully acknowledged; and
    /// delta records that never reached a carrier (docs/75 D3). In Reliable
    /// delivery these, not lost packets, are what viewers lose.
    pub uplink_carriers_opened: u64,
    pub uplink_carriers_expired: u64,
    pub uplink_records_shed: u64,

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

    /// Linux (docs/58 D6/D14, the Go app's fields — `gawk-telemetry` already
    /// types all three, from R35): completed mid-session capture rebuilds,
    /// each a freeze a viewer saw. Absent at zero, so the Windows and macOS
    /// wire is unchanged (G12).
    #[serde(skip_serializing_if = "is_zero_u64")]
    pub capture_restarts: u64,
    /// What the portal picker returned: "screen" or "window".
    #[serde(skip_serializing_if = "String::is_empty")]
    pub share_mode: String,
    /// The application whose audio is captured, by process binary.
    #[serde(skip_serializing_if = "String::is_empty")]
    pub audio_app: String,

    /// R76 (docs/77 D2): the process footprint in the figure the OS's own
    /// task manager shows (Windows' private working set, Apple's
    /// `phys_footprint`, Linux's anonymous + shared resident), its peak, and
    /// every resident page. False where the OS could not be read.
    pub mem_available: bool,
    pub mem_footprint_bytes: u64,
    pub mem_footprint_peak_bytes: u64,
    pub mem_resident_bytes: u64,
    /// Windows' committed private memory ("Commit size"): a leak that
    /// pages out leaves the working set and stays here. Absent elsewhere.
    #[serde(skip_serializing_if = "is_zero_u64")]
    pub mem_commit_bytes: u64,
    /// iOS: bytes left before jetsam kills the app. Absent elsewhere.
    #[serde(skip_serializing_if = "is_zero_u64")]
    pub mem_limit_remaining_bytes: u64,
    /// The Rust heap, live and peak (D3): footprint minus this is memory
    /// held through libraries — the GPU driver, the media frameworks, the
    /// UI renderer. False where the shell runs without the counting
    /// allocator.
    pub mem_heap_available: bool,
    pub mem_heap_bytes: u64,
    pub mem_heap_peak_bytes: u64,
    /// Video memory charged to the process (D5): DXGI's per-process usage
    /// on Windows, the kernel's graphics ledger on Apple. False on Linux.
    pub mem_gpu_available: bool,
    pub mem_gpu_bytes: u64,
    /// What each component says it holds (D4); absent until it reports.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem_capture_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem_encode_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem_send_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem_preview_bytes: Option<u64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub mem_telemetry_bytes: Option<u64>,
}

impl Stats {
    /// Copies a memory snapshot into the `mem*` fields (docs/77 D6).
    pub fn apply_memory(&mut self, m: &memory::Snapshot) {
        if let Some(p) = m.process {
            self.mem_available = true;
            self.mem_footprint_bytes = p.footprint;
            self.mem_footprint_peak_bytes = m.footprint_peak;
            self.mem_resident_bytes = p.resident;
            self.mem_commit_bytes = p.commit;
            self.mem_limit_remaining_bytes = p.limit_remaining;
        }
        if let Some((live, peak)) = m.heap {
            self.mem_heap_available = true;
            self.mem_heap_bytes = live;
            self.mem_heap_peak_bytes = peak;
        }
        if let Some(gpu) = m.gpu {
            self.mem_gpu_available = true;
            self.mem_gpu_bytes = gpu;
        }
        let [capture, encode, send, preview, telemetry] = m.components;
        self.mem_capture_bytes = capture;
        self.mem_encode_bytes = encode;
        self.mem_send_bytes = send;
        self.mem_preview_bytes = preview;
        self.mem_telemetry_bytes = telemetry;
    }
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
            capture_restarts: 2,
            share_mode: "window".into(),
            audio_app: "hl2_linux".into(),
            uplink_mode: "carrier".into(),
            uplink_mode_requested: "reliable".into(),
            uplink_congestion: "bbr".into(),
            mem_commit_bytes: 1,
            mem_limit_remaining_bytes: 1,
            mem_capture_bytes: Some(1),
            mem_encode_bytes: Some(1),
            mem_send_bytes: Some(1),
            mem_preview_bytes: Some(1),
            mem_telemetry_bytes: Some(1),
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
            "uplinkPacketsAvailable",
            "uplinkPacketsSent",
            "uplinkPacketsLost",
            "uplinkDatagramsDropped",
            "uplinkMode",
            "uplinkModeRequested",
            "uplinkCongestion",
            "uplinkCarriersOpened",
            "uplinkCarriersExpired",
            "uplinkRecordsShed",
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
            "captureRestarts",
            "shareMode",
            "audioApp",
            "memAvailable",
            "memFootprintBytes",
            "memFootprintPeakBytes",
            "memResidentBytes",
            "memCommitBytes",
            "memLimitRemainingBytes",
            "memHeapAvailable",
            "memHeapBytes",
            "memHeapPeakBytes",
            "memGpuAvailable",
            "memGpuBytes",
            "memCaptureBytes",
            "memEncodeBytes",
            "memSendBytes",
            "memPreviewBytes",
            "memTelemetryBytes",
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
            "captureRestarts",
            "shareMode",
            "audioApp",
            "uplinkMode",
            "uplinkModeRequested",
            "uplinkCongestion",
            "memCommitBytes",
            "memLimitRemainingBytes",
            "memCaptureBytes",
        ] {
            assert!(!obj.contains_key(absent), "{absent} should be absent");
        }
        assert_eq!(obj["memAvailable"], false);
        assert_eq!(obj["memHeapAvailable"], false);
        assert_eq!(obj["memGpuAvailable"], false);
        assert_eq!(obj["captureFpsAvailable"], false);
        assert_eq!(obj["keyframeIntervalAvailable"], false);
        assert_eq!(obj["viewerCountAvailable"], false);
        assert_eq!(obj["timeSyncAvailable"], false);
        assert_eq!(obj["uplinkPacketsAvailable"], false);
    }
}
