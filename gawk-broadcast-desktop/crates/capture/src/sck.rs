//! One `SCStream` from the picker's filter (docs/54 D4): `420v` at the
//! fitted size, host-clock timestamps, damage-driven delivery, cursor in.
//!
//! One of the three modules `unsafe` is confined to (D3). Every Objective-C
//! entry point — the output and delegate methods and the completion
//! handlers — runs behind a [`CallbackGuard`], because a panic unwinding
//! into an Objective-C frame aborts the process.
//!
//! **Ownership (D4/D10).** A [`Frame`] is lent to the frame callback for
//! the duration of that call only. SCK's surface pool stalls if the app
//! holds more than `queueDepth − 1` buffers; lending rather than handing
//! over means our side holds at most the one being delivered, and the
//! encoder (MB3) keeps what it needs by VideoToolbox retaining the pixel
//! buffer inside `VTCompressionSessionEncodeFrame`, never by us.

use crate::host;
use crate::sck_picker::Picked;
use crate::sck_policy::{CallbackGuard, FrameStatus, Plane, QUEUE_DEPTH};
use block2::RcBlock;
use dispatch2::{DispatchQueue, DispatchRetained};
use objc2::rc::Retained;
use objc2::runtime::{NSObject, NSObjectProtocol, ProtocolObject};
use objc2::{AllocAnyThread, DefinedClass, define_class, msg_send};
use objc2_core_foundation::CFDictionary;
use objc2_core_media::{CMSampleBuffer, CMTime};
use objc2_core_video::{
    CVPixelBuffer, CVPixelBufferGetBaseAddressOfPlane, CVPixelBufferGetBytesPerRowOfPlane,
    CVPixelBufferGetHeight, CVPixelBufferGetHeightOfPlane, CVPixelBufferGetPixelFormatType,
    CVPixelBufferGetPlaneCount, CVPixelBufferGetWidth, CVPixelBufferLockBaseAddress,
    CVPixelBufferLockFlags, CVPixelBufferUnlockBaseAddress,
    kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange,
};
use objc2_foundation::{NSError, NSNumber, NSString};
use objc2_screen_capture_kit::{
    SCStream, SCStreamConfiguration, SCStreamDelegate, SCStreamFrameInfoStatus, SCStreamOutput,
    SCStreamOutputType,
};
use std::ffi::c_void;
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::time::Duration;

/// `420v`: the format VideoToolbox consumes natively, so there is no
/// conversion stage (D4).
pub const PIXEL_FORMAT: u32 = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange;

/// What the stream is asked for.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct StreamSettings {
    /// The fitted size (D9): SCK scales in the compositor; the app never
    /// resamples.
    pub width: u32,
    pub height: u32,
    pub fps: u32,
}

/// One delivered frame, lent for the duration of the frame callback.
pub struct Frame<'a> {
    pub status: FrameStatus,
    /// The presentation time on the host clock, 100 ns ticks — map it with
    /// [`host::mapper`]. `None` if the buffer carried no valid time.
    pub pts_100ns: Option<i64>,
    image: Option<&'a CVPixelBuffer>,
}

impl Frame<'_> {
    /// The pixel buffer's size; `None` for status-only frames.
    pub fn size(&self) -> Option<(u32, u32)> {
        let pb = self.image?;
        Some((
            CVPixelBufferGetWidth(pb) as u32,
            CVPixelBufferGetHeight(pb) as u32,
        ))
    }

    pub fn pixel_format(&self) -> Option<u32> {
        self.image.map(|pb| CVPixelBufferGetPixelFormatType(pb))
    }

    /// The IOSurface-backed buffer itself, for VideoToolbox (MB3).
    pub fn pixel_buffer(&self) -> Option<&CVPixelBuffer> {
        self.image
    }

    /// Runs `f` over the luma and chroma planes of a `420v` frame, locked
    /// read-only for the call. `None` for status-only or non-`420v` frames
    /// and when the lock fails.
    pub fn with_planes<R>(
        &self,
        f: impl FnOnce(&Plane<'_>, &Plane<'_>, u32, u32) -> R,
    ) -> Option<R> {
        let pb = self.image?;
        if CVPixelBufferGetPixelFormatType(pb) != PIXEL_FORMAT || CVPixelBufferGetPlaneCount(pb) < 2
        {
            return None;
        }
        // SAFETY: lock/unlock bracket every read of the base addresses; the
        // slices are built from the plane's own row stride × height and do
        // not outlive the lock.
        unsafe {
            if CVPixelBufferLockBaseAddress(pb, CVPixelBufferLockFlags::ReadOnly) != 0 {
                return None;
            }
            let plane = |i: usize| {
                let base = CVPixelBufferGetBaseAddressOfPlane(pb, i) as *const u8;
                let stride = CVPixelBufferGetBytesPerRowOfPlane(pb, i);
                let rows = CVPixelBufferGetHeightOfPlane(pb, i);
                let data = if base.is_null() {
                    &[][..]
                } else {
                    std::slice::from_raw_parts(base, stride * rows)
                };
                Plane { data, stride }
            };
            let (y, uv) = (plane(0), plane(1));
            let r = f(
                &y,
                &uv,
                CVPixelBufferGetWidth(pb) as u32,
                CVPixelBufferGetHeight(pb) as u32,
            );
            CVPixelBufferUnlockBaseAddress(pb, CVPixelBufferLockFlags::ReadOnly);
            Some(r)
        }
    }
}

