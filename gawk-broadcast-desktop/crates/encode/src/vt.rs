//! VideoToolbox low-latency H.264 (R52 MB3, docs/54 D7/D8).
//!
//! One of the three modules `unsafe` is confined to (D3). The session is
//! created with `EnableLowLatencyRateControl` + `RequireHardwareAccelerated`
//! — the low-latency mode *is* hardware-only, one-in-one-out, no B-frames —
//! and trial-gated through the shared cascade before a broadcast may use it
//! (enumeration is not acceptance). Output is AVCC with the parameter sets
//! only in the format description; the output callback rewrites every AU to
//! Annex-B and prepends SPS/PPS to every IDR (D8), so what leaves this
//! module is exactly what the Windows path hands the engine.

use crate::cascade::{self, Candidate, TrialAu, TrialRun, TrialRunner};
use crate::h264;
use crate::vt_policy::{self, KeyframeCadence};
use objc2_core_foundation::{
    CFArray, CFBoolean, CFDictionary, CFNumber, CFRetained, CFString, CFType,
};
use objc2_core_media::{
    CMFormatDescription, CMSampleBuffer, CMTime, CMTimeFlags,
    CMVideoFormatDescriptionGetH264ParameterSetAtIndex, kCMSampleAttachmentKey_NotSync,
    kCMTimeInvalid, kCMVideoCodecType_H264,
};
use objc2_core_video::{
    CVPixelBuffer, CVPixelBufferCreate, CVPixelBufferGetBaseAddressOfPlane,
    CVPixelBufferGetBytesPerRowOfPlane, CVPixelBufferGetHeightOfPlane,
    CVPixelBufferLockBaseAddress, CVPixelBufferLockFlags, CVPixelBufferUnlockBaseAddress,
    kCVPixelBufferIOSurfacePropertiesKey, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
};
use objc2_video_toolbox::{
    VTCompressionSession, VTEncodeInfoFlags, VTSessionSetProperty,
    kVTCompressionPropertyKey_AllowFrameReordering, kVTCompressionPropertyKey_AverageBitRate,
    kVTCompressionPropertyKey_DataRateLimits, kVTCompressionPropertyKey_ExpectedFrameRate,
    kVTCompressionPropertyKey_MaxKeyFrameInterval,
    kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, kVTCompressionPropertyKey_ProfileLevel,
    kVTCompressionPropertyKey_RealTime, kVTEncodeFrameOptionKey_ForceKeyFrame,
    kVTProfileLevel_H264_High_AutoLevel, kVTVideoEncoderSpecification_EnableLowLatencyRateControl,
    kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder,
};
use std::ffi::{c_int, c_void};
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::ptr::NonNull;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

/// The one hardware H.264 encoder on Apple Silicon, as the cascade and the
/// last-good cache name it (D7: there is no vendor cascade here).
pub const ENCODER_ID: &str = "Apple H.264";

/// Timestamps cross the session in 100 ns ticks (the cascade's unit).
const TIMESCALE: i32 = 10_000_000;

/// What the session is built for.
#[derive(Debug, Clone, Copy)]
pub struct EncoderParams {
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    pub peak_bitrate_bps: u32,
}

/// One encoded access unit: Annex-B, parameter sets in-band on IDRs.
#[derive(Debug, Clone)]
pub struct EncodedAu {
    pub data: Vec<u8>,
    /// The presentation time the frame was submitted with, 100 ns ticks.
    pub time_100ns: i64,
    pub keyframe: bool,
}

type OnAu = dyn FnMut(EncodedAu) + Send;
type OnError = dyn Fn(String) + Send + Sync;

/// What the output callback reaches through its refcon.
struct Ctx {
    on_au: Mutex<Box<OnAu>>,
    on_error: Box<OnError>,
    in_flight: AtomicUsize,
    failed: AtomicBool,
    /// The Annex-B SPS/PPS of the last format description, for the trial's
    /// `sequence_header`.
    param_sets: Mutex<Vec<u8>>,
}

