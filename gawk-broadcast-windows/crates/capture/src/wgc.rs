//! Windows.Graphics.Capture: the in-app picker's enumeration and the frame
//! source (docs/38 D6).
//!
//! The picker is ours by structural necessity, not preference: the system
//! `GraphicsCapturePicker` returns an item with no HWND and no PID, and
//! process-loopback audio (D8) needs the PID. Enumeration gathers raw window
//! properties and defers every eligibility judgment to the portable
//! [`crate::picker::alt_tab_eligible`] filter, which is where the unit tests
//! live.

use crate::d3d::GpuDevice;
use crate::picker::{IconRgba, MonitorCandidate, WindowCandidate, WindowProps, alt_tab_eligible};
use windows::Foundation::Metadata::ApiInformation;
use windows::Foundation::TypedEventHandler;
use windows::Graphics::Capture::{
    Direct3D11CaptureFramePool, GraphicsCaptureItem, GraphicsCaptureSession,
};
use windows::Graphics::DirectX::DirectXPixelFormat;
use windows::Graphics::SizeInt32;
use windows::Win32::Foundation::{HWND, LPARAM, RECT};
use windows::Win32::Graphics::Direct3D11::ID3D11Texture2D;
use windows::Win32::Graphics::Dwm::{DWMWA_CLOAKED, DwmGetWindowAttribute};
use windows::Win32::Graphics::Gdi::{
    DEVMODEW, ENUM_CURRENT_SETTINGS, EnumDisplayMonitors, EnumDisplaySettingsW, GetMonitorInfoW,
    HDC, HMONITOR, MONITORINFO, MONITORINFOEXW,
};
use windows::Win32::System::WinRT::Direct3D11::IDirect3DDxgiInterfaceAccess;
use windows::Win32::System::WinRT::Graphics::Capture::IGraphicsCaptureItemInterop;
use windows::Win32::UI::WindowsAndMessaging::{
    EnumWindows, GW_OWNER, GWL_EXSTYLE, GetWindow, GetWindowLongPtrW, GetWindowTextW,
    GetWindowThreadProcessId, IsIconic, IsWindowVisible, WS_EX_APPWINDOW, WS_EX_TOOLWINDOW,
};
use windows::core::{BOOL, HSTRING, Interface, Result};

/// What the picker captured about a chosen target; the audio side reads the
/// PID off the window variant.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CaptureTarget {
    Window { hwnd: isize, pid: u32 },
    Monitor { hmonitor: isize },
}

/// One frame off the pool: a BGRA texture on the shared device plus its
/// QPC-derived timestamp (100 ns ticks — `SystemRelativeTime`).
pub struct CapturedFrame {
    pub texture: ID3D11Texture2D,
    pub width: u32,
    pub height: u32,
    pub system_relative_100ns: i64,
}

// Handed from the pool thread to the encode pump by channel; defined because
// the shared device is multithread-protected (d3d::GpuDevice::create).
unsafe impl Send for CapturedFrame {}

/// Enumerates the alt-tab-eligible top-level windows (docs/38 D6): raw
/// properties from the Win32 APIs, judgment from the portable filter.
pub fn enumerate_windows() -> Vec<WindowCandidate> {
    struct EnumState {
        out: Vec<WindowCandidate>,
        own_pid: u32,
    }

    unsafe extern "system" fn enum_proc(hwnd: HWND, lparam: LPARAM) -> BOOL {
        let state = unsafe { &mut *(lparam.0 as *mut EnumState) };

        let mut title_buf = [0u16; 512];
        let len = unsafe { GetWindowTextW(hwnd, &mut title_buf) };
        let title = String::from_utf16_lossy(&title_buf[..len.max(0) as usize]);

        let ex_style = unsafe { GetWindowLongPtrW(hwnd, GWL_EXSTYLE) } as u32;
        let mut cloaked = 0u32;
        let _ = unsafe {
            DwmGetWindowAttribute(
                hwnd,
                DWMWA_CLOAKED,
                &mut cloaked as *mut u32 as *mut _,
                std::mem::size_of::<u32>() as u32,
            )
        };
        let mut pid = 0u32;
        unsafe { GetWindowThreadProcessId(hwnd, Some(&mut pid)) };

        let props = WindowProps {
            title: title.clone(),
            visible: unsafe { IsWindowVisible(hwnd) }.as_bool(),
            cloaked: cloaked != 0,
            tool_window: ex_style & WS_EX_TOOLWINDOW.0 != 0,
            app_window: ex_style & WS_EX_APPWINDOW.0 != 0,
            owned: !unsafe { GetWindow(hwnd, GW_OWNER) }
                .map(|w| w.is_invalid())
                .unwrap_or(true),
            own_process: pid == state.own_pid,
        };
        if alt_tab_eligible(&props) {
            state.out.push(WindowCandidate {
                hwnd: hwnd.0 as isize,
                pid,
                title,
                icon: window_icon(hwnd),
            });
        }
        true.into()
    }

    let mut state = EnumState {
        out: Vec::new(),
        own_pid: std::process::id(),
    };
    unsafe {
        let _ = EnumWindows(
            Some(enum_proc),
            LPARAM(&mut state as *mut EnumState as isize),
        );
    }
    state.out
}

