//! The ScreenCaptureKit capture's decisions, with the framework taken out
//! (R52 MB2, docs/54 D3/D4/D10): which frames go on to the encoder, when the
//! picked content has gone stale, what a picked filter means for audio, and
//! the panic fence every Objective-C callback runs behind. Pure and portable
//! so every rule is a host test; `sck` and `sck_picker` only translate
//! framework values into these types and back.

use crate::gate::{FpsGate, FpsMeter};
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::atomic::{AtomicBool, Ordering};

/// `SCFrameStatus` (SCStream.h), restated so this module needs no bindings.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum FrameStatus {
    /// New content: the only status that reaches the encoder.
    Complete,
    /// Nothing changed since the last frame — SCK is damage-driven.
    Idle,
    /// No content, e.g. the window is minimized or off-screen.
    Blank,
    Suspended,
    Started,
    Stopped,
    /// A value a newer SDK added; treated like `Blank` (never encoded).
    Unknown(i64),
}

impl FrameStatus {
    pub fn from_raw(raw: i64) -> Self {
        match raw {
            0 => Self::Complete,
            1 => Self::Idle,
            2 => Self::Blank,
            3 => Self::Suspended,
            4 => Self::Started,
            5 => Self::Stopped,
            other => Self::Unknown(other),
        }
    }
}

/// Why a frame did not go on to the encoder, for the stats card.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Drop {
    /// Not a `.complete` frame (docs/54 D4: no new content).
    NoNewContent,
    /// Over the rung's fps (the shared drop-only gate).
    OverRate,
}

/// The per-frame gate (docs/54 D4): only `.complete` frames pass, and those
/// only through the drop-only [`FpsGate`]. Never synthesizes a frame (R14
/// Decision 13) — an idle desktop sends nothing, and that is the truth.
#[derive(Debug)]
pub struct Admission {
    gate: FpsGate,
    meter: FpsMeter,
    stale: StaleWatch,
    pub admitted: u64,
    pub dropped_no_content: u64,
    pub dropped_over_rate: u64,
}

impl Admission {
    pub fn new(target_fps: u32) -> Self {
        Self {
            gate: FpsGate::new(target_fps),
            meter: FpsMeter::default(),
            stale: StaleWatch::default(),
            admitted: 0,
            dropped_no_content: 0,
            dropped_over_rate: 0,
        }
    }

    /// Judges one frame stamped `ts_us` on the session clock.
    pub fn judge(&mut self, status: FrameStatus, ts_us: u64) -> Result<(), Drop> {
        self.stale.observe(status, ts_us);
        if status != FrameStatus::Complete {
            self.dropped_no_content += 1;
            return Err(Drop::NoNewContent);
        }
        if !self.gate.admit(ts_us) {
            self.dropped_over_rate += 1;
            return Err(Drop::OverRate);
        }
        self.meter.observe(ts_us);
        self.admitted += 1;
        Ok(())
    }

    /// Measured fps of admitted frames; `None` until two have passed.
    pub fn fps(&self) -> Option<f64> {
        self.meter.fps()
    }

    /// Whether the minimized/hidden hint should show at `now_us`.
    pub fn stale(&self, now_us: u64) -> bool {
        self.stale.stale(now_us)
    }
}

/// How long without live content before the GUI says so (docs/54 D4).
pub const STALE_AFTER_US: u64 = 1_000_000;

/// Tracks whether the picked content is still *there* (docs/54 D4's
/// minimized hint). `.complete` and `.idle` both mean the content exists —
/// an unchanging window is not a minimized one — so only a run of
/// `.blank`/`.suspended` frames, or no frames at all, for longer than
/// [`STALE_AFTER_US`] counts. Nothing is stale before the first frame: a
/// stream that is still starting is not a hidden window.
#[derive(Debug, Default)]
pub struct StaleWatch {
    last_live_us: Option<u64>,
}

impl StaleWatch {
    pub fn observe(&mut self, status: FrameStatus, ts_us: u64) {
        if matches!(status, FrameStatus::Complete | FrameStatus::Idle) {
            self.last_live_us = Some(ts_us);
        }
    }