impl Ctx {
    fn fail(&self, text: String) {
        if !self.failed.swap(true, Ordering::AcqRel) {
            (self.on_error)(text);
        }
    }
}

/// A live compression session.
pub struct Encoder {
    session: CFRetained<VTCompressionSession>,
    ctx: *const Ctx,
    cadence: Mutex<KeyframeCadence>,
    force: AtomicBool,
    finished: bool,
}

// SAFETY: VTCompressionSession is thread-safe for EncodeFrame from one
// thread at a time (the capture queue, serial) while outputs arrive on
// VideoToolbox's own thread; `ctx` is only read through `&Ctx`, whose
// fields are all synchronised.
unsafe impl Send for Encoder {}
unsafe impl Sync for Encoder {}

impl Encoder {
    /// Creates and configures the session (D7's table). `on_au` runs on
    /// VideoToolbox's output thread and must not block; `on_error` reports
    /// a failed encode once.
    pub fn new(
        params: EncoderParams,
        on_au: impl FnMut(EncodedAu) + Send + 'static,
        on_error: impl Fn(String) + Send + Sync + 'static,
    ) -> Result<Self, String> {
        let ctx = Box::into_raw(Box::new(Ctx {
            on_au: Mutex::new(Box::new(on_au)),
            on_error: Box::new(on_error),
            in_flight: AtomicUsize::new(0),
            failed: AtomicBool::new(false),
            param_sets: Mutex::new(Vec::new()),
        }));
        match unsafe { create_session(params, ctx) } {
            Ok(session) => Ok(Self {
                session,
                ctx,
                cadence: Mutex::new(KeyframeCadence::new(params.fps)),
                force: AtomicBool::new(false),
                finished: false,
            }),
            Err(e) => {
                // SAFETY: no session exists to call back into it.
                drop(unsafe { Box::from_raw(ctx) });
                Err(e)
            }
        }
    }

    fn ctx(&self) -> &Ctx {
        // SAFETY: freed only in `finish`, after the session is invalidated.
        unsafe { &*self.ctx }
    }

    /// Frames submitted and not yet returned — the backpressure gate's
    /// count (D10).
    pub fn in_flight(&self) -> usize {
        self.ctx().in_flight.load(Ordering::Acquire)
    }

    /// The next submitted frame is an IDR (the resume re-prime).
    pub fn force_idr(&self) {
        self.force.store(true, Ordering::Release);
    }

    /// Submits one frame stamped `time_100ns` lasting `duration_100ns`.
    /// VideoToolbox retains the pixel buffer for the encode; the caller
    /// keeps nothing (D4/D10).
    pub fn encode(
        &self,
        pixels: &CVPixelBuffer,
        time_100ns: i64,
        duration_100ns: i64,
    ) -> Result<(), String> {
        let forced = self.force.swap(false, Ordering::AcqRel);
        let idr = self.cadence.lock().unwrap().next(forced);
        let props = idr.then(force_keyframe_props);
        self.ctx().in_flight.fetch_add(1, Ordering::AcqRel);
        // SAFETY: a live session, a live pixel buffer, a properties
        // dictionary alive for the call.
        let status = unsafe {
            self.session.encode_frame(
                pixels,
                CMTime::new(time_100ns, TIMESCALE),
                CMTime::new(duration_100ns.max(1), TIMESCALE),
                props.as_deref().map(|d| d.as_opaque()),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
            )
        };
        if status != 0 {
            self.ctx().in_flight.fetch_sub(1, Ordering::AcqRel);
            return Err(format!("VTCompressionSessionEncodeFrame failed ({status})"));
        }
        Ok(())
    }

    /// Emits every pending frame, then invalidates the session.
    pub fn finish(mut self) {
        self.finish_inner();
    }

