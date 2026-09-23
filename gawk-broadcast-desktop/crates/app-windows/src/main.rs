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
mod toast;

use gawk_engine::config::{self, Config};
use gawk_ui::MainWindow;
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
}

impl Platform for Windows {
    fn hooks(&self) -> Hooks {
        Hooks { notify, creds }
    }

    fn launch_log(&self) {
        #[cfg(windows)]
        log::info!(
            "UniversalApiContract level: {}",
            gawk_capture::wgc::universal_contract_level()
        );
    }

    fn init_window(&mut self, ui: &MainWindow) {
        self.refresh_picker(ui);
    }

    #[cfg(windows)]
    fn prepare_start(&mut self, ui: &MainWindow, cfg: &Config) -> Result<Prepared, String> {
        let tab = ui.get_picker_tab();
        let target = if tab == 0 {
            let idx = ui.get_selected_window();
            match self.picked_windows.get(idx.max(0) as usize) {
                Some(w) if idx >= 0 => gawk_capture::wgc::CaptureTarget::Window {
                    hwnd: w.hwnd,
                    pid: w.pid,
                },
                _ => return Err("Pick a window (or a screen) to share first.".into()),
            }
        } else {
            let idx = ui.get_selected_monitor();
            match self.picked_monitors.get(idx.max(0) as usize) {
                Some(m) if idx >= 0 => gawk_capture::wgc::CaptureTarget::Monitor {
                    hmonitor: m.hmonitor,
                },
                _ => return Err("Pick a screen (or a window) to share first.".into()),
            }
        };
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

    fn as_any_mut(&mut self) -> &mut dyn Any {
        self
    }
}

impl Windows {
    fn refresh_picker(&mut self, ui: &MainWindow) {
        #[cfg(windows)]
        {
            use gawk_ui::{MonitorRow, WindowRow};
            use slint::{ModelRc, VecModel};
            self.picked_windows = gawk_capture::wgc::enumerate_windows();
            self.picked_monitors = gawk_capture::wgc::enumerate_monitors();
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
        }
        #[cfg(not(windows))]
        {
            let _ = ui;
        }
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
    #[cfg(windows)]
    toast::init();

    shell::run(Box::new(Windows::default()), |ui, shell| {
        let shell = shell.clone();
        let ui_weak = ui.as_weak();
        ui.on_refresh_picker(move || {
            if let Some(ui) = ui_weak.upgrade() {
                shell
                    .borrow_mut()
                    .platform_mut::<Windows>()
                    .refresh_picker(&ui);
            }
        });
    });
}
