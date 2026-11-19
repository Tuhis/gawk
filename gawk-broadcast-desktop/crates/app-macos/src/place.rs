//! Where the window and the screen's visible frame are, for window fit (R64,
//! docs/66 D15). AppKit measures in points, Slint's logical px, from the
//! primary screen's bottom-left corner; `gawk_ui::fit` flips and decides.
//! The same measurements remember where the window was (docs/66 D18).

use gawk_ui::MainWindow;
use gawk_ui::fit::{self, Placement, Rect};
use objc2::MainThreadMarker;
use objc2_app_kit::{NSScreen, NSView, NSWindow, NSWindowStyleMask};
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

/// The window's `NSWindow`, on the main thread.
fn ns_window(ui: &MainWindow) -> Option<objc2::rc::Retained<NSWindow>> {
    MainThreadMarker::new()?;
    let handle = ui.window().window_handle();
    let RawWindowHandle::AppKit(h) = handle.window_handle().ok()?.as_raw() else {
        return None;
    };
    // SAFETY: the handle is this window's content view, alive as long as the
    // window, and this runs on the main thread (checked above).
    let view: &NSView = unsafe { h.ns_view.cast::<NSView>().as_ref() };
    view.window()
}

/// AppKit's origin is the bottom-left of the primary screen, the first:
/// its height flips a rectangle to a top-left origin.
fn primary_h(mtm: MainThreadMarker) -> Option<f32> {
    Some(NSScreen::screens(mtm).firstObject()?.frame().size.height as f32)
}

fn arranged(window: &NSWindow) -> bool {
    window.isZoomed()
        || window.isMiniaturized()
        || window.styleMask().contains(NSWindowStyleMask::FullScreen)
}

pub fn placement(ui: &MainWindow) -> Option<Placement> {
    let mtm = MainThreadMarker::new()?;
    let window = ns_window(ui)?;
    let screen = window.screen()?;
    let primary_h = primary_h(mtm)?;
    let arranged = arranged(&window);
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

/// The standard title bar's height, in points: the window doesn't exist
/// yet when [`place_window`] needs it.
const TITLE_BAR: f32 = 28.0;

/// Where the window is (docs/66 D18): its client area's top-left, in
/// points from the primary screen's top-left. The client area, not the
/// frame, because winit creates a window from its content rectangle, so a
/// frame position would reopen a title bar higher every launch. `None`
/// while zoomed, minimized or full screen, whose frames aren't yours:
/// AppKit keeps no restore rectangle to ask, so the last one seen stands.
pub fn window_position(ui: &MainWindow) -> Option<(i32, i32)> {
    let mtm = MainThreadMarker::new()?;
    let window = ns_window(ui)?;
    if arranged(&window) {
        return None;
    }
    let content = fit::from_appkit(
        rect(window.contentRectForFrameRect(window.frame())),
        primary_h(mtm)?,
    );
    Some((content.x.round() as i32, content.y.round() as i32))
}

/// Before the window exists (docs/66 D18): open it at `saved` if its title
/// bar would still be on a screen, pulled in to that screen's visible
/// frame. `width` is the client width, in points as everything here is.
pub fn place_window(ui: &MainWindow, saved: (i32, i32), width: f32) {
    let Some(mtm) = MainThreadMarker::new() else {
        return;
    };
    let Some(primary_h) = primary_h(mtm) else {
        return;
    };
    let works: Vec<Rect> = NSScreen::screens(mtm)
        .iter()
        .map(|s| fit::from_appkit(rect(s.visibleFrame()), primary_h))
        .collect();
    let saved_f = (saved.0 as f32, saved.1 as f32);
    match fit::launch_content_position(saved_f, TITLE_BAR, width, &works) {
        Some((x, y)) => {
            log::info!(
                "window position: {},{} (saved {},{})",
                x.round(),
                y.round(),
                saved.0,
                saved.1
            );
            ui.window().set_position(slint::LogicalPosition::new(x, y));
        }
        None => log::info!(
            "window position: saved {},{} is on no screen, left to macOS",
            saved.0,
            saved.1
        ),
    }
}