    fn finish_inner(&mut self) {
        if std::mem::replace(&mut self.finished, true) {
            return;
        }
        // SAFETY: completing and invalidating a live session; after
        // invalidation no callback can reach `ctx`, so it is freed.
        unsafe {
            let _ = self.session.complete_frames(kCMTimeInvalid);
            self.session.invalidate();
            drop(Box::from_raw(self.ctx.cast_mut()));
        }
    }

    /// The parameter sets VideoToolbox has emitted so far, Annex-B.
    fn param_sets(&self) -> Vec<u8> {
        self.ctx().param_sets.lock().unwrap().clone()
    }
}

impl Drop for Encoder {
    fn drop(&mut self) {
        self.finish_inner();
    }
}

fn cf_bool(v: bool) -> &'static CFType {
    CFBoolean::new(v).as_ref()
}

fn force_keyframe_props() -> CFRetained<CFDictionary<CFString, CFType>> {
    // SAFETY: an immutable framework constant.
    let key = unsafe { kVTEncodeFrameOptionKey_ForceKeyFrame };
    CFDictionary::from_slices(&[key], &[cf_bool(true)])
}

/// D7's encoder specification and session properties.
unsafe fn create_session(
    p: EncoderParams,
    ctx: *const Ctx,
) -> Result<CFRetained<VTCompressionSession>, String> {
    // SAFETY (whole body): framework constants, and CF objects alive for
    // each call; the refcon outlives the session (see `Encoder`).
    unsafe {
        let spec = CFDictionary::<CFString, CFType>::from_slices(
            &[
                kVTVideoEncoderSpecification_EnableLowLatencyRateControl,
                kVTVideoEncoderSpecification_RequireHardwareAcceleratedVideoEncoder,
            ],
            &[cf_bool(true), cf_bool(true)],
        );
        let mut out: *mut VTCompressionSession = std::ptr::null_mut();
        let status = VTCompressionSession::create(
            None,
            p.width as i32,
            p.height as i32,
            kCMVideoCodecType_H264,
            Some(spec.as_opaque()),
            None,
            None,
            Some(output_callback),
            ctx.cast_mut().cast(),
            NonNull::from(&mut out),
        );
        let Some(session) = NonNull::new(out).filter(|_| status == 0) else {
            return Err(format!(
                "no hardware low-latency H.264 session (VTCompressionSessionCreate {status})"
            ));
        };
        let session = CFRetained::from_raw(session);

        let set = |key: &CFString, value: &CFType| VTSessionSetProperty(&session, key, Some(value));
        let (avg, peak_bytes, window) = vt_policy::rate_limits(p.peak_bitrate_bps);
        let gop = vt_policy::gop_frames(p.fps);
        // Required: a session that refuses any of these is not the one D7
        // specifies, and the trial rejects it.
        for (name, key, value) in [
            (
                "RealTime",
                kVTCompressionPropertyKey_RealTime,
                cf_bool(true),
            ),
            (
                "AllowFrameReordering",
                kVTCompressionPropertyKey_AllowFrameReordering,
                cf_bool(false),
            ),
            (
                "ProfileLevel",
                kVTCompressionPropertyKey_ProfileLevel,
                kVTProfileLevel_H264_High_AutoLevel.as_ref(),
            ),
            (
                "AverageBitRate",
                kVTCompressionPropertyKey_AverageBitRate,
                CFNumber::new_i32(avg as i32).as_ref(),
            ),
        ] {
            let st = set(key, value);
            if st != 0 {
                session.invalidate();
                return Err(format!("the encoder refused {name} ({st})"));
            }
        }
        // Advisory: the forced-IDR cadence does not depend on the GOP
        // properties (low-latency mode ignores them by documentation), and
        // the peak cap is V-6's to measure — a refusal is logged, not fatal.
        let peak = CFNumber::new_i32(peak_bytes as i32);
        let win = CFNumber::new_f64(window);
        let (peak, win): (&CFType, &CFType) = (peak.as_ref(), win.as_ref());
        let limits = CFArray::<CFType>::from_objects(&[peak, win]);
        for (name, key, value) in [
            (
                "DataRateLimits",
                kVTCompressionPropertyKey_DataRateLimits,
                limits.as_ref(),
            ),
            (
                "ExpectedFrameRate",
                kVTCompressionPropertyKey_ExpectedFrameRate,
                CFNumber::new_i32(p.fps as i32).as_ref(),
            ),
            (
                "MaxKeyFrameInterval",
                kVTCompressionPropertyKey_MaxKeyFrameInterval,
                CFNumber::new_i32(gop as i32).as_ref(),
            ),
            (
                "MaxKeyFrameIntervalDuration",
                kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration,
                CFNumber::new_f64(0.5).as_ref(),
            ),
        ] {
            let st = set(key, value);
            if st != 0 {
                log::warn!("VideoToolbox did not accept {name} ({st}); continuing");
            }
        }
        let _ = session.prepare_to_encode_frames();
        Ok(session)
    }
}

