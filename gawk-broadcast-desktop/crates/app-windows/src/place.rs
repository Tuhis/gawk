//! Where the window and the screen's work area are, for window fit (R64,
//! docs/66 D15). Win32 measures in physical px; `gawk_ui::fit` converts and
//! decides.

use gawk_ui::MainWindow;
use gawk_ui::fit::{self, Placement, Rect};
use raw_window_handle::{HasWindowHandle, RawWindowHandle};
use slint::ComponentHandle;
use windows::Win32::Foundation::LPARAM;
use windows::Win32::Foundation::{HWND, POINT, RECT};
use windows::Win32::Graphics::Gdi::{
    EnumDisplayMonitors, GetMonitorInfoW, HDC, HMONITOR, MONITOR_DEFAULTTONEAREST,
    MONITOR_DEFAULTTOPRIMARY, MONITORINFO, MonitorFromPoint, MonitorFromWindow,
};
use windows::Win32::System::LibraryLoader::{GetModuleHandleW, GetProcAddress};
use windows::Win32::UI::HiDpi::{GetDpiForMonitor, MDT_EFFECTIVE_DPI};
use windows::Win32::UI::WindowsAndMessaging::{
    GetWindowPlacement, GetWindowRect, IsIconic, IsZoomed, SW_SHOWNORMAL, WINDOWPLACEMENT,
};
use windows::core::{BOOL, s, w};

fn rect(r: RECT) -> Rect {
    Rect::new(
        r.left as f32,
        r.top as f32,
        (r.right - r.left) as f32,
        (r.bottom - r.top) as f32,
    )
}

pub(crate) fn hwnd(ui: &MainWindow) -> Option<HWND> {
    let window = ui.window().window_handle();
    match window.window_handle().ok()?.as_raw() {
        RawWindowHandle::Win32(h) => Some(HWND(h.hwnd.get() as *mut core::ffi::c_void)),
        _ => None,
    }
}

/// The monitor's work area (the screen less the taskbar), in physical px.
fn work_area(monitor: HMONITOR) -> Option<RECT> {
    let mut info = MONITORINFO {
        cbSize: size_of::<MONITORINFO>() as u32,
        ..Default::default()
    };
    // SAFETY: `info` is a MONITORINFO with its size set, as the call requires.
    unsafe { GetMonitorInfoW(monitor, &mut info) }
        .as_bool()
        .then_some(info.rcWork)
}

pub fn placement(ui: &MainWindow) -> Option<Placement> {
    let hwnd = hwnd(ui)?;
    let scale = ui.window().scale_factor();
    let mut frame = RECT::default();
    // SAFETY: `hwnd` is this process's live top-level window; `frame` is a
    // RECT the call fills.
    unsafe { GetWindowRect(hwnd, &mut frame) }.ok()?;
    // SAFETY: as above.
    let work = work_area(unsafe { MonitorFromWindow(hwnd, MONITOR_DEFAULTTONEAREST) })?;
    // SAFETY: as above; both only read the window's state.
    let arranged = unsafe { IsZoomed(hwnd).as_bool() || IsIconic(hwnd).as_bool() }
        || snapped(hwnd, rect(frame));
    Some(Placement {
        // GetWindowRect includes the invisible resize borders (≈ 7 px at the
        // sides and bottom): conservative for the bottom-edge test, and the
        // same space SetWindowPos — and so set_position — uses (docs/66 §4.1).
        frame: fit::to_logical(rect(frame), scale),
        client_h: ui.window().size().to_logical(scale).height,
        work: fit::to_logical(rect(work), scale),
        positioned: true,
        arranged,
    })
}

/// Windows Snap. `IsWindowArranged` is documented only for recent builds, and
/// a static import that user32.dll lacks stops the EXE from starting, so it
/// is looked up at run time. Where it's missing, a window that isn't
/// maximized but isn't at its restore rectangle has been arranged.
fn snapped(hwnd: HWND, window: Rect) -> bool {
    type IsWindowArranged = unsafe extern "system" fn(HWND) -> BOOL;
    // SAFETY: user32.dll is loaded in every GUI process; the name is a
    // static NUL-terminated string.
    let found = unsafe {
        GetModuleHandleW(w!("user32.dll"))
            .ok()
            .and_then(|user32| GetProcAddress(user32, s!("IsWindowArranged")))
    };
    if let Some(f) = found {
        // SAFETY: IsWindowArranged's documented signature is
        // `BOOL IsWindowArranged(HWND)`, stdcall/system ABI.
        let f = unsafe {
            std::mem::transmute::<unsafe extern "system" fn() -> isize, IsWindowArranged>(f)
        };
        // SAFETY: `hwnd` is this process's live window.
        return unsafe { f(hwnd) }.as_bool();
    }
    let mut wp = WINDOWPLACEMENT {
        length: size_of::<WINDOWPLACEMENT>() as u32,
        ..Default::default()
    };
    // SAFETY: `wp` has its length set, as the call requires.
    if unsafe { GetWindowPlacement(hwnd, &mut wp) }.is_err() || wp.showCmd != SW_SHOWNORMAL.0 as u32
    {
        return false;
    }
    let Some(restore) = restore_rect(&wp) else {
        return false;
    };
    fit::differs_from_restore(window, restore)
}

