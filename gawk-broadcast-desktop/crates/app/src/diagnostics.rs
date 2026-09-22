//! `Copy diagnostics` JSON (docs/38 D12.8): the Linux dump's shape with
//! `kind: "gawk-broadcast-windows"` and one honest improvement — capture
//! fps is a real number here (we own capture; the Go shell's is permanently
//! `"n/a"`). Nullable-pointer semantics survive the port: keys for
//! availability-gated numbers are always present, `null` when unmeasured —
//! an absent number and a zero must stay distinguishable to readers.

use gawk_engine::stats::Stats;
use serde::Serialize;

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Diagnostics<'a> {
    kind: &'static str,
    /// The full build string ("1.0.0+g1a2b3c4"), not the bare release the
    /// telemetry wire carries: a pasted dump should say which *build* produced
    /// it, since every EXE in existence is a CI artifact or a local build
    /// rather than a tagged release.
    app_version: String,
    timestamp: String,
    #[serde(skip_serializing_if = "str::is_empty")]
    broadcast_id: &'a str,
    state: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    error: &'a str,

    #[serde(skip_serializing_if = "String::is_empty")]
    encoder: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    capture_path: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    codec: String,
    /// "app" | "screen" — the Windows-only mode axis.
    #[serde(skip_serializing_if = "str::is_empty")]
    capture_mode: &'a str,
    rung: String,

    capture_fps: Option<f64>,
    encoder_fps: f64,
    sent_fps: f64,

    encoded_frames: u64,
    keyframes: u64,
    datagrams_sent: u64,
    bytes_sent: u64,
    configs_sent: u64,
    keyframe_streams_sent: u64,
    keyframe_streams_failed: u64,
    keyframe_streams_superseded: u64,
    frames_dropped_at_send: u64,

    keyframe_interval_ms: Option<f64>,
    time_sync_rtt_ms: Option<f64>,
    time_sync_offset_us: Option<i64>,
    viewer_count: Option<u32>,

    resumes: u64,
    resuming: bool,

    audio_state: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    audio_source: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    audio_codec: String,
    #[serde(skip_serializing_if = "is_zero_u32")]
    audio_sample_rate: u32,
    #[serde(skip_serializing_if = "is_zero_u8")]
    audio_channels: u8,
    #[serde(skip_serializing_if = "is_zero_u32")]
    audio_bitrate_bps: u32,
    audio_packets_sent: u64,
    audio_bytes_sent: u64,
    audio_configs_sent: u64,
    audio_packets_dropped: u64,
}

fn is_zero_u32(v: &u32) -> bool {
    *v == 0
}
fn is_zero_u8(v: &u8) -> bool {
    *v == 0
}

/// Renders the diagnostics dump. `state` is the visible state label
/// ("Not broadcasting" / "Starting…" / "Live").
pub fn render(
    st: &Stats,
    broadcast_id: &str,
    state: &str,
    error: &str,
    capture_mode: &str,
    timestamp_rfc3339: String,
) -> String {
    let d = Diagnostics {
        kind: "gawk-broadcast-windows",
        app_version: crate::version::display(),
        timestamp: timestamp_rfc3339,
        broadcast_id,
        state,
        error,
        encoder: st.encoder.clone(),
        capture_path: st.capture_path.clone(),
        codec: st.codec.clone(),
        capture_mode,
        rung: format!(
            "{}x{}@{} {:.1}Mbps",
            st.width,
            st.height,
            st.fps,
            f64::from(st.bitrate_bps) / 1e6
        ),
        capture_fps: st.capture_fps_available.then_some(st.capture_fps),
        encoder_fps: st.encoder_fps,
        sent_fps: st.sent_fps,
        encoded_frames: st.encoded_frames,
        keyframes: st.keyframes,
        datagrams_sent: st.datagrams_sent,
        bytes_sent: st.bytes_sent,
        configs_sent: st.configs_sent,
        keyframe_streams_sent: st.keyframe_streams_sent,
        keyframe_streams_failed: st.keyframe_streams_failed,
        keyframe_streams_superseded: st.keyframe_streams_superseded,
        frames_dropped_at_send: st.frames_dropped_at_send,
        keyframe_interval_ms: st
            .keyframe_interval_available
            .then_some(st.keyframe_interval_ms),
        time_sync_rtt_ms: st.time_sync_available.then_some(st.time_sync_rtt_ms),
        time_sync_offset_us: st.time_sync_available.then_some(st.time_sync_offset_us),
        viewer_count: st.viewer_count_available.then_some(st.viewer_count),
        resumes: st.resumes,
        resuming: st.resuming,
        audio_state: if st.audio_state.is_empty() {
            "off".into()
        } else {
            st.audio_state.clone()
        },
        audio_source: st.audio_source.clone(),
        audio_codec: st.audio_codec.clone(),
        audio_sample_rate: st.audio_sample_rate,
        audio_channels: st.audio_channels,
        audio_bitrate_bps: st.audio_bitrate_bps,
        audio_packets_sent: st.audio_packets_sent,
        audio_bytes_sent: st.audio_bytes_sent,
        audio_configs_sent: st.audio_configs_sent,
        audio_packets_dropped: st.audio_packets_dropped,
    };
    serde_json::to_string_pretty(&d).unwrap_or_else(|_| "{}".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kind_and_nullable_semantics() {
        let dump = render(
            &Stats::default(),
            "K7XQ2M",
            "Live",
            "",
            "app",
            "2026-07-31T00:00:00Z".into(),
        );
        let v: serde_json::Value = serde_json::from_str(&dump).unwrap();
        assert_eq!(v["kind"], "gawk-broadcast-windows");
        assert_eq!(v["state"], "Live");
        assert_eq!(v["broadcastId"], "K7XQ2M");
        assert_eq!(v["captureMode"], "app");
        // Nullable-pointer semantics: keys present, null when unmeasured.
        for key in [
            "captureFps",
            "keyframeIntervalMs",
            "timeSyncRttMs",
            "timeSyncOffsetUs",
            "viewerCount",
        ] {
            assert!(v.get(key).is_some(), "{key} must be present");
            assert!(v[key].is_null(), "{key} must be null when unavailable");
        }
        // Unset audio reads as off, error key absent when empty.
        assert_eq!(v["audioState"], "off");
        assert!(v.get("error").is_none());
    }

    /// A dump pasted into a bug report has to say which build produced it.
    #[test]
    fn carries_the_build_version() {
        let dump = render(
            &Stats::default(),
            "",
            "Not broadcasting",
            "",
            "",
            "2026-07-31T00:00:00Z".into(),
        );
        let v: serde_json::Value = serde_json::from_str(&dump).unwrap();
        assert_eq!(v["appVersion"], crate::version::display());
        assert!(
            v["appVersion"]
                .as_str()
                .is_some_and(|s| s.starts_with(crate::version::RELEASE)),
            "appVersion must lead with the release"
        );
    }

    #[test]
    fn capture_fps_is_a_real_number_when_measured() {
        let st = Stats {
            capture_fps_available: true,
            capture_fps: 59.7,
            viewer_count_available: true,
            viewer_count: 3,
            ..Default::default()
        };
        let v: serde_json::Value =
            serde_json::from_str(&render(&st, "", "Live", "", "screen", String::new())).unwrap();
        assert!((v["captureFps"].as_f64().unwrap() - 59.7).abs() < 1e-9);
        assert_eq!(v["viewerCount"], 3);
        assert!(v.get("broadcastId").is_none());
    }
}