    pub fn stale(&self, now_us: u64) -> bool {
        self.last_live_us
            .is_some_and(|t| now_us.saturating_sub(t) > STALE_AFTER_US)
    }
}

/// What the system picker returned, from `SCContentFilter.style`
/// (`SCShareableContentStyle`: none 0, window 1, display 2, application 3).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ShareStyle {
    Window,
    Application,
    Display,
}

impl ShareStyle {
    /// `None` for `SCShareableContentStyle.none` and unknown values: a
    /// filter the picker should never hand back, refused rather than
    /// guessed at.
    pub fn from_raw(raw: i64) -> Option<Self> {
        match raw {
            1 => Some(Self::Window),
            2 => Some(Self::Display),
            3 => Some(Self::Application),
            _ => None,
        }
    }

    /// docs/54 D6: SCK scopes audio by the filter — a window or application
    /// filter yields that app's audio only, a display filter every app's
    /// but ours. One API, no PID plumbing.
    pub fn audio_scope(self) -> AudioScope {
        match self {
            Self::Window | Self::Application => AudioScope::ThisApp,
            Self::Display => AudioScope::WholeSystem,
        }
    }

    /// The diagnostics/telemetry `captureMode` — the Windows shell's
    /// vocabulary, so the dashboard reads both platforms the same way.
    pub fn capture_mode(self) -> &'static str {
        match self.audio_scope() {
            AudioScope::ThisApp => "app",
            AudioScope::WholeSystem => "screen",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AudioScope {
    ThisApp,
    WholeSystem,
}

impl AudioScope {
    /// The Share card's mode label (docs/54 D11).
    pub fn label(self) -> &'static str {
        match self {
            Self::ThisApp => "this app's audio",
            Self::WholeSystem => "whole-system audio",
        }
    }
}

/// Frames the encoder may hold at once before the pipeline drops instead
/// of submitting (docs/54 D10 — favor dropped frames over stalled playback).
/// The Windows value; V-5 measures whether it holds at 1080p60.
pub const ENCODER_MAX_IN_FLIGHT: usize = 3;

/// `SCStreamConfiguration.queueDepth` (docs/54 D10): the surfaces SCK keeps
/// outstanding. Two more than the encoder can hold, so SCK never stalls on
/// our account even when every in-flight slot is full and one buffer sits
/// in the output callback.
pub const QUEUE_DEPTH: usize = ENCODER_MAX_IN_FLIGHT + 2;

// The pin, and SCK's documented 3–8 range for the property.
const _: () = assert!(QUEUE_DEPTH == ENCODER_MAX_IN_FLIGHT + 2);
const _: () = assert!(QUEUE_DEPTH >= 3 && QUEUE_DEPTH <= 8);

/// The panic fence for every Objective-C callback (docs/54 D3). Unwinding
/// through an Objective-C frame aborts the process, so a Rust panic inside
/// a delegate or output method must stop *here*: it is caught, reported
/// once through `on_fail` (which ends the broadcast as a session error), and
/// every later call is skipped — a capture that has already panicked is not
/// trusted with another frame.
pub struct CallbackGuard {
    failed: AtomicBool,
    on_fail: Box<dyn Fn(String) + Send + Sync>,
}

impl CallbackGuard {
    pub fn new(on_fail: impl Fn(String) + Send + Sync + 'static) -> Self {
        Self {
            failed: AtomicBool::new(false),
            on_fail: Box::new(on_fail),
        }
    }

    /// Runs `f` unless an earlier call panicked; `None` when skipped or
    /// when `f` panicked.
    pub fn run<R>(&self, f: impl FnOnce() -> R) -> Option<R> {
        if self.failed.load(Ordering::Acquire) {
            return None;
        }
        match catch_unwind(AssertUnwindSafe(f)) {
            Ok(r) => Some(r),
            Err(payload) => {
                if !self.failed.swap(true, Ordering::AcqRel) {
                    let msg = payload
                        .downcast_ref::<&str>()
                        .map(|s| (*s).to_owned())
                        .or_else(|| payload.downcast_ref::<String>().cloned())
                        .unwrap_or_else(|| "non-string panic payload".to_owned());
                    (self.on_fail)(format!("capture callback panicked: {msg}"));
                }
                None
            }
        }
    }