/// `VTCompressionOutputCallback`. Runs on VideoToolbox's thread; a panic
/// must not unwind into it (D3), so the body is fenced.
unsafe extern "C-unwind" fn output_callback(
    refcon: *mut c_void,
    _source_frame: *mut c_void,
    status: i32,
    flags: VTEncodeInfoFlags,
    sample: *mut CMSampleBuffer,
) {
    // SAFETY: the refcon is the `Ctx` the session was created with, alive
    // until after invalidation.
    let Some(ctx) = (unsafe { (refcon as *const Ctx).as_ref() }) else {
        return;
    };
    ctx.in_flight.fetch_sub(1, Ordering::AcqRel);
    let result = catch_unwind(AssertUnwindSafe(|| {
        if status != 0 {
            ctx.fail(format!("a frame failed to encode ({status})"));
            return;
        }
        if flags.contains(VTEncodeInfoFlags::FrameDropped) {
            return; // the encoder's own drop: nothing to send
        }
        // SAFETY: a non-null sample buffer is valid for the callback.
        let Some(sample) = (unsafe { sample.as_ref() }) else {
            return;
        };
        match unsafe { annex_b_au(sample, ctx) } {
            Ok(au) => (ctx.on_au.lock().unwrap())(au),
            Err(e) => ctx.fail(e),
        }
    }));
    if result.is_err() {
        ctx.fail("the encoder output callback panicked".into());
    }
}

/// One output sample as an Annex-B AU (D8).
unsafe fn annex_b_au(sample: &CMSampleBuffer, ctx: &Ctx) -> Result<EncodedAu, String> {
    // SAFETY (whole body): read-only CoreMedia getters on a live sample.
    unsafe {
        let block = sample.data_buffer().ok_or("encoded sample has no data")?;
        let len = block.data_length();
        let mut avcc = vec![0u8; len];
        if len > 0 {
            let st =
                block.copy_data_bytes(0, len, NonNull::new_unchecked(avcc.as_mut_ptr().cast()));
            if st != 0 {
                return Err(format!("could not read the encoded sample ({st})"));
            }
        }
        let keyframe = is_sync(sample);
        let desc = sample
            .format_description()
            .ok_or("encoded sample has no format description")?;
        let (sets, nal_len) = parameter_sets(&desc)?;
        let mut data = h264::avcc_to_annex_b(&avcc, nal_len)
            .ok_or("VideoToolbox emitted a malformed AVCC access unit")?;
        let sets = h264::annex_b_from_nals(sets.iter().map(Vec::as_slice));
        if keyframe {
            // Load-bearing: extradata is empty on the Annex-B path (D8).
            data = cascade::ensure_idr_headers(data, true, &sets);
        }
        *ctx.param_sets.lock().unwrap() = sets;
        let pts = sample.presentation_time_stamp();
        let time_100ns = vt_policy::cm_time_to_100ns(
            pts.value,
            pts.timescale,
            pts.flags.contains(CMTimeFlags::Valid),
        )
        .ok_or("encoded sample has no presentation time")?;
        Ok(EncodedAu {
            data,
            time_100ns,
            keyframe,
        })
    }
}