/// The window's icon, best-effort: WM_GETICON (timeout-guarded — a hung
/// window must not hang the picker), then the class icon, drawn out of the
/// color bitmap as RGBA. `None` is fine; the GUI shows title-only rows.
fn window_icon(hwnd: HWND) -> Option<IconRgba> {
    use windows::Win32::UI::WindowsAndMessaging::{
        GCLP_HICON, GetClassLongPtrW, GetIconInfo, HICON, ICON_SMALL2, ICONINFO, SMTO_ABORTIFHUNG,
        SendMessageTimeoutW, WM_GETICON,
    };

    unsafe {
        let mut got = 0usize;
        let _ = SendMessageTimeoutW(
            hwnd,
            WM_GETICON,
            windows::Win32::Foundation::WPARAM(ICON_SMALL2 as usize),
            LPARAM(0),
            SMTO_ABORTIFHUNG,
            50,
            Some(&mut got),
        );
        let mut hicon = HICON(got as *mut _);
        if hicon.is_invalid() {
            hicon = HICON(GetClassLongPtrW(hwnd, GCLP_HICON) as *mut _);
        }
        if hicon.is_invalid() {
            return None;
        }

        let mut info = ICONINFO::default();
        GetIconInfo(hicon, &mut info).ok()?;
        let out = bitmap_rgba(info.hbmColor);
        let _ = windows::Win32::Graphics::Gdi::DeleteObject(info.hbmColor.into());
        let _ = windows::Win32::Graphics::Gdi::DeleteObject(info.hbmMask.into());
        out
    }
}

fn bitmap_rgba(hbm: windows::Win32::Graphics::Gdi::HBITMAP) -> Option<IconRgba> {
    use windows::Win32::Graphics::Gdi::{
        BI_RGB, BITMAP, BITMAPINFO, BITMAPINFOHEADER, DIB_RGB_COLORS, GetDC, GetDIBits, GetObjectW,
        ReleaseDC,
    };

    unsafe {
        let mut bmp = BITMAP::default();
        if GetObjectW(
            hbm.into(),
            std::mem::size_of::<BITMAP>() as i32,
            Some(&mut bmp as *mut _ as *mut _),
        ) == 0
            || bmp.bmWidth <= 0
            || bmp.bmHeight <= 0
        {
            return None;
        }
        let (w, h) = (bmp.bmWidth as u32, bmp.bmHeight as u32);
        let mut info = BITMAPINFO {
            bmiHeader: BITMAPINFOHEADER {
                biSize: std::mem::size_of::<BITMAPINFOHEADER>() as u32,
                biWidth: w as i32,
                biHeight: -(h as i32), // top-down
                biPlanes: 1,
                biBitCount: 32,
                biCompression: BI_RGB.0,
                ..Default::default()
            },
            ..Default::default()
        };
        let mut bgra = vec![0u8; (w * h * 4) as usize];
        let hdc = GetDC(None);
        let rows = GetDIBits(
            hdc,
            hbm,
            0,
            h,
            Some(bgra.as_mut_ptr() as *mut _),
            &mut info,
            DIB_RGB_COLORS,
        );
        let _ = ReleaseDC(None, hdc);
        if rows == 0 {
            return None;
        }
        // BGRA → RGBA; icons without an alpha channel read back all-zero
        // alpha and would render invisible, so promote those to opaque.
        let any_alpha = bgra.chunks_exact(4).any(|p| p[3] != 0);
        let mut rgba = Vec::with_capacity(bgra.len());
        for p in bgra.chunks_exact(4) {
            rgba.extend_from_slice(&[p[2], p[1], p[0], if any_alpha { p[3] } else { 0xff }]);
        }
        Some(IconRgba {
            width: w,
            height: h,
            rgba,
        })
    }
}

