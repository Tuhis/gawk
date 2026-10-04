//! The viewer, as Swift sees it (docs/67 D3, D15, D16): one [`Viewer`] per
//! watched broadcast, running the Rust session on a thread of its own and
//! handing a [`ViewerListener`] everything already decoded or ready to
//! enqueue:
//!
//! - H.264 as length-prefixed samples, enqueued **compressed** to an
//!   `AVSampleBufferDisplayLayer`, with the parameter sets whenever the
//!   format description must be (re)built;
//! - VP8/VP9 decoded by libvpx and converted to NV12
//!   (`kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange`);
//! - Opus decoded to interleaved `f32` PCM.
//!
//! Every item carries `present_at_ms` on the clock [`viewer_clock_ms`]
//! reads, which Swift maps onto the host clock its
//! `AVSampleBufferRenderSynchronizer` runs on.
//!
//! Callbacks run on the viewer's thread: Swift hops to its own queues.

use gawk_viewer::decode::h264::H264Stream;
use gawk_viewer::decode::opus::AudioDecoder;
use gawk_viewer::decode::vpx::{I420Frame, VpxCodec, VpxDecoder};
use gawk_viewer::pipeline::{PipelineStats, ViewerEvent};
use gawk_viewer::playout::PlayoutPreset;
use gawk_viewer::session::{
    self, Command, EndReason, ViewerClock, ViewerConfig, ViewerSink, ViewerState, WtSubscribeDialer,
};
use std::sync::{Arc, Mutex, OnceLock};
use tokio::sync::mpsc;

static CLOCK: OnceLock<ViewerClock> = OnceLock::new();

fn clock() -> ViewerClock {
    *CLOCK.get_or_init(ViewerClock::new)
}

/// Milliseconds on the clock every `present_at_ms` is on. Swift samples it
/// beside the host clock once to map one onto the other.
#[uniffi::export]
pub fn viewer_clock_ms() -> f64 {
    clock().now_ms()
}

/// D15's two presets.
#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum Preset {
    Balanced,
    LowestLatency,
}

impl From<Preset> for PlayoutPreset {
    fn from(p: Preset) -> Self {
        match p {
            Preset::Balanced => PlayoutPreset::Balanced,
            Preset::LowestLatency => PlayoutPreset::LowestLatency,
        }
    }
}

#[derive(Debug, Clone, uniffi::Record)]
pub struct ViewerOptions {
    pub relay_url: String,
    pub broadcast_id: String,
    pub preset: Preset,
    /// Skip certificate verification: a local dev relay only.
    pub insecure: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, uniffi::Enum)]
pub enum ViewerEnd {
    /// 4000: the broadcaster stopped and the relay let the broadcast go.
    BroadcastEnded,
    /// 4006: an operator ended the broadcast.
    TerminatedByOperator,
    NotFound,
    GaveUp,
    Stopped,
}

#[derive(Debug, Clone, PartialEq, Eq, uniffi::Enum)]
pub enum ViewerStatus {
    Connecting,
    Live,
    Reconnecting {
        attempt: u32,
        delay_ms: u64,
        /// 4002: the relay is draining for a planned restart.
        draining: bool,
    },
    Ended {
        reason: ViewerEnd,
    },
}

impl From<ViewerState> for ViewerStatus {
    fn from(s: ViewerState) -> Self {
        match s {
            ViewerState::Connecting => Self::Connecting,
            ViewerState::Live => Self::Live,
            ViewerState::Reconnecting {
                attempt,
                delay_ms,
                close_code,
            } => Self::Reconnecting {
                attempt,
                delay_ms,
                draining: close_code == Some(gawk_wire::CLOSE_CODE_SERVER_DRAINING),
            },
            ViewerState::Ended(r) => Self::Ended {
                reason: match r {
                    EndReason::Closed(gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR) => {
                        ViewerEnd::TerminatedByOperator
                    }
                    EndReason::Closed(_) => ViewerEnd::BroadcastEnded,
                    EndReason::NotFound => ViewerEnd::NotFound,
                    EndReason::GaveUp => ViewerEnd::GaveUp,
                    EndReason::Stopped => ViewerEnd::Stopped,
                },
            },
        }
    }
}

