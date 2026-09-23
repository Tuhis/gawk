//! The macOS window: the shared `MainWindow` in system-picker mode (docs/54
//! D11), driving the MB2 capture. Encode and send arrive in MB3, so Start
//! runs the capture alone — a capture test the header says is one — which
//! is what MB2's manual criteria are checked against: the picked content,
//! the fitted size, the measured rate, the live thumbnail with the cursor
//! in it, and the minimized hint.

use gawk_capture::fit::fit_within;
use gawk_capture::host;
use gawk_capture::sck::{Capture, Frame, PIXEL_FORMAT, StreamSettings};
use gawk_capture::sck_picker::{Picked, Picker, PickerEvent};
use gawk_capture::sck_policy::{Admission, nv12_thumbnail};
use gawk_engine::clock::{Clock, MonotonicClock, QpcMapper};
use gawk_engine::config::Config;
use gawk_ui::{MainWindow, StatRow, refresh_captions, version};
use slint::{ComponentHandle, Image, ModelRc, Rgba8Pixel, SharedPixelBuffer, VecModel};
use std::cell::RefCell;
use std::rc::Rc;
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::time::Duration;

/// The "what viewers see" thumbnail box and cadence (docs/54 D11: 1 Hz).
const THUMB_W: u32 = 320;
const THUMB_H: u32 = 180;
const THUMB_EVERY_US: u64 = 1_000_000;

const CAPTURE_TEST: &str = "Capture test — nothing is sent";

/// Cross-thread events, drained on the GUI thread by the UI timer.
enum Msg {
    Picker(PickerEvent),
    CaptureError(String),
}

/// What the capture callback measures, read by the UI timer.
struct CaptureStats {
    admission: Admission,
    size: Option<(u32, u32)>,
    pixel_format: Option<u32>,
    last_thumb_us: Option<u64>,
    thumb: Option<(u32, u32, Vec<u8>)>,
}

struct Live {
    capture: Capture,
    stats: Arc<Mutex<CaptureStats>>,
    settings: StreamSettings,
}

struct Shell {
    cfg: Config,
    picker: Picker,
    picked: Option<Picked>,
    live: Option<Live>,
    clock: Arc<MonotonicClock>,
    tx: mpsc::Sender<Msg>,
    rx: mpsc::Receiver<Msg>,
}

pub fn run() {
    let ui = MainWindow::new().expect("create window");
    ui.set_app_version(format!("v{}", version::display()).into());
    ui.set_system_picker(true);
    // Consolas is Windows-only; Menlo ships with every macOS.
    ui.set_mono_font("Menlo".into());

    // Loading and saving the D12 config file is MB5; until then the
    // defaults are the rung, as on a first run (docs/54 G8).
    let cfg = Config::default();
    refresh_captions(&ui, &cfg);

    let (tx, rx) = mpsc::channel();
    let picker = {
        let tx = tx.clone();
        Picker::new(move |ev| {
            let _ = tx.send(Msg::Picker(ev));
        })
    };
    let shell = Rc::new(RefCell::new(Shell {
        cfg,
        picker,
        picked: None,
        live: None,
        clock: Arc::new(MonotonicClock::new()),
        tx,
        rx,
    }));

    wire_callbacks(&ui, &shell);

    let timer = slint::Timer::default();
    {
        let ui_weak = ui.as_weak();
        let shell = shell.clone();
        timer.start(
            slint::TimerMode::Repeated,
            Duration::from_millis(250),
            move || {
                if let Some(ui) = ui_weak.upgrade() {
                    pump(&ui, &shell);
                    refresh_live(&ui, &shell.borrow());
                }
            },
        );
    }

    ui.run().expect("run event loop");
    // ⌘Q and a confirmed close both land here: stop the stream before the
    // process goes (docs/54 §6 "Stopping").
    stop(&mut shell.borrow_mut());
}

