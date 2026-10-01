//! Ready's source preview on Windows (docs/65 D3): a WGC session on the
//! chosen window or display with no encoder behind it, sampled once a
//! second through the same VideoProcessor downscale the live thumbnail uses
//! (`Converter::thumbnail_rgba`). Dropping it closes the session.

use gawk_capture::d3d::{Converter, GpuDevice};
use gawk_capture::wgc::{self, CaptureTarget};
use gawk_ui::preview::PreviewSource;
use gawk_ui::shell::Thumb;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

const EVERY: Duration = Duration::from_secs(1);
const WIDTH: u32 = 320;

pub struct Preview {
    thumb: Arc<Mutex<Option<Thumb>>>,
    _capture: wgc::Capture,
    _gpu: GpuDevice,
}

impl Preview {
    pub fn start(target: CaptureTarget) -> Result<Self, String> {
        let gpu = GpuDevice::hardware().map_err(|e| format!("no graphics device: {e}"))?;
        let item = wgc::create_item(target).map_err(|e| format!("capture target: {e}"))?;
        let thumb: Arc<Mutex<Option<Thumb>>> = Arc::default();
        let capture = {
            let gpu = gpu.clone();
            let thumb = thumb.clone();
            let mut converter: Option<Converter> = None;
            let mut last: Option<Instant> = None;
            wgc::Capture::start(&gpu.clone(), item, move |frame| {
                // A broken frame costs the preview a picture, never the app.
                let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    if frame.width == 0
                        || frame.height == 0
                        || last.is_some_and(|t| t.elapsed() < EVERY)
                    {
                        return;
                    }
                    if !converter
                        .as_ref()
                        .is_some_and(|c| c.matches_input(frame.width, frame.height))
                    {
                        let (w, h) =
                            gawk_capture::fit::fit_within(frame.width, frame.height, WIDTH, WIDTH);
                        converter = Converter::new(&gpu, frame.width, frame.height, w, h).ok();
                    }
                    if let Some(t) = converter
                        .as_ref()
                        .and_then(|c| c.thumbnail_rgba(&frame.texture, WIDTH).ok())
                    {
                        last = Some(Instant::now());
                        *thumb.lock().unwrap() = Some(t);
                    }
                }));
            })
            .map_err(|e| format!("capture start: {e}"))?
        };
        Ok(Self {
            thumb,
            _capture: capture,
            _gpu: gpu,
        })
    }
}

impl PreviewSource for Preview {
    fn take(&self) -> Option<Thumb> {
        self.thumb.lock().unwrap().take()
    }
}