    pub fn failed(&self) -> bool {
        self.failed.load(Ordering::Acquire)
    }
}

/// A `CMTime` on the host clock as 100 ns ticks, the unit the engine's
/// [`gawk_engine::clock::QpcMapper`] maps (docs/54 D5: D7's single clock
/// with `mach_absolute_time` in QPC's place — the mapper is unit-agnostic
/// affine arithmetic). `None` for an invalid time or a non-positive
/// timescale; i128 keeps a nanosecond timescale exact at any uptime.
pub fn cm_time_to_100ns(value: i64, timescale: i32, valid: bool) -> Option<i64> {
    if !valid || timescale <= 0 {
        return None;
    }
    i64::try_from(i128::from(value) * 10_000_000 / i128::from(timescale)).ok()
}

/// One plane of a locked `420v` pixel buffer: rows of `stride` bytes.
pub struct Plane<'a> {
    pub data: &'a [u8],
    pub stride: usize,
}

/// A `max_w`×`max_h`-bounded RGBA thumbnail of a `420v` (NV12, BT.709
/// video range) frame: the Share card's 1 Hz "what viewers see" (docs/54
/// D11), sampled nearest-neighbour straight off the capture buffer — no
/// second stream, no second permission, and at thumbnail size and 1 Hz
/// cheaper than a vImage round trip. Returns `(width, height, rgba)`.
pub fn nv12_thumbnail(
    y: &Plane<'_>,
    uv: &Plane<'_>,
    width: u32,
    height: u32,
    max_w: u32,
    max_h: u32,
) -> (u32, u32, Vec<u8>) {
    let (tw, th) = crate::fit::fit_within(width, height, max_w, max_h);
    let mut out = Vec::with_capacity((tw * th * 4) as usize);
    for ty in 0..th {
        let sy = (u64::from(ty) * u64::from(height) / u64::from(th)) as usize;
        for tx in 0..tw {
            let sx = (u64::from(tx) * u64::from(width) / u64::from(tw)) as usize;
            let luma = y.data.get(sy * y.stride + sx).copied().unwrap_or(16);
            let c = (sy / 2) * uv.stride + (sx / 2) * 2;
            let cb = uv.data.get(c).copied().unwrap_or(128);
            let cr = uv.data.get(c + 1).copied().unwrap_or(128);
            let [r, g, b] = bt709_video_to_rgb(luma, cb, cr);
            out.extend_from_slice(&[r, g, b, 255]);
        }
    }
    (tw, th, out)
}