fn wire_callbacks(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    {
        let shell = shell.clone();
        ui.on_choose_content(move || {
            let sh = shell.borrow();
            match &sh.live {
                Some(live) => sh.picker.present_for(&live.capture),
                None => sh.picker.present(),
            }
        });
    }
    {
        let ui_weak = ui.as_weak();
        let shell = shell.clone();
        ui.on_start_broadcast(move || {
            let Some(ui) = ui_weak.upgrade() else { return };
            start(&ui, &mut shell.borrow_mut());
        });
    }
    {
        let ui_weak = ui.as_weak();
        let shell = shell.clone();
        ui.on_stop_broadcast(move || {
            let Some(ui) = ui_weak.upgrade() else { return };
            stop(&mut shell.borrow_mut());
            show_idle(&ui);
        });
    }
    {
        let ui_weak = ui.as_weak();
        let shell = shell.clone();
        ui.window().on_close_requested(move || {
            let live = shell.borrow().live.is_some();
            match ui_weak.upgrade() {
                Some(ui) if live => {
                    ui.set_confirm_close(true);
                    slint::CloseRequestResponse::KeepWindowShown
                }
                _ => slint::CloseRequestResponse::HideWindow,
            }
        });
    }
    {
        let shell = shell.clone();
        ui.on_quit_confirmed(move || {
            stop(&mut shell.borrow_mut());
            let _ = slint::quit_event_loop();
        });
    }
}

/// The rung box fitted to the picked content (docs/54 D9).
fn settings_for(cfg: &Config, picked: &Picked) -> StreamSettings {
    let (box_w, box_h, fps, _) = cfg.resolve_rung();
    let (width, height) = fit_within(picked.width, picked.height, box_w, box_h);
    StreamSettings { width, height, fps }
}

fn start(ui: &MainWindow, sh: &mut Shell) {
    if sh.live.is_some() {
        return;
    }
    let Some(picked) = &sh.picked else {
        ui.set_error_text("Choose what to share first.".into());
        return;
    };
    let settings = settings_for(&sh.cfg, picked);
    let stats = Arc::new(Mutex::new(CaptureStats {
        admission: Admission::new(settings.fps),
        size: None,
        pixel_format: None,
        last_thumb_us: None,
        thumb: None,
    }));
    let mapper = host::mapper(&*sh.clock);
    let on_frame = {
        let stats = stats.clone();
        let clock = sh.clock.clone();
        move |frame: &Frame<'_>| on_frame(frame, &stats, &mapper, &*clock)
    };
    let on_error = {
        let tx = sh.tx.clone();
        move |text: String| {
            let _ = tx.send(Msg::CaptureError(text));
        }
    };
    match Capture::start(picked, settings, on_frame, on_error) {
        Ok(capture) => {
            log::info!(
                "capture started: {} at {}x{}@{}",
                picked.summary,
                settings.width,
                settings.height,
                settings.fps
            );
            sh.live = Some(Live {
                capture,
                stats,
                settings,
            });
            // Active while live, so the system's menu-bar sharing control
            // can re-pick too (D4); off again in `stop`.
            sh.picker.set_active(true);
            ui.set_error_text("".into());
            ui.set_busy(true);
            ui.set_live(true);
            ui.set_state_label(CAPTURE_TEST.into());
            ui.set_encode_line("Starting capture…".into());
        }
        Err(e) => ui.set_error_text(e.into()),
    }
}

/// The capture queue's per-frame work: judge, measure, and at 1 Hz sample
/// the thumbnail. Never blocks and never keeps the frame (D4/D10).
fn on_frame(frame: &Frame<'_>, stats: &Mutex<CaptureStats>, mapper: &QpcMapper, clock: &dyn Clock) {
    let ts_us = frame
        .pts_100ns
        .map(|p| mapper.to_session_us(p))
        .unwrap_or_else(|| clock.now_us());
    let mut st = stats.lock().unwrap();
    if st.admission.judge(frame.status, ts_us).is_err() {
        return;
    }
    // MB3: VTCompressionSessionEncodeFrame goes here.
    st.size = frame.size();
    st.pixel_format = frame.pixel_format();
    let due = st
        .last_thumb_us
        .is_none_or(|t| ts_us.saturating_sub(t) >= THUMB_EVERY_US);
    if due
        && let Some(thumb) =
            frame.with_planes(|y, uv, w, h| nv12_thumbnail(y, uv, w, h, THUMB_W, THUMB_H))
    {
        st.thumb = Some(thumb);
        st.last_thumb_us = Some(ts_us);
    }
}