/// A sample is a sync (IDR) sample unless its attachments say `NotSync`.
unsafe fn is_sync(sample: &CMSampleBuffer) -> bool {
    // SAFETY: CF getters on a live sample.
    unsafe {
        let Some(arr) = sample.sample_attachments_array(false) else {
            return true;
        };
        if arr.count() < 1 {
            return true;
        }
        let Some(dict) = (arr.value_at_index(0) as *const CFDictionary).as_ref() else {
            return true;
        };
        let key: &CFString = kCMSampleAttachmentKey_NotSync;
        let v = dict.value((key as *const CFString).cast());
        match (v as *const CFBoolean).as_ref() {
            Some(b) => !b.as_bool(),
            None => true,
        }
    }
}

/// The H.264 parameter sets and NAL length size from a format description.
unsafe fn parameter_sets(desc: &CMFormatDescription) -> Result<(Vec<Vec<u8>>, usize), String> {
    // SAFETY: out-pointers to locals; the returned parameter-set pointers
    // are owned by the description, copied before it is released.
    unsafe {
        let mut count = 0usize;
        let mut nal_len: c_int = 0;
        let st = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
            desc,
            0,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            &mut count,
            &mut nal_len,
        );
        if st != 0 {
            return Err(format!(
                "no H.264 parameter sets in the format description ({st})"
            ));
        }
        let mut sets = Vec::with_capacity(count);
        for i in 0..count {
            let mut ptr: *const u8 = std::ptr::null();
            let mut size = 0usize;
            let st = CMVideoFormatDescriptionGetH264ParameterSetAtIndex(
                desc,
                i,
                &mut ptr,
                &mut size,
                std::ptr::null_mut(),
                std::ptr::null_mut(),
            );
            if st != 0 || ptr.is_null() {
                return Err(format!("parameter set {i} unreadable ({st})"));
            }
            sets.push(std::slice::from_raw_parts(ptr, size).to_vec());
        }
        Ok((sets, nal_len as usize))
    }
}

/// The D7 trial: a session built exactly as the live one, fed synthetic
/// IOSurface-backed `420v` buffers the trial allocates itself (never the
/// capture stream's), checked row by row by the shared
/// `cascade::validate_trial` plus the forced-cadence check.
pub struct VtTrialRunner {
    pub params: EncoderParams,
}

impl TrialRunner for VtTrialRunner {
    fn run(&mut self, candidate: &Candidate) -> Result<TrialRun, String> {
        if candidate.id != ENCODER_ID {
            return Err(format!("unknown candidate {}", candidate.id));
        }
        let (frames, forced_at) = vt_policy::trial_plan(self.params.fps);
        let aus: Arc<Mutex<Vec<TrialAu>>> = Arc::default();
        let error: Arc<Mutex<Option<String>>> = Arc::default();
        let enc = {
            let aus = aus.clone();
            let error = error.clone();
            Encoder::new(
                self.params,
                move |au| {
                    aus.lock().unwrap().push(TrialAu {
                        data: au.data,
                        time_100ns: au.time_100ns,
                    })
                },
                move |e| {
                    error.lock().unwrap().get_or_insert(e);
                },
            )?
        };
        let frame_100ns = 10_000_000 / i64::from(self.params.fps.max(1));
        let mut input_times = Vec::with_capacity(frames);
        for i in 0..frames {
            let pb = synthetic_frame(self.params.width, self.params.height, i as u8)?;
            if i == forced_at {
                enc.force_idr();
            }
            let t = i as i64 * frame_100ns;
            input_times.push(t);
            enc.encode(&pb, t, frame_100ns)?;
        }
        let sequence_header = enc.param_sets();
        enc.finish(); // CompleteFrames: every pending AU is emitted first
        if let Some(e) = error.lock().unwrap().take() {
            return Err(e);
        }
        let run = TrialRun {
            inputs_fed: frames,
            aus: std::mem::take(&mut *aus.lock().unwrap()),
            input_times_100ns: input_times,
            forced_idr_at: Some(forced_at),
            sequence_header,
        };
        let idrs: Vec<bool> = run.aus.iter().map(|a| h264::has_idr(&a.data)).collect();
        vt_policy::check_cadence(&idrs, self.params.fps)?;
        Ok(run)
    }
}

