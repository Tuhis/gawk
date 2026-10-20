//! The gawk-broadcast Windows shell (WB6, docs/38 D12): the shared shell
//! (`gawk_ui::shell`) with the Windows platform plugged in — the in-app
//! window/screen picker over WGC, the Media Foundation pipeline, toast
//! notifications and DPAPI-wrapped credentials. Everything else — settings,
//! rooms, the session lifecycle, stats — is the shell's, shared with the
//! macOS app since R52 (docs/54 D11).
#![cfg_attr(windows, windows_subsystem = "windows")]

#[cfg_attr(not(windows), allow(dead_code))]
mod base64util;
#[cfg(windows)]
mod dpapi;
#[cfg(windows)]
mod pipeline;
#[cfg(windows)]
mod place;
#[cfg(windows)]
mod preview;
#[cfg_attr(not(windows), allow(dead_code))]
mod register;
#[cfg_attr(not(windows), allow(dead_code))]
mod single;
#[cfg(windows)]
mod toast;

use gawk_engine::config::{self, Config};
use gawk_ui::MainWindow;
use gawk_ui::preview::PreviewFrame;
use gawk_ui::shell::{self, Hooks, Platform, Prepared};
use slint::ComponentHandle;
use std::any::Any;

/// The Windows platform: what the picker last enumerated, so Start can map
/// the selected row back to a capture target.
#[derive(Default)]
struct Windows {
    #[cfg(windows)]
    picked_windows: Vec<gawk_capture::picker::WindowCandidate>,
    #[cfg(windows)]
    picked_monitors: Vec<gawk_capture::picker::MonitorCandidate>,
    /// Ready's preview of the chosen target (docs/65 D3).
    #[cfg(windows)]
    preview: gawk_ui::preview::PreviewSlot<gawk_capture::wgc::CaptureTarget>,
    /// Lists windows and screens again every second while the picker page
    /// is open, so nobody has to press a refresh button.
    poll: slint::Timer,
}

/// How often the open picker lists windows and screens again.
const PICKER_POLL: std::time::Duration = std::time::Duration::from_secs(1);

impl Platform for Windows {
    fn hooks(&self) -> Hooks {
        Hooks { notify, creds }
    }

    fn notices(&self) -> &'static str {
        include_str!("../../../THIRD-PARTY-NOTICES.md")
    }

    fn launch_log(&self) {
        #[cfg(windows)]
        log::info!(
            "UniversalApiContract level: {}",
            gawk_capture::wgc::universal_contract_level()
        );
        // docs/68 D9: here, because this runs once the debug log is up and
        // only in a launch that did not hand off to a running instance.
        #[cfg(windows)]
        register::ensure();
    }

    fn init_window(&mut self, ui: &MainWindow) {
        self.refresh_picker(ui);
    }

    #[cfg(windows)]
    fn prepare_start(&mut self, ui: &MainWindow, cfg: &Config) -> Result<Prepared, String> {
        let tab = ui.get_picker_tab();
        let (target, source) = self.chosen(ui)?;
        let capture_mode = if tab == 0 { "app" } else { "screen" };
        let params = {
            let (w, h, fps, bps) = cfg.resolve_rung();
            pipeline::PipelineParams {
                target,
                width: w,
                height: h,
                fps,
                bitrate_bps: bps,
                last_good_encoder: (!cfg.last_good_encoder.is_empty())
                    .then(|| cfg.last_good_encoder.clone()),
                audio_mode: if cfg.disable_audio {
                    gawk_audio::AudioMode::Off
                } else if capture_mode == "app" {
                    match target {
                        gawk_capture::wgc::CaptureTarget::Window { pid, .. } => {
                            gawk_audio::AudioMode::ProcessLoopback { pid }
                        }
                        _ => gawk_audio::AudioMode::SystemLoopback,
                    }
                } else {
                    gawk_audio::AudioMode::SystemLoopback
                },
            }
        };
        Ok(Prepared {
            capture_mode,
            source_is_window: tab == 0,
            source,
            build: Box::new(move |env| {
                pipeline::Pipeline::build(params, env.sender, env.clock, env.rt)
                    .map(|p| Box::new(p) as Box<dyn shell::Media>)
            }),
        })
    }

    #[cfg(not(windows))]
    fn prepare_start(&mut self, _ui: &MainWindow, _cfg: &Config) -> Result<Prepared, String> {
        Err("This binary only captures on Windows (dev shell).".into())
    }

    #[cfg(windows)]
    fn preview(&mut self, ui: &MainWindow, wanted: bool) -> PreviewFrame {
        let want = if wanted {
            self.chosen(ui).ok().map(|(target, _)| target)
        } else {
            None
        };
        self.preview.update(want, |target| {
            preview::Preview::start(*target)
                .map(|p| Box::new(p) as Box<dyn gawk_ui::preview::PreviewSource>)
        })
    }

    #[cfg(not(windows))]
    fn preview(&mut self, _ui: &MainWindow, _wanted: bool) -> PreviewFrame {
        PreviewFrame::Hidden
    }

    /// Window fit (R64, docs/66 D15): the window's frame and the monitor's
    /// work area.
    #[cfg(windows)]
    fn placement(&self, ui: &MainWindow) -> Option<gawk_ui::fit::Placement> {
        place::placement(ui)
    }

    /// A second launch or a link (docs/68 D6): restore a minimized window
    /// and bring it to the front. The second launch passed its foreground
    /// right on with `AllowSetForegroundWindow` (D8), which is what lets
    /// this take focus instead of only flashing the taskbar button.
    #[cfg(windows)]
    fn raise_window(&mut self, ui: &MainWindow, _activation: Option<String>) {
        use windows::Win32::UI::WindowsAndMessaging::{
            IsIconic, SW_RESTORE, SetForegroundWindow, ShowWindow,
        };
        let _ = ui.show();
        let Some(hwnd) = place::hwnd(ui) else {
            return;
        };
        // SAFETY: `hwnd` is this process's live top-level window.
        unsafe {
            if IsIconic(hwnd).as_bool() {
                let _ = ShowWindow(hwnd, SW_RESTORE);
            }
            let _ = SetForegroundWindow(hwnd);
        }
    }

    fn as_any_mut(&mut self) -> &mut dyn Any {
        self
    }
}