/// What `CMVideoFormatDescriptionCreateFromH264ParameterSets` takes.
#[derive(Debug, Clone, PartialEq, Eq, uniffi::Record)]
pub struct H264Format {
    pub sps: Vec<Vec<u8>>,
    pub pps: Vec<Vec<u8>>,
    pub nal_length_size: u8,
}

/// One compressed H.264 access unit, length-prefixed.
#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct H264Sample {
    pub data: Vec<u8>,
    pub keyframe: bool,
    pub timestamp_us: u64,
    pub present_at_ms: f64,
    /// Rebuild the format description from this before enqueueing.
    pub format: Option<H264Format>,
}

/// One decoded VP8/VP9 frame as NV12 (`420YpCbCr8BiPlanarVideoRange`).
#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct Nv12Frame {
    pub width: u32,
    pub height: u32,
    pub y: Vec<u8>,
    pub y_stride: u32,
    /// Interleaved Cb Cr, `ceil(height / 2)` rows.
    pub uv: Vec<u8>,
    pub uv_stride: u32,
    pub timestamp_us: u64,
    pub present_at_ms: f64,
}

/// One decoded audio packet: interleaved `f32`.
#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct PcmBlock {
    pub sample_rate: u32,
    pub channels: u8,
    pub samples: Vec<f32>,
    pub present_at_ms: f64,
}

/// The stats sheet's numbers (G3, G8).
#[derive(Debug, Clone, PartialEq, uniffi::Record)]
pub struct ViewerStats {
    pub offset_ms: f64,
    pub jitter_ms: Option<f64>,
    pub rtt_ms: Option<f64>,
    pub frames_completed: u64,
    pub frames_dropped: u64,
    pub frames_recovered_by_parity: u64,
    pub gap_resyncs: u64,
    pub drops_to_live: u64,
    pub viewer_count: Option<u32>,
}

impl From<PipelineStats> for ViewerStats {
    fn from(s: PipelineStats) -> Self {
        Self {
            offset_ms: s.offset_ms,
            jitter_ms: s.jitter_ms,
            rtt_ms: s.time_sync_rtt_ms,
            frames_completed: s.reassembly.frames_completed,
            frames_dropped: s.reassembly.frames_dropped_incomplete
                + s.reassembly.frames_dropped_late
                + s.reorder.deltas_dropped
                + s.reorder.keyframe_wait_drops,
            frames_recovered_by_parity: s.reassembly.frames_recovered_by_parity,
            gap_resyncs: s.reorder.gap_resyncs,
            drops_to_live: s.drops_to_live,
            viewer_count: s.viewer_count,
        }
    }
}

/// Implemented in Swift.
#[uniffi::export(with_foreign)]
pub trait ViewerListener: Send + Sync {
    fn on_status(&self, status: ViewerStatus);
    fn on_h264(&self, sample: H264Sample);
    fn on_nv12(&self, frame: Nv12Frame);
    fn on_audio(&self, pcm: PcmBlock);
    /// Drop everything queued in both renderers.
    fn on_flush(&self);
    /// The broadcast's codec isn't one this player handles.
    fn on_unsupported_codec(&self, codec: String);
    fn on_stats(&self, stats: ViewerStats);
}

/// The decoder for the config in force.
enum Video {
    None,
    H264(H264Stream),
    Vpx(VpxDecoder),
    Unsupported,
}

/// Turns the session's events into what the listener takes.
struct Bridge {
    listener: Arc<dyn ViewerListener>,
    commands: mpsc::UnboundedSender<Command>,
    state: Mutex<(Video, AudioDecoder)>,
}

impl Bridge {
    fn resync(&self) {
        let _ = self.commands.send(Command::Resync);
    }