/// Whether a picked window is currently minimized — feeds the GUI's
/// "window is minimized — restore it to resume" hint (docs/38 D6: WGC
/// delivers no frames for minimized windows; honesty beats the ROADMAP's
/// optimistic claim until V-2 says otherwise).
pub fn is_minimized(hwnd: isize) -> bool {
    unsafe { IsIconic(HWND(hwnd as *mut _)) }.as_bool()
}

/// Enumerates monitors with resolution and primary flag.
pub fn enumerate_monitors() -> Vec<MonitorCandidate> {
    unsafe extern "system" fn enum_proc(
        hmonitor: HMONITOR,
        _hdc: HDC,
        _rect: *mut RECT,
        lparam: LPARAM,
    ) -> BOOL {
        let out = unsafe { &mut *(lparam.0 as *mut Vec<MonitorCandidate>) };
        let mut info = MONITORINFOEXW {
            monitorInfo: MONITORINFO {
                cbSize: std::mem::size_of::<MONITORINFOEXW>() as u32,
                ..Default::default()
            },
            ..Default::default()
        };
        if unsafe { GetMonitorInfoW(hmonitor, &mut info.monitorInfo) }.as_bool() {
            let device = String::from_utf16_lossy(
                &info.szDevice[..info
                    .szDevice
                    .iter()
                    .position(|&c| c == 0)
                    .unwrap_or(info.szDevice.len())],
            );
            // Native mode from the adapter settings; the monitor rect is in
            // scaled desktop coordinates and would lie under DPI scaling.
            let mut mode = DEVMODEW {
                dmSize: std::mem::size_of::<DEVMODEW>() as u16,
                ..Default::default()
            };
            let (w, h) = if unsafe {
                EnumDisplaySettingsW(
                    windows::core::PCWSTR(info.szDevice.as_ptr()),
                    ENUM_CURRENT_SETTINGS,
                    &mut mode,
                )
            }
            .as_bool()
            {
                (mode.dmPelsWidth, mode.dmPelsHeight)
            } else {
                let r = info.monitorInfo.rcMonitor;
                (
                    (r.right - r.left).max(0) as u32,
                    (r.bottom - r.top).max(0) as u32,
                )
            };
            out.push(MonitorCandidate {
                hmonitor: hmonitor.0 as isize,
                name: device,
                width: w,
                height: h,
                // MONITORINFOF_PRIMARY
                primary: info.monitorInfo.dwFlags & 1 != 0,
            });
        }
        true.into()
    }

    let mut out: Vec<MonitorCandidate> = Vec::new();
    unsafe {
        let _ = EnumDisplayMonitors(
            None,
            None,
            Some(enum_proc),
            LPARAM(&mut out as *mut Vec<MonitorCandidate> as isize),
        );
    }
    // Primary first, then stable by name — the picker's order.
    out.sort_by(|a, b| b.primary.cmp(&a.primary).then(a.name.cmp(&b.name)));
    out
}

/// Builds the `GraphicsCaptureItem` for a picked target via the interop
/// factory (the system picker is structurally unusable — no HWND/PID).
pub fn create_item(target: CaptureTarget) -> Result<GraphicsCaptureItem> {
    let interop = windows::core::factory::<GraphicsCaptureItem, IGraphicsCaptureItemInterop>()?;
    unsafe {
        match target {
            CaptureTarget::Window { hwnd, .. } => interop.CreateForWindow(HWND(hwnd as *mut _)),
            CaptureTarget::Monitor { hmonitor } => {
                interop.CreateForMonitor(HMONITOR(hmonitor as *mut _))
            }
        }
    }
}