/// BT.709, video (limited) range — `kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange`
/// with `colorSpaceName = ITU_R_709_2` (docs/54 D4).
fn bt709_video_to_rgb(y: u8, cb: u8, cr: u8) -> [u8; 3] {
    let y = (f32::from(y) - 16.0) * (255.0 / 219.0);
    let cb = (f32::from(cb) - 128.0) * (255.0 / 224.0);
    let cr = (f32::from(cr) - 128.0) * (255.0 / 224.0);
    let clamp = |v: f32| v.round().clamp(0.0, 255.0) as u8;
    [
        clamp(y + 1.5748 * cr),
        clamp(y - 0.1873 * cb - 0.4681 * cr),
        clamp(y + 1.8556 * cb),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};

    #[test]
    fn frame_status_maps_the_sdk_values() {
        assert_eq!(FrameStatus::from_raw(0), FrameStatus::Complete);
        assert_eq!(FrameStatus::from_raw(1), FrameStatus::Idle);
        assert_eq!(FrameStatus::from_raw(2), FrameStatus::Blank);
        assert_eq!(FrameStatus::from_raw(3), FrameStatus::Suspended);
        assert_eq!(FrameStatus::from_raw(4), FrameStatus::Started);
        assert_eq!(FrameStatus::from_raw(5), FrameStatus::Stopped);
        assert_eq!(FrameStatus::from_raw(9), FrameStatus::Unknown(9));
    }

    #[test]
    fn only_complete_frames_reach_the_encoder() {
        let mut a = Admission::new(60);
        for (i, status) in [
            FrameStatus::Idle,
            FrameStatus::Blank,
            FrameStatus::Suspended,
            FrameStatus::Started,
            FrameStatus::Stopped,
            FrameStatus::Unknown(7),
        ]
        .into_iter()
        .enumerate()
        {
            assert_eq!(
                a.judge(status, i as u64 * 20_000),
                Err(Drop::NoNewContent),
                "{status:?}"
            );
        }
        assert_eq!(a.judge(FrameStatus::Complete, 200_000), Ok(()));
        assert_eq!((a.admitted, a.dropped_no_content), (1, 6));
    }

    #[test]
    fn complete_frames_are_gated_drop_only_to_the_rung() {
        let mut a = Admission::new(30);
        // A 120 Hz display delivering new content on every refresh.
        let passed = (0..120u64)
            .filter(|i| a.judge(FrameStatus::Complete, i * 8_333).is_ok())
            .count();
        assert!((28..=32).contains(&passed), "passed {passed}");
        assert_eq!(a.admitted as usize, passed);
        assert_eq!(a.dropped_over_rate as usize, 120 - passed);
        assert!((25.0..35.0).contains(&a.fps().unwrap()));
    }

    #[test]
    fn an_idle_desktop_sends_nothing_and_is_not_stale() {
        let mut a = Admission::new(60);
        assert_eq!(a.judge(FrameStatus::Complete, 0), Ok(()));
        for i in 1..=50u64 {
            assert!(a.judge(FrameStatus::Idle, i * 100_000).is_err());
        }
        assert_eq!(a.admitted, 1, "no frame synthesized for stillness");
        assert!(!a.stale(5_000_000 + 500_000), "unchanging is not hidden");
    }

    #[test]
    fn blank_frames_go_stale_after_a_second() {
        let mut w = StaleWatch::default();
        assert!(
            !w.stale(10_000_000),
            "nothing is stale before the first frame"
        );
        w.observe(FrameStatus::Complete, 0);
        w.observe(FrameStatus::Blank, 400_000);
        w.observe(FrameStatus::Blank, 900_000);
        assert!(!w.stale(1_000_000));
        assert!(w.stale(1_000_001));
        // Restoring the window brings content back; the hint clears.
        w.observe(FrameStatus::Complete, 3_000_000);
        assert!(!w.stale(3_500_000));
        // No frames at all is stale too (delivery simply stopped).
        assert!(w.stale(4_000_001));
    }

    #[test]
    fn share_style_decides_audio_and_capture_mode() {
        assert_eq!(ShareStyle::from_raw(0), None);
        assert_eq!(ShareStyle::from_raw(4), None);
        let win = ShareStyle::from_raw(1).unwrap();
        let disp = ShareStyle::from_raw(2).unwrap();
        let app = ShareStyle::from_raw(3).unwrap();
        assert_eq!(win, ShareStyle::Window);
        assert_eq!(disp, ShareStyle::Display);
        assert_eq!(app, ShareStyle::Application);
        for s in [win, app] {
            assert_eq!(s.audio_scope(), AudioScope::ThisApp);
            assert_eq!(s.audio_scope().label(), "this app's audio");
            assert_eq!(s.capture_mode(), "app");
        }
        assert_eq!(disp.audio_scope(), AudioScope::WholeSystem);
        assert_eq!(disp.audio_scope().label(), "whole-system audio");
        assert_eq!(disp.capture_mode(), "screen");
    }

    #[test]
    fn queue_depth_leaves_sck_two_surfaces_of_slack() {
        assert_eq!(QUEUE_DEPTH, ENCODER_MAX_IN_FLIGHT + 2);
    }

    #[test]
    fn a_panicking_callback_is_reported_once_and_fenced_off() {
        let reports = Arc::new(Mutex::new(Vec::<String>::new()));
        let guard = {
            let reports = reports.clone();
            CallbackGuard::new(move |m| reports.lock().unwrap().push(m))
        };
        assert_eq!(guard.run(|| 1), Some(1));
        let prev = std::panic::take_hook();
        std::panic::set_hook(Box::new(|_| {})); // keep the test output clean
        let r: Option<()> = guard.run(|| panic!("injected on a fake output"));
        std::panic::set_hook(prev);
        assert_eq!(r, None);
        assert!(guard.failed());
        // Later frames are not handed to a capture that already panicked.
        let mut ran = false;
        assert_eq!(guard.run(|| ran = true), None);
        assert!(!ran);
        let reports = reports.lock().unwrap();
        assert_eq!(reports.len(), 1);
        assert!(
            reports[0].contains("injected on a fake output"),
            "{reports:?}"
        );
    }

    #[test]
    fn cm_time_converts_to_100ns_ticks() {
        // The host clock's usual shape: nanoseconds, timescale 1e9.
        assert_eq!(
            cm_time_to_100ns(1_500_000_000, 1_000_000_000, true),
            Some(15_000_000)
        );
        // A week of uptime in ns does not overflow on the way.
        let week_ns = 7 * 24 * 3600 * 1_000_000_000i64;
        assert_eq!(
            cm_time_to_100ns(week_ns, 1_000_000_000, true),
            Some(week_ns / 100)
        );
        // Other timescales (600 is common in CoreMedia).
        assert_eq!(cm_time_to_100ns(600, 600, true), Some(10_000_000));
        assert_eq!(cm_time_to_100ns(1, 1, false), None);
        assert_eq!(cm_time_to_100ns(1, 0, true), None);
    }

    #[test]
    fn bt709_video_range_endpoints() {
        assert_eq!(bt709_video_to_rgb(16, 128, 128), [0, 0, 0]);
        assert_eq!(bt709_video_to_rgb(235, 128, 128), [255, 255, 255]);
        // BT.709 pure red, video range: Y 63, Cb 102, Cr 240.
        let [r, g, b] = bt709_video_to_rgb(63, 102, 240);
        assert!(r >= 250 && g <= 5 && b <= 5, "{r} {g} {b}");
    }

    #[test]
    fn thumbnail_fits_the_box_and_samples_both_planes() {
        // 8×4 frame: left half black, right half white; neutral chroma
        // except the top-right chroma sample, which is red.
        let (w, h) = (8u32, 4u32);
        let mut y = vec![0u8; 8 * 4];
        for row in 0..4 {
            for col in 0..8 {
                y[row * 8 + col] = if col < 4 { 16 } else { 235 };
            }
        }
        let mut uv = vec![128u8; 8 * 2];
        uv[6] = 102; // chroma (col 6..8, rows 0..2)
        uv[7] = 240;
        let (tw, th, rgba) = nv12_thumbnail(
            &Plane {
                data: &y,
                stride: 8,
            },
            &Plane {
                data: &uv,
                stride: 8,
            },
            w,
            h,
            4,
            4,
        );
        assert_eq!((tw, th), (4, 2), "aspect kept inside the box");
        assert_eq!(rgba.len(), (tw * th * 4) as usize);
        let px = |x: u32, yy: u32| {
            let i = ((yy * tw + x) * 4) as usize;
            [rgba[i], rgba[i + 1], rgba[i + 2], rgba[i + 3]]
        };
        assert_eq!(px(0, 0), [0, 0, 0, 255]);
        assert_eq!(px(2, 1), [255, 255, 255, 255]);
        let [r, g, _, a] = px(3, 0);
        assert!(r > g && a == 255, "top-right carries the red chroma");
    }

    #[test]
    fn thumbnail_tolerates_short_planes() {
        // A buffer shorter than its claimed size must not panic inside an
        // Objective-C callback; missing samples read as black.
        let (tw, th, rgba) = nv12_thumbnail(
            &Plane {
                data: &[],
                stride: 0,
            },
            &Plane {
                data: &[],
                stride: 0,
            },
            64,
            36,
            32,
            18,
        );
        assert_eq!((tw, th), (32, 18));
        assert!(rgba.chunks(4).all(|p| p == [0, 0, 0, 255]));
    }
}