    fn on_video(&self, data: &[u8], keyframe: bool, timestamp_us: u64, present_at_ms: f64) {
        let mut state = self.state.lock().unwrap();
        match &mut state.0 {
            Video::H264(stream) => match stream.sample(data) {
                Ok(s) => {
                    let format = if s.format_changed {
                        stream.parameter_sets().map(|ps| H264Format {
                            sps: ps.sps.clone(),
                            pps: ps.pps.clone(),
                            nal_length_size: stream.nal_length_size(),
                        })
                    } else {
                        None
                    };
                    drop(state);
                    self.listener.on_h264(H264Sample {
                        data: s.data,
                        keyframe,
                        timestamp_us,
                        present_at_ms,
                        format,
                    });
                }
                // A frame that can't be framed: freeze to the next keyframe
                // rather than feed a broken chain (favor dropped frames).
                Err(_) => {
                    drop(state);
                    self.resync();
                }
            },
            Video::Vpx(dec) => match dec.decode(data) {
                Ok(frames) => {
                    drop(state);
                    for f in frames {
                        self.listener.on_nv12(nv12(&f, timestamp_us, present_at_ms));
                    }
                }
                Err(_) => {
                    drop(state);
                    self.resync();
                }
            },
            Video::None | Video::Unsupported => {}
        }
    }
}

impl ViewerSink for Bridge {
    fn state(&self, state: ViewerState) {
        self.listener.on_status(state.into());
    }

    fn event(&self, event: ViewerEvent) {
        match event {
            ViewerEvent::VideoConfig(c) => {
                let video = if c.codec.starts_with("avc1") || c.codec.starts_with("avc3") {
                    H264Stream::new(&c.codec, &c.extradata).map_or(Video::Unsupported, Video::H264)
                } else if let Some(codec) = VpxCodec::from_codec_string(&c.codec) {
                    VpxDecoder::new(codec).map_or(Video::Unsupported, Video::Vpx)
                } else {
                    Video::Unsupported
                };
                let unsupported = matches!(video, Video::Unsupported);
                self.state.lock().unwrap().0 = video;
                if unsupported {
                    self.listener.on_unsupported_codec(c.codec);
                }
            }
            ViewerEvent::VideoFrame {
                keyframe,
                timestamp_us,
                data,
                present_at_ms,
                ..
            } => self.on_video(&data, keyframe, timestamp_us, present_at_ms),
            ViewerEvent::AudioConfig(c) => {
                let cfg = gawk_wire::AudioConfig {
                    codec: &c.codec,
                    sample_rate: c.sample_rate,
                    channels: c.channels,
                    description: &c.description,
                };
                // Audio never fails a broadcast (R25 Decision 6): a config it
                // can't take leaves the lane silent and video playing.
                let _ = self.state.lock().unwrap().1.configure(&cfg);
            }
            ViewerEvent::Audio {
                packet,
                present_at_ms,
            } => {
                let pcm = self.state.lock().unwrap().1.decode(&packet.payload);
                if let Ok(Some(p)) = pcm {
                    self.listener.on_audio(PcmBlock {
                        sample_rate: p.sample_rate,
                        channels: p.channels,
                        samples: p.samples,
                        present_at_ms,
                    });
                }
            }
            ViewerEvent::Flush => self.listener.on_flush(),
            ViewerEvent::Stats(s) => self.listener.on_stats(s.into()),
            ViewerEvent::ViewerCount(_)
            | ViewerEvent::TelemetryHello { .. }
            | ViewerEvent::TelemetryEndpoint(_) => {}
        }
    }
}

/// I420 → NV12: the luma rows as they are, the two chroma planes
/// interleaved, every plane tightly packed.
fn nv12(f: &I420Frame, timestamp_us: u64, present_at_ms: f64) -> Nv12Frame {
    let (w, h) = (f.width, f.height);
    let mut y = Vec::with_capacity(w * h);
    for row in 0..h {
        let start = row * f.y.stride;
        y.extend_from_slice(&f.y.data[start..start + w]);
    }
    let (cw, ch) = (w.div_ceil(2), h.div_ceil(2));
    let mut uv = Vec::with_capacity(cw * 2 * ch);
    for row in 0..ch {
        let (u, v) = (row * f.u.stride, row * f.v.stride);
        for col in 0..cw {
            uv.push(f.u.data[u + col]);
            uv.push(f.v.data[v + col]);
        }
    }
    Nv12Frame {
        width: w as u32,
        height: h as u32,
        y,
        y_stride: w as u32,
        uv,
        uv_stride: (cw * 2) as u32,
        timestamp_us,
        present_at_ms,
    }
}

