//! Picker candidates and the alt-tab eligibility filter (docs/38 D6).
//!
//! The in-app picker is structurally required, not a UX preference: the
//! system `GraphicsCapturePicker` returns an opaque item with no HWND and no
//! PID, and process-loopback audio (D8) needs the PID. So the app enumerates
//! top-level windows itself and must reproduce the alt-tab set — every
//! window a user would expect to see, none of the invisible tool/overlay
//! noise `EnumWindows` actually returns.
//!
//! The decision function is pure and portable so the filter is a unit test;
//! the Windows-only enumeration (`wgc::enumerate_windows`) fills
//! [`WindowProps`] from the real APIs and defers every judgment here.

/// Observable properties of a top-level window, as gathered by enumeration.
#[derive(Debug, Clone, Default)]
pub struct WindowProps {
    pub title: String,
    /// `IsWindowVisible`.
    pub visible: bool,
    /// DWM cloak state (`DWMWA_CLOAKED` != 0): UWP hosts, suspended apps and
    /// other-virtual-desktop windows are "visible" but composited nowhere.
    pub cloaked: bool,
    /// `WS_EX_TOOLWINDOW`: palettes/overlays that alt-tab also hides.
    pub tool_window: bool,
    /// `WS_EX_APPWINDOW`: forces a taskbar/alt-tab presence and overrides
    /// the owner rule below.
    pub app_window: bool,
    /// The window has an owner (`GW_OWNER` != 0): dialogs and satellites;
    /// alt-tab shows their owner instead.
    pub owned: bool,
    /// It is one of our own windows — sharing the picker would be a mirror
    /// maze.
    pub own_process: bool,
}

/// The alt-tab eligibility rule (docs/38 D6, WB3 acceptance row 1).
///
/// Mirrors the shell's documented behavior: visible, uncloaked, titled,
/// unowned top-level windows, minus `WS_EX_TOOLWINDOW`, with
/// `WS_EX_APPWINDOW` overriding the owner exclusion — and never ourselves.
/// Minimized windows stay listed: WGC delivers nothing for them until
/// restored, but excluding them would make "pick the game, then restore it"
/// impossible; the GUI's `IsIconic` hint owns that honesty (D6).
pub fn alt_tab_eligible(w: &WindowProps) -> bool {
    if !w.visible || w.cloaked || w.own_process || w.title.is_empty() {
        return false;
    }
    if w.tool_window && !w.app_window {
        return false;
    }
    if w.owned && !w.app_window {
        return false;
    }
    true
}

/// A window's icon as raw RGBA — the GUI builds its image type from this
/// directly, no image-codec dependency needed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct IconRgba {
    pub width: u32,
    pub height: u32,
    pub rgba: Vec<u8>,
}

/// One pickable window. `hwnd`/`pid` are raw so the type stays portable;
/// only the Windows half ever mints or consumes them.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WindowCandidate {
    pub hwnd: isize,
    pub pid: u32,
    pub title: String,
    pub icon: Option<IconRgba>,
}

/// One pickable monitor.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MonitorCandidate {
    pub hmonitor: isize,
    /// Friendly name when the API yields one, else the device name
    /// ("\\.\DISPLAY1").
    pub name: String,
    pub width: u32,
    pub height: u32,
    pub primary: bool,
}

impl MonitorCandidate {
    /// The picker line: "Dell U2723QE — 3840×2160 (primary)".
    pub fn label(&self) -> String {
        let mut s = format!("{} — {}×{}", self.name, self.width, self.height);
        if self.primary {
            s.push_str(" (primary)");
        }
        s
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn plain(title: &str) -> WindowProps {
        WindowProps {
            title: title.into(),
            visible: true,
            ..Default::default()
        }
    }

    #[test]
    fn plain_titled_visible_windows_are_eligible() {
        assert!(alt_tab_eligible(&plain("Elden Ring")));
    }

    #[test]
    fn invisible_cloaked_untitled_and_own_windows_are_not() {
        assert!(!alt_tab_eligible(&WindowProps {
            visible: false,
            ..plain("Hidden")
        }));
        // Cloaked-but-visible is the UWP/virtual-desktop trap: visibly
        // enumerable, composited nowhere.
        assert!(!alt_tab_eligible(&WindowProps {
            cloaked: true,
            ..plain("Suspended UWP")
        }));
        assert!(!alt_tab_eligible(&plain("")));
        assert!(!alt_tab_eligible(&WindowProps {
            own_process: true,
            ..plain("gawk-broadcast")
        }));
    }

    #[test]
    fn tool_windows_and_owned_dialogs_are_hidden_unless_appwindow_forces_them() {
        assert!(!alt_tab_eligible(&WindowProps {
            tool_window: true,
            ..plain("Palette")
        }));
        assert!(!alt_tab_eligible(&WindowProps {
            owned: true,
            ..plain("Settings dialog")
        }));
        // WS_EX_APPWINDOW is the documented override for both exclusions.
        assert!(alt_tab_eligible(&WindowProps {
            tool_window: true,
            app_window: true,
            ..plain("Forced tool")
        }));
        assert!(alt_tab_eligible(&WindowProps {
            owned: true,
            app_window: true,
            ..plain("Forced owned")
        }));
    }

    #[test]
    fn monitor_label_reads_like_the_picker_line() {
        let m = MonitorCandidate {
            hmonitor: 1,
            name: "Dell U2723QE".into(),
            width: 3840,
            height: 2160,
            primary: true,
        };
        assert_eq!(m.label(), "Dell U2723QE — 3840×2160 (primary)");
    }
}