type OnFrame = dyn FnMut(&Frame<'_>) + Send;
type OnError = dyn Fn(String) + Send + Sync;

struct OutputIvars {
    on_frame: Mutex<Box<OnFrame>>,
    on_error: Arc<OnError>,
    guard: Arc<CallbackGuard>,
}

define_class!(
    // SAFETY: NSObject has no subclassing requirements and this class does
    // not implement Drop.
    #[unsafe(super(NSObject))]
    #[name = "GawkScreenCaptureOutput"]
    #[ivars = OutputIvars]
    struct Output;

    unsafe impl NSObjectProtocol for Output {}

    unsafe impl SCStreamOutput for Output {
        #[unsafe(method(stream:didOutputSampleBuffer:ofType:))]
        fn did_output(
            &self,
            _stream: &SCStream,
            sample: &CMSampleBuffer,
            kind: SCStreamOutputType,
        ) {
            if kind != SCStreamOutputType::Screen {
                return; // audio is MB4's
            }
            let iv = self.ivars();
            iv.guard.run(|| {
                // SAFETY: the sample buffer is valid for this callback.
                let image = unsafe { sample.image_buffer() };
                let frame = Frame {
                    status: unsafe { frame_status(sample) },
                    pts_100ns: host::to_100ns(unsafe { sample.presentation_time_stamp() }),
                    image: image.as_deref(),
                };
                // The output queue is serial, so this lock is never
                // contended; it only makes the FnMut shareable.
                (iv.on_frame.lock().unwrap())(&frame);
            });
        }
    }

    unsafe impl SCStreamDelegate for Output {
        #[unsafe(method(stream:didStopWithError:))]
        fn did_stop(&self, _stream: &SCStream, error: &NSError) {
            let iv = self.ivars();
            let text = format!("Capture stopped: {}", error.localizedDescription());
            iv.guard.run(|| (iv.on_error)(text));
        }
    }
);

/// `SCStreamFrameInfoStatus` from the sample's first attachment
/// dictionary; a frame without one is reported as unknown (never encoded).
unsafe fn frame_status(sample: &CMSampleBuffer) -> FrameStatus {
    // SAFETY: CF getters on a live sample buffer; the attachment array and
    // dictionary are owned by the buffer and read before it is released.
    unsafe {
        let Some(arr) = sample.sample_attachments_array(false) else {
            return FrameStatus::Unknown(-1);
        };
        if arr.count() < 1 {
            return FrameStatus::Unknown(-1);
        }
        let Some(dict) = (arr.value_at_index(0) as *const CFDictionary).as_ref() else {
            return FrameStatus::Unknown(-1);
        };
        let key: &NSString = SCStreamFrameInfoStatus;
        let value = dict.value(key as *const NSString as *const c_void);
        match (value as *const NSNumber).as_ref() {
            Some(n) => FrameStatus::from_raw(n.integerValue() as i64),
            None => FrameStatus::Unknown(-1),
        }
    }
}

fn configuration(s: StreamSettings) -> Retained<SCStreamConfiguration> {
    // SAFETY: setters on a fresh configuration object.
    unsafe {
        let c = SCStreamConfiguration::new();
        c.setWidth(s.width as usize);
        c.setHeight(s.height as usize);
        c.setMinimumFrameInterval(CMTime::new(1, s.fps.max(1) as i32));
        c.setPixelFormat(PIXEL_FORMAT);
        c.setColorSpaceName(objc2_core_graphics::kCGColorSpaceITUR_709);
        // The fitted size keeps the source aspect; scale into it, never
        // stretch.
        c.setScalesToFit(true);
        c.setPreservesAspectRatio(true);
        c.setShowsCursor(true); // R14 invariant
        c.setQueueDepth(QUEUE_DEPTH as isize);
        c.setCapturesAudio(false); // MB4
        c
    }
}

/// A running capture. Dropping it stops the stream and waits (bounded) for
/// the stop to land, so a capture never outlives the broadcast — the
/// `finish()` incident class (docs/54 §6 "Stopping").
pub struct Capture {
    stream: Retained<SCStream>,
    output: Retained<Output>,
    _queue: DispatchRetained<DispatchQueue>,
    on_error: Arc<OnError>,
    stopped: bool,
}

impl Capture {
    /// Starts capturing `picked`. `on_frame` runs on the capture's own
    /// serial queue and must not block; `on_error` may run on any thread
    /// (a start failure, the stream stopping, a callback panic) and is
    /// called at most once per cause.
    pub fn start(
        picked: &Picked,
        settings: StreamSettings,
        on_frame: impl FnMut(&Frame<'_>) + Send + 'static,
        on_error: impl Fn(String) + Send + Sync + 'static,
    ) -> Result<Self, String> {
        let on_error: Arc<OnError> = Arc::new(on_error);
        let guard = Arc::new(CallbackGuard::new({
            let on_error = on_error.clone();
            move |msg| on_error(msg)
        }));
        let output = Output::alloc().set_ivars(OutputIvars {
            on_frame: Mutex::new(Box::new(on_frame)),
            on_error: on_error.clone(),
            guard,
        });
        // SAFETY: NSObject's designated initializer on a fresh instance.
        let output: Retained<Output> = unsafe { msg_send![super(output), init] };
        let queue = DispatchQueue::new("fi.ioio.gawk.capture.screen", None);
        // SAFETY: the filter, configuration, output and queue are all alive
        // for the calls; the stream retains what it keeps.
        unsafe {
            let stream = SCStream::initWithFilter_configuration_delegate(
                SCStream::alloc(),
                &picked.filter,
                &configuration(settings),
                Some(ProtocolObject::from_ref(&*output)),
            );
            stream
                .addStreamOutput_type_sampleHandlerQueue_error(
                    ProtocolObject::from_ref(&*output),
                    SCStreamOutputType::Screen,
                    Some(&queue),
                )
                .map_err(|e| {
                    format!(
                        "Could not attach to the capture: {}",
                        e.localizedDescription()
                    )
                })?;
            let started = completion(on_error.clone(), "Capture could not start");
            stream.startCaptureWithCompletionHandler(Some(&started));
            Ok(Self {
                stream,
                output,
                _queue: queue,
                on_error,
                stopped: false,
            })
        }
    }

    /// Points the live stream at newly picked content (D4: re-pick without
    /// stopping) at `settings`, which the caller re-fits to the new source.
    pub fn update(&self, picked: &Picked, settings: StreamSettings) {
        // SAFETY: as in `start`.
        unsafe {
            let done = completion(self.on_error.clone(), "Could not switch the shared content");
            self.stream
                .updateContentFilter_completionHandler(&picked.filter, Some(&done));
            let done = completion(self.on_error.clone(), "Could not resize the capture");
            self.stream
                .updateConfiguration_completionHandler(&configuration(settings), Some(&done));
        }
    }

    pub(crate) fn stream(&self) -> &SCStream {
        &self.stream
    }

    /// Stops the stream and waits up to two seconds for SCK to confirm.
    pub fn stop(mut self) {
        self.stop_blocking();
    }

    fn stop_blocking(&mut self) {
        if std::mem::replace(&mut self.stopped, true) {
            return;
        }
        let (tx, rx) = mpsc::channel::<()>();
        let tx = Mutex::new(Some(tx));
        let done = RcBlock::new(move |_err: *mut NSError| {
            if let Some(tx) = tx.lock().ok().and_then(|mut t| t.take()) {
                let _ = tx.send(());
            }
        });
        // SAFETY: the stream is alive; the block owns its captures.
        unsafe {
            self.stream.stopCaptureWithCompletionHandler(Some(&done));
            let _ = self.stream.removeStreamOutput_type_error(
                ProtocolObject::from_ref(&*self.output),
                SCStreamOutputType::Screen,
            );
        }
        if rx.recv_timeout(Duration::from_secs(2)).is_err() {
            log::warn!("capture stop not confirmed within 2 s");
        }
    }
}

impl Drop for Capture {
    fn drop(&mut self) {
        self.stop_blocking();
    }
}

/// A completion handler that reports a non-nil error through `on_error`.
fn completion(on_error: Arc<OnError>, what: &'static str) -> RcBlock<dyn Fn(*mut NSError)> {
    RcBlock::new(move |err: *mut NSError| {
        // SAFETY: a non-null error pointer from the framework is a live
        // NSError for the duration of the handler.
        if let Some(err) = unsafe { err.as_ref() } {
            let text = format!("{what}: {}", err.localizedDescription());
            // Not behind the guard: this closure cannot panic short of an
            // allocation failure, and a guard here would need its own
            // failure channel for nothing.
            on_error(text);
        }
    })
}