/// A live WGC capture: a free-threaded frame pool feeding `on_frame` from
/// the pool's own thread. Dropping stops the capture.
pub struct Capture {
    session: GraphicsCaptureSession,
    pool: Direct3D11CaptureFramePool,
    /// Kept so `Closed`/`FrameArrived` tokens die with us.
    _item: GraphicsCaptureItem,
}

impl Capture {
    /// Starts capturing `item` on `gpu`'s device. `on_frame` runs on the
    /// frame-pool thread; it must hand the texture off (the engine's gate)
    /// rather than block. The pool is recreated on content-size changes so
    /// window resizes keep delivering full frames.
    pub fn start(
        gpu: &GpuDevice,
        item: GraphicsCaptureItem,
        on_frame: impl FnMut(CapturedFrame) + Send + 'static,
    ) -> Result<Self> {
        let size = item.Size()?;
        let pool = Direct3D11CaptureFramePool::CreateFreeThreaded(
            &gpu.winrt,
            DirectXPixelFormat::B8G8R8A8UIntNormalized,
            2,
            size,
        )?;

        // The handler must be Send but raw COM interfaces are not; WinRT
        // graphics types are agile, and AgileReference is the sanctioned
        // way to say so to the type system.
        let winrt_device = windows::core::AgileReference::new(&gpu.winrt)?;
        // TypedEventHandler wants Fn; the pool thread is single, so the
        // mutex is uncontended bookkeeping, not a lock in anger.
        let state = std::sync::Mutex::new((on_frame, size));
        pool.FrameArrived(&TypedEventHandler::new(
            move |pool: windows::core::Ref<'_, Direct3D11CaptureFramePool>, _| {
                let Some(pool) = pool.as_ref() else {
                    return Ok(());
                };
                let Ok(frame) = pool.TryGetNextFrame() else {
                    return Ok(());
                };
                let (on_frame, last_size) = &mut *state.lock().unwrap();
                let content = frame.ContentSize()?;
                if content != *last_size && content.Width > 0 && content.Height > 0 {
                    // Window resized: recreate at the new size; this frame
                    // is delivered at the old pool size and skipped.
                    *last_size = content;
                    pool.Recreate(
                        &winrt_device.resolve()?,
                        DirectXPixelFormat::B8G8R8A8UIntNormalized,
                        2,
                        content,
                    )?;
                    return Ok(());
                }
                let surface = frame.Surface()?;
                let access: IDirect3DDxgiInterfaceAccess = surface.cast()?;
                let texture: ID3D11Texture2D = unsafe { access.GetInterface()? };
                let ts = frame.SystemRelativeTime()?;
                on_frame(CapturedFrame {
                    texture,
                    width: content.Width.max(0) as u32,
                    height: content.Height.max(0) as u32,
                    system_relative_100ns: ts.Duration,
                });
                Ok(())
            },
        ))?;

        let session = pool.CreateCaptureSession(&item)?;
        // Cursor embedded (R14 invariant); border removed where the API
        // exists (build 20348+) — on plain Windows 10 the yellow border
        // stays, documented not fought (docs/38 D6).
        let _ = session.SetIsCursorCaptureEnabled(true);
        if api_present(
            "Windows.Graphics.Capture.GraphicsCaptureSession",
            "IsBorderRequired",
        ) {
            let _ = session.SetIsBorderRequired(false);
        }
        session.StartCapture()?;

        Ok(Self {
            session,
            pool,
            _item: item,
        })
    }
}

impl Drop for Capture {
    fn drop(&mut self) {
        let _ = self.session.Close();
        let _ = self.pool.Close();
    }
}

fn api_present(type_name: &str, property: &str) -> bool {
    ApiInformation::IsPropertyPresent(&HSTRING::from(type_name), &HSTRING::from(property))
        .unwrap_or(false)
}

/// The pool size a target starts at (also the sanity check that the item is
/// alive). Exposed for the shell's pre-start validation.
pub fn item_size(item: &GraphicsCaptureItem) -> Result<SizeInt32> {
    item.Size()
}