impl Windows {
    /// The chosen source (docs/64 D14): `picker-tab` is committed only by
    /// the picker's Share this / Switch, never by the tab being looked at.
    #[cfg(windows)]
    fn chosen(
        &self,
        ui: &MainWindow,
    ) -> Result<(gawk_capture::wgc::CaptureTarget, String), String> {
        if ui.get_picker_tab() == 0 {
            let idx = ui.get_selected_window();
            match self.picked_windows.get(idx.max(0) as usize) {
                Some(w) if idx >= 0 => Ok((
                    gawk_capture::wgc::CaptureTarget::Window {
                        hwnd: w.hwnd,
                        pid: w.pid,
                    },
                    w.title.clone(),
                )),
                _ => Err("Pick a window (or a screen) to share first.".into()),
            }
        } else {
            let idx = ui.get_selected_monitor();
            match self.picked_monitors.get(idx.max(0) as usize) {
                Some(m) if idx >= 0 => Ok((
                    gawk_capture::wgc::CaptureTarget::Monitor {
                        hmonitor: m.hmonitor,
                    },
                    m.label(),
                )),
                _ => Err("Pick a screen (or a window) to share first.".into()),
            }
        }
    }

    /// Lists windows and screens again. An unchanged listing touches
    /// nothing, so the 1 s poll never rebuilds rows under the pointer. A
    /// changed one keeps the chosen source by its handle — titles change
    /// and indices move — and returns `true` when that source is gone or
    /// nothing was chosen, for the caller to select as at launch.
    fn refresh_picker(&mut self, ui: &MainWindow) -> bool {
        #[cfg(windows)]
        {
            use gawk_ui::{MonitorRow, WindowRow};
            use slint::{ModelRc, VecModel};
            let windows = gawk_capture::wgc::enumerate_windows(&self.picked_windows);
            let monitors = gawk_capture::wgc::enumerate_monitors();
            if gawk_capture::picker::same_windows(&windows, &self.picked_windows)
                && monitors == self.picked_monitors
            {
                return false;
            }
            let window_keys = |v: &[gawk_capture::picker::WindowCandidate]| {
                v.iter().map(|w| (w.hwnd, w.pid)).collect::<Vec<_>>()
            };
            let monitor_keys = |v: &[gawk_capture::picker::MonitorCandidate]| {
                v.iter().map(|m| m.hmonitor).collect::<Vec<_>>()
            };
            let window = follow(
                &window_keys(&self.picked_windows),
                &window_keys(&windows),
                ui.get_selected_window(),
            );
            let monitor = follow(
                &monitor_keys(&self.picked_monitors),
                &monitor_keys(&monitors),
                ui.get_selected_monitor(),
            );
            self.picked_windows = windows;
            self.picked_monitors = monitors;
            let rows: Vec<WindowRow> = self
                .picked_windows
                .iter()
                .map(|w| {
                    let (icon, has_icon) = match &w.icon {
                        Some(i) => (shell::rgba_image(i.width, i.height, &i.rgba), true),
                        None => (slint::Image::default(), false),
                    };
                    WindowRow {
                        hwnd: w.hwnd as i32,
                        pid: w.pid as i32,
                        title: w.title.clone().into(),
                        icon,
                        has_icon,
                    }
                })
                .collect();
            ui.set_windows(ModelRc::new(VecModel::from(rows)));
            let monitors: Vec<MonitorRow> = self
                .picked_monitors
                .iter()
                .map(|m| MonitorRow {
                    hmonitor: m.hmonitor as i32,
                    label: m.label().into(),
                })
                .collect();
            ui.set_monitors(ModelRc::new(VecModel::from(monitors)));
            let kept = if ui.get_picker_tab() == 0 {
                window.map(|w| (w, -1))
            } else {
                monitor.map(|m| (-1, m))
            };
            let Some((w, m)) = kept else {
                return true;
            };
            ui.set_selected_window(w);
            ui.set_selected_monitor(m);
            // The picker page shows the choice too (docs/60 D5).
            ui.set_draft_window(w);
            ui.set_draft_monitor(m);
            false
        }
        #[cfg(not(windows))]
        {
            let _ = ui;
            false
        }
    }
}

