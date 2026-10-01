//! Ready's source preview on macOS (docs/65 D3): an `SCStream` on the
//! picker's filter with no encoder and no audio, which ScreenCaptureKit
//! scales to the thumbnail box at 1 fps, converted by the live thumbnail's
//! `nv12_thumbnail`. Dropping it stops the stream. While one runs, the
//! system shows its screen-sharing indicator (OD1).

use gawk_capture::fit::fit_within;
use gawk_capture::sck::{Capture, Frame, StreamSettings};
use gawk_capture::sck_picker::Picked;
use gawk_capture::sck_policy::nv12_thumbnail;
use gawk_ui::preview::PreviewSource;
use gawk_ui::shell::Thumb;
use std::sync::{Arc, Mutex};

const WIDTH: u32 = 320;
const HEIGHT: u32 = 180;

pub struct Preview {
    thumb: Arc<Mutex<Option<Thumb>>>,
    _capture: Capture,
}

impl Preview {
    pub fn start(picked: &Picked) -> Result<Self, String> {
        let (width, height) = fit_within(picked.width, picked.height, WIDTH, HEIGHT);
        let thumb: Arc<Mutex<Option<Thumb>>> = Arc::default();
        let on_frame = {
            let thumb = thumb.clone();
            move |frame: &Frame<'_>| {
                if let Some(t) =
                    frame.with_planes(|y, uv, w, h| nv12_thumbnail(y, uv, w, h, WIDTH, HEIGHT))
                {
                    *thumb.lock().unwrap() = Some(t);
                }
            }
        };
        let capture = Capture::start(
            picked,
            StreamSettings {
                width,
                height,
                fps: 1,
            },
            on_frame,
            None,
            |e| log::info!("preview capture: {e}"),
        )?;
        Ok(Self {
            thumb,
            _capture: capture,
        })
    }
}

impl PreviewSource for Preview {
    fn take(&self) -> Option<Thumb> {
        self.thumb.lock().unwrap().take()
    }
}