fn stop(sh: &mut Shell) {
    if let Some(live) = sh.live.take() {
        live.capture.stop();
        log::info!("capture stopped");
    }
    sh.picker.set_active(false);
}

fn show_idle(ui: &MainWindow) {
    ui.set_busy(false);
    ui.set_live(false);
    ui.set_confirm_close(false);
    ui.set_state_label("Not broadcasting".into());
    ui.set_encode_line("".into());
    ui.set_minimized_hint("".into());
    ui.set_show_thumbnail(false);
    ui.set_stats_rows(ModelRc::new(VecModel::from(Vec::<StatRow>::new())));
}

fn pump(ui: &MainWindow, shell: &Rc<RefCell<Shell>>) {
    let mut sh = shell.borrow_mut();
    while let Ok(msg) = sh.rx.try_recv() {
        if let Msg::Picker(_) = msg {
            // The picker is done with the screen; keep it active only while
            // a capture runs (see `Picker`).
            sh.picker.set_active(sh.live.is_some());
        }
        match msg {
            Msg::Picker(PickerEvent::Picked(picked)) => {
                log::info!("picked {picked:?}");
                ui.set_share_summary(picked.summary.clone().into());
                ui.set_share_mode_label(picked.style.audio_scope().label().into());
                ui.set_error_text("".into());
                let settings = settings_for(&sh.cfg, &picked);
                if let Some(live) = &mut sh.live {
                    // Re-pick while live (D4): same stream, new content,
                    // re-fitted to the new source.
                    live.capture.update(&picked, settings);
                    live.settings = settings;
                }
                sh.picked = Some(picked);
            }
            Msg::Picker(PickerEvent::Cancelled) => {}
            Msg::Picker(PickerEvent::Failed(text)) => ui.set_error_text(text.into()),
            Msg::CaptureError(text) => {
                // A session error: the capture ends, the GUI says why (D3).
                log::warn!("{text}");
                stop(&mut sh);
                show_idle(ui);
                ui.set_error_text(text.into());
            }
        }
    }
}

fn refresh_live(ui: &MainWindow, sh: &Shell) {
    let Some(live) = &sh.live else { return };
    let mut st = live.stats.lock().unwrap();
    let now = sh.clock.now_us();
    let fps = st.admission.fps();
    let size = st
        .size
        .map(|(w, h)| format!("{w}×{h}"))
        .unwrap_or_else(|| "no frame yet".into());
    let format_ok = st.pixel_format.is_none_or(|f| f == PIXEL_FORMAT);
    ui.set_encode_line(
        format!(
            "ScreenCaptureKit · {size}{} · {}",
            if format_ok {
                " 420v"
            } else {
                " (unexpected pixel format)"
            },
            fps.map(|f| format!("{f:.1} fps"))
                .unwrap_or_else(|| "waiting for new content".into()),
        )
        .into(),
    );
    ui.set_minimized_hint(
        if st.admission.stale(now) {
            "The shared window is minimized or hidden — restore it to resume."
        } else {
            ""
        }
        .into(),
    );
    if let Some((w, h, rgba)) = st.thumb.take() {
        let buf = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(&rgba, w, h);
        ui.set_thumbnail(Image::from_rgba8(buf));
        ui.set_show_thumbnail(true);
    }
    let rows = vec![
        stat(
            "Requested size",
            format!("{}×{}", live.settings.width, live.settings.height),
        ),
        stat("Delivered size", size),
        stat("Frame rate cap", format!("{} fps", live.settings.fps)),
        stat("Frames admitted", st.admission.admitted.to_string()),
        stat(
            "Dropped: no new content",
            st.admission.dropped_no_content.to_string(),
        ),
        stat(
            "Dropped: over the rate cap",
            st.admission.dropped_over_rate.to_string(),
        ),
    ];
    ui.set_stats_rows(ModelRc::new(VecModel::from(rows)));
}

fn stat(label: &str, value: String) -> StatRow {
    StatRow {
        label: label.into(),
        value: value.into(),
    }
}