/// `rcNormalPosition` in screen coordinates, physical px. It is in
/// workspace coordinates, relative to the PRIMARY monitor's work area.
fn restore_rect(wp: &WINDOWPLACEMENT) -> Option<Rect> {
    // SAFETY: a plain lookup of the primary monitor.
    let primary =
        work_area(unsafe { MonitorFromPoint(POINT { x: 0, y: 0 }, MONITOR_DEFAULTTOPRIMARY) })?;
    Some(fit::workspace_to_screen(
        rect(wp.rcNormalPosition),
        (primary.left as f32, primary.top as f32),
    ))
}

/// Where the window restores to (docs/66 D18): the top-left of its restore
/// rectangle, physical px, so a position kept while it is maximized,
/// snapped or minimized is the one it had before. Physical, not logical:
/// with monitors at different scales, one logical space doesn't exist.
pub fn window_position(ui: &MainWindow) -> Option<(i32, i32)> {
    let hwnd = hwnd(ui)?;
    let mut wp = WINDOWPLACEMENT {
        length: size_of::<WINDOWPLACEMENT>() as u32,
        ..Default::default()
    };
    // SAFETY: `wp` has its length set, as the call requires; `hwnd` is this
    // process's live window.
    unsafe { GetWindowPlacement(hwnd, &mut wp) }.ok()?;
    let r = restore_rect(&wp)?;
    Some((r.x.round() as i32, r.y.round() as i32))
}

/// Before the window exists (docs/66 D18): open it at `saved` if that is
/// still on a monitor, pulled in to its work area. `width` is the logical
/// client width; the monitor's scale makes it physical.
pub fn place_window(ui: &MainWindow, saved: (i32, i32), width: f32) {
    let works = monitors();
    let at = POINT {
        x: saved.0,
        y: saved.1,
    };
    // SAFETY: plain lookups; `dpi` is filled by the call.
    let scale = unsafe {
        let monitor = MonitorFromPoint(at, MONITOR_DEFAULTTONEAREST);
        let (mut dpi, mut _y) = (96u32, 0u32);
        GetDpiForMonitor(monitor, MDT_EFFECTIVE_DPI, &mut dpi, &mut _y)
            .map(|()| dpi as f32 / 96.0)
            .unwrap_or(1.0)
    };
    let saved_f = (saved.0 as f32, saved.1 as f32);
    match fit::launch_position(saved_f, width * scale, &works) {
        Some((x, y)) => {
            log::info!(
                "window position: {},{} (saved {},{})",
                x.round(),
                y.round(),
                saved.0,
                saved.1
            );
            ui.window().set_position(slint::PhysicalPosition::new(
                x.round() as i32,
                y.round() as i32,
            ));
        }
        None => log::info!(
            "window position: saved {},{} is on no monitor, left to Windows",
            saved.0,
            saved.1
        ),
    }
}

/// Every monitor's work area, physical px.
fn monitors() -> Vec<Rect> {
    unsafe extern "system" fn each(m: HMONITOR, _: HDC, _: *mut RECT, data: LPARAM) -> BOOL {
        // SAFETY: `data` is the `&mut Vec<Rect>` passed below, alive for the
        // whole enumeration.
        let out = unsafe { &mut *(data.0 as *mut Vec<Rect>) };
        if let Some(work) = work_area(m) {
            out.push(rect(work));
        }
        BOOL(1)
    }
    let mut out: Vec<Rect> = Vec::new();
    // SAFETY: no DC and no clip: every monitor on the desktop. The callback
    // only runs during the call, while `out` lives.
    let _ = unsafe {
        EnumDisplayMonitors(
            None,
            None,
            Some(each),
            LPARAM(&mut out as *mut Vec<Rect> as isize),
        )
    };
    out
}