/// One watched broadcast. Dropping it (or [`Viewer::stop`]) ends it.
#[derive(uniffi::Object)]
pub struct Viewer {
    commands: mpsc::UnboundedSender<Command>,
}

#[uniffi::export]
impl Viewer {
    /// Starts watching; status and media arrive on `listener`.
    #[uniffi::constructor]
    pub fn start(options: ViewerOptions, listener: Arc<dyn ViewerListener>) -> Arc<Self> {
        let (tx, rx) = mpsc::unbounded_channel();
        let bridge = Arc::new(Bridge {
            listener,
            commands: tx.clone(),
            state: Mutex::new((Video::None, AudioDecoder::new())),
        });
        let cfg = ViewerConfig {
            relay_url: options.relay_url,
            broadcast_id: options.broadcast_id,
            preset: options.preset.into(),
        };
        let dialer = Arc::new(WtSubscribeDialer {
            origin: gawk_engine::defaults::origin().to_owned(),
            insecure: options.insecure,
        });
        // One thread, one current-thread runtime per viewer (D12's shape).
        std::thread::Builder::new()
            .name("gawk-viewer".into())
            .spawn(move || {
                let rt = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                    .expect("a tokio runtime");
                rt.block_on(session::run(cfg, dialer, bridge, clock(), rx));
            })
            .expect("spawn the viewer thread");
        Arc::new(Self { commands: tx })
    }

    /// The renderer put this frame on screen (D15's drop-to-live input).
    pub fn presented(&self, timestamp_us: u64) {
        let _ = self.commands.send(Command::Presented { timestamp_us });
    }

    /// The renderer's queue is too deep: resync at the next keyframe.
    pub fn resync(&self) {
        let _ = self.commands.send(Command::Resync);
    }

    pub fn set_preset(&self, preset: Preset) {
        let _ = self.commands.send(Command::SetPreset(preset.into()));
    }

    pub fn stop(&self) {
        let _ = self.commands.send(Command::Stop);
    }
}

impl Drop for Viewer {
    fn drop(&mut self) {
        let _ = self.commands.send(Command::Stop);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use gawk_viewer::decode::vpx::Plane;

    #[test]
    fn nv12_packs_luma_and_interleaves_chroma() {
        // 3×3 with padded strides: chroma is ceil(3/2) = 2 square.
        let plane = |stride, w, rows, base: u8| Plane {
            data: (0..stride * rows).map(|i| base + i as u8).collect(),
            stride,
            width: w,
            rows,
        };
        let f = I420Frame {
            width: 3,
            height: 3,
            y: plane(4, 3, 3, 0),
            u: plane(3, 2, 2, 100),
            v: plane(3, 2, 2, 200),
        };
        let out = nv12(&f, 7, 1.5);
        assert_eq!(out.y, [0, 1, 2, 4, 5, 6, 8, 9, 10]);
        assert_eq!(out.uv, [100, 200, 101, 201, 103, 203, 104, 204]);
        assert_eq!((out.y_stride, out.uv_stride), (3, 4));
        assert_eq!((out.width, out.height, out.timestamp_us), (3, 3, 7));
    }

    #[test]
    fn statuses_name_the_terminal_codes() {
        let s: ViewerStatus = ViewerState::Ended(EndReason::Closed(
            gawk_wire::CLOSE_CODE_TERMINATED_BY_OPERATOR,
        ))
        .into();
        assert_eq!(
            s,
            ViewerStatus::Ended {
                reason: ViewerEnd::TerminatedByOperator
            }
        );
        let s: ViewerStatus = ViewerState::Reconnecting {
            attempt: 1,
            delay_ms: 0,
            close_code: Some(gawk_wire::CLOSE_CODE_SERVER_DRAINING),
        }
        .into();
        assert!(matches!(
            s,
            ViewerStatus::Reconnecting { draining: true, .. }
        ));
    }
}
