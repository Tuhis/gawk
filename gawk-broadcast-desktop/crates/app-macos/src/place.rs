//! Where the window and the screen's visible frame are, for window fit (R64,
//! docs/66 D15). AppKit measures in points, Slint's logical px, from the
//! primary screen's bottom-left corner; `gawk_ui::fit` flips and decides.

use gawk_ui::MainWindow;
use gawk_ui::fit::{self, Placement, Rect};
use objc2::MainThreadMarker;
use objc2_app_kit::{NSScreen, NSView, NSWindowStyleMask};
use objc2_foundation::NSRect;
use raw_window_handle::{HasWindowHandle, RawWindowHandle};
use slint::ComponentHandle;

fn rect(r: NSRect) -> Rect {
    Rect::new(
        r.origin.x as f32,
        r.origin.y as f32,
        r.size.width as f32,
        r.size.height as f32,
    )
}

pub fn placement(ui: &MainWindow) -> Option<Placement> {
    let mtm = MainThreadMarker::new()?;
    let handle = ui.window().window_handle();
    let RawWindowHandle::AppKit(h) = handle.window_handle().ok()?.as_raw() else {
        return None;
    };
    // SAFETY: the handle is this window's content view, alive as long as the
    // window, and this runs on the main thread (checked above).
    let view: &NSView = unsafe { h.ns_view.cast::<NSView>().as_ref() };
    let window = view.window()?;
    let screen = window.screen()?;
    // AppKit's origin is the bottom-left of the primary screen, the first.
    let primary_h = NSScreen::screens(mtm).firstObject()?.frame().size.height as f32;
    let arranged = window.isZoomed()
        || window.isMiniaturized()
        || window.styleMask().contains(NSWindowStyleMask::FullScreen);
    Some(Placement {
        frame: fit::from_appkit(rect(window.frame()), primary_h),
        client_h: ui
            .window()
            .size()
            .to_logical(ui.window().scale_factor())
            .height,
        work: fit::from_appkit(rect(screen.visibleFrame()), primary_h),
        positioned: true,
        arranged,
    })
}