/// The candidates: the one Apple Silicon encoder (D7).
pub fn candidates() -> Vec<Candidate> {
    vec![Candidate {
        id: ENCODER_ID.into(),
    }]
}

/// A `420v` IOSurface-backed frame with a moving luma ramp, so the encoder
/// has real (changing) content to code.
pub fn synthetic_frame(
    width: u32,
    height: u32,
    phase: u8,
) -> Result<CFRetained<CVPixelBuffer>, String> {
    // SAFETY: CoreVideo creation with an attributes dictionary alive for
    // the call; the planes are written between lock and unlock.
    unsafe {
        let io = CFDictionary::<CFString, CFType>::empty();
        let attrs = CFDictionary::<CFString, CFType>::from_slices(
            &[kCVPixelBufferIOSurfacePropertiesKey],
            &[io.as_ref()],
        );
        let mut out: *mut CVPixelBuffer = std::ptr::null_mut();
        let st = CVPixelBufferCreate(
            None,
            width as usize,
            height as usize,
            kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
            Some(attrs.as_opaque()),
            NonNull::from(&mut out),
        );
        let Some(pb) = NonNull::new(out).filter(|_| st == 0) else {
            return Err(format!("trial frame allocation failed ({st})"));
        };
        let pb = CFRetained::from_raw(pb);
        if CVPixelBufferLockBaseAddress(&pb, CVPixelBufferLockFlags::empty()) != 0 {
            return Err("trial frame lock failed".into());
        }
        for plane in 0..2 {
            let base = CVPixelBufferGetBaseAddressOfPlane(&pb, plane) as *mut u8;
            let stride = CVPixelBufferGetBytesPerRowOfPlane(&pb, plane);
            let rows = CVPixelBufferGetHeightOfPlane(&pb, plane);
            if base.is_null() {
                continue;
            }
            let buf = std::slice::from_raw_parts_mut(base, stride * rows);
            for (r, row) in buf.chunks_mut(stride).enumerate() {
                for (c, px) in row.iter_mut().enumerate() {
                    *px = if plane == 0 {
                        16u8.wrapping_add(((r + c) as u8).wrapping_add(phase.wrapping_mul(4)) % 200)
                    } else {
                        128
                    };
                }
            }
        }
        CVPixelBufferUnlockBaseAddress(&pb, CVPixelBufferLockFlags::empty());
        Ok(pb)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The D7 trial against this machine's real encoder: property
    /// acceptance, no B-frames, VFR pass-through, SPS/PPS before every IDR,
    /// the forced cadence under low-latency mode (V-3), `420v` input (V-9).
    /// Ignored by default: a CI VM may have no hardware encoder, and that
    /// is the refusal path, not a failure. Run on a Mac with
    /// `cargo test -p gawk-encode vt::tests -- --ignored --nocapture`.
    #[test]
    #[ignore = "needs a hardware H.264 encoder"]
    fn the_trial_passes_on_this_mac() {
        let mut runner = VtTrialRunner {
            params: EncoderParams {
                width: 1920,
                height: 1080,
                fps: 60,
                peak_bitrate_bps: 12_000_000,
            },
        };
        let accepted = cascade::choose(&candidates(), None, &mut runner)
            .unwrap_or_else(|r| panic!("refused: {:?}", r.tried));
        println!(
            "accepted {} · {} · {} prepend bytes",
            accepted.id,
            accepted.codec_string,
            accepted.prepend_headers.len()
        );
        assert_eq!(accepted.id, ENCODER_ID);
        assert!(
            accepted.codec_string.starts_with("avc1.64"),
            "{}",
            accepted.codec_string
        );
    }
}