/// Where the chosen row went when the list changed: the index of the same
/// key (window handle and PID, or monitor handle) in the new list. `None`
/// when nothing was chosen or it is no longer listed.
#[cfg_attr(not(windows), allow(dead_code))]
fn follow<K: PartialEq>(old: &[K], new: &[K], chosen: i32) -> Option<i32> {
    let key = old.get(usize::try_from(chosen).ok()?)?;
    new.iter().position(|k| k == key).map(|i| i as i32)
}

/// Lists the picker's sources again; when the chosen one is gone, selects
/// as at launch (docs/60 D5).
fn relist(ui: &MainWindow, shell: &std::rc::Rc<std::cell::RefCell<shell::Shell>>) {
    let lost = shell
        .borrow_mut()
        .platform_mut::<Windows>()
        .refresh_picker(ui);
    if lost {
        shell::reselect_source(ui, shell);
    }
}

fn creds() -> Box<dyn config::Credentials> {
    #[cfg(windows)]
    {
        Box::new(dpapi::Dpapi)
    }
    #[cfg(not(windows))]
    {
        Box::new(config::Plaintext)
    }
}

fn notify(summary: &str, body: &str, critical: bool) {
    #[cfg(windows)]
    toast::show(
        summary,
        body,
        if critical {
            toast::Urgency::Critical
        } else {
            toast::Urgency::Normal
        },
    );
    #[cfg(not(windows))]
    eprintln!("[{}] {summary}: {body}", if critical { "!" } else { " " });
}

fn main() {
    // The commit build.rs stamped, for the version badge (crates/ui/build_rev.rs).
    gawk_ui::version::set_build_rev(option_env!("GAWK_BUILD_REV"));
    // Single instance first (docs/68 D8): a second launch hands its link
    // to the running window and exits here, before touching anything else.
    let launch = launch();
    #[cfg(windows)]
    toast::init();

    shell::run(
        Box::new(Windows::default()),
        |ui, shell| {
            // Opening the picker lists at once; while it stays open, every
            // second.
            let (sh, ui_weak) = (shell.clone(), ui.as_weak());
            ui.on_refresh_picker(move || {
                if let Some(ui) = ui_weak.upgrade() {
                    relist(&ui, &sh);
                }
            });
            let (sh, ui_weak) = (shell.clone(), ui.as_weak());
            shell.borrow_mut().platform_mut::<Windows>().poll.start(
                slint::TimerMode::Repeated,
                PICKER_POLL,
                move || {
                    if let Some(ui) = ui_weak.upgrade()
                        && ui.get_page() == 1
                    {
                        relist(&ui, &sh);
                    }
                },
            );
        },
        launch,
    );
}

/// This launch's link, and the inbox later launches arrive on (docs/68 D6,
/// D8). With no endpoint (a dev build off Windows, or one that cannot be
/// set up) the app runs alone with its own link.
fn launch() -> gawk_ui::instance::Launch {
    use gawk_ui::instance::{Launch, Request};
    let args: Vec<String> = std::env::args_os()
        .map(|a| a.to_string_lossy().into_owned())
        .collect();
    #[cfg(windows)]
    let note = match single::Pipe::new() {
        Ok(pipe) => return gawk_ui::instance::launch(&args, Box::new(pipe)),
        Err(e) => Some(format!("no single instance ({e})")),
    };
    #[cfg(not(windows))]
    let note = None;
    Launch {
        note,
        ..Launch::new(Request::from_args(&args), None)
    }
}

#[cfg(test)]
mod tests {
    use super::follow;

    #[test]
    fn the_chosen_row_follows_its_key_when_the_list_changes() {
        let old = [(10, 1), (20, 2), (30, 3)];
        // A window above it closed and a new one opened below it.
        assert_eq!(follow(&old, &[(20, 2), (30, 3), (40, 4)], 1), Some(0));
        assert_eq!(follow(&old, &[(10, 1), (30, 3)], 1), None, "it closed");
        assert_eq!(
            follow(&old, &[(10, 1), (20, 9)], 1),
            None,
            "a reused handle"
        );
        assert_eq!(follow(&old, &old, -1), None, "nothing was chosen");
        assert_eq!(follow(&old, &old, 7), None, "out of range");
    }
}
