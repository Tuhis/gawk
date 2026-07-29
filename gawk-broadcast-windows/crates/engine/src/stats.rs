//! Session counters, mirroring the Go engine's `Stats`. When these are
//! serialized (diagnostics JSON, R28 telemetry batches — WB7) the field
//! names MUST be the browser's lowerCamelCase names; the Go engine learned
//! that the hard way (untagged names broke every downstream reader).

#[derive(Debug, Clone, Default, PartialEq)]
pub struct Stats {
    pub encoded_frames: u64,
    pub sent_frames: u64,
    pub keyframes: u64,
    pub datagrams_sent: u64,
    pub bytes_sent: u64,
    pub frames_dropped_at_send: u64,
    pub keyframe_streams_sent: u64,
    pub keyframe_streams_failed: u64,
    pub keyframe_streams_superseded: u64,
    pub keyframe_bytes_sent: u64,
    pub configs_sent: u64,
    pub codec: Option<String>,
    /// Measured keyframe cadence (EMA, α = 0.3) — measured and displayed,
    /// never assumed: damage-driven capture stretches the wall-clock GOP.
    pub keyframe_interval_ms: Option<f64>,

    pub parity_level: u8,
    pub parity_chunks_sent: u64,
    pub parity_bytes_sent: u64,

    pub audio_packets_sent: u64,
    pub audio_packets_dropped: u64,
    pub audio_bytes_sent: u64,
    pub audio_configs_sent: u64,
    pub audio_codec: Option<String>,
    pub audio_sample_rate: u32,
    pub audio_channels: u8,
    pub audio_bitrate_bps: u32,
    pub audio_source: Option<String>,
    /// Latches the R25 Decision-10 refusal: a bitstream that disagrees with
    /// the advertised config is not shipped.
    pub audio_errored: bool,

    pub viewer_count: Option<u32>,
    pub resumes: u64,
    pub resuming: bool,
}
