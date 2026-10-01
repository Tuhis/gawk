//! Where the window and the screen's work area are, for window fit (R64,
//! docs/66 D15). Win32 measures in physical px; `gawk_ui::fit` converts and
//! decides.

use gawk_ui::MainWindow;
use gawk_ui::fit::{self, Placement, Rect};
use raw_window_handle::{HasWindowHandle, RawWindowHandle};
use slint::ComponentHandle;
use windows::Win32::Foundation::{HWND, POINT, RECT};
use windows::Win32::Graphics::Gdi::{
    GetMonitorInfoW, MONITOR_DEFAULTTONEAREST, MONITOR_DEFAULTTOPRIMARY, MONITORINFO,
    MonitorFromPoint, MonitorFromWindow,
};
use windows::Win32::System::LibraryLoader::{GetModuleHandleW, GetProcAddress};
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

fn hwnd(ui: &MainWindow) -> Option<HWND> {
    let window = ui.window().window_handle();
    match window.window_handle().ok()?.as_raw() {
        RawWindowHandle::Win32(h) => Some(HWND(h.hwnd.get() as *mut core::ffi::c_void)),
        _ => None,
    }
}

/// The monitor's work area (the screen less the taskbar), in physical px.
fn work_area(monitor: windows::Win32::Graphics::Gdi::HMONITOR) -> Option<RECT> {
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
    // rcNormalPosition is in workspace coordinates, relative to the PRIMARY
    // monitor's work area.
    // SAFETY: a plain lookup of the primary monitor.
    let Some(primary) =
        work_area(unsafe { MonitorFromPoint(POINT { x: 0, y: 0 }, MONITOR_DEFAULTTOPRIMARY) })
    else {
        return false;
    };
    let restore = fit::workspace_to_screen(
        rect(wp.rcNormalPosition),
        (primary.left as f32, primary.top as f32),
    );
    fit::differs_from_restore(window, restore)
}
