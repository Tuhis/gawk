//! The window's layout on Slint's testing backend (R64, docs/66 WF1): the
//! action bar stays in reach at any height, the main page publishes what it
//! needs for window fit, the preview gives up height first, and the alert
//! slot shows one strip.

use crate::{MainWindow, RoomRow, StatRow};
use i_slint_backend_testing::{AccessibleRole, ElementHandle};
use slint::{ComponentHandle, LogicalSize, ModelRc, SharedString, VecModel};
use std::time::Duration;

thread_local! {
    static BACKEND: () = i_slint_backend_testing::init_no_event_loop();
}

/// A MainWindow of this size on the testing backend, shown and settled.
/// The backend is set once per test thread; a test may open several.
fn window(w: f32, h: f32) -> MainWindow {
    BACKEND.with(|_| ());
    let ui = MainWindow::new().unwrap();
    ui.window().set_size(LogicalSize::new(w, h));
    ui.show().unwrap();
    settle();
    ui
}

/// Instantiates `if` blocks and runs `changed` handlers: the testing
/// backend's stand-in for a turn of the event loop. A handler can change
/// what another one reads, so a few turns.
fn settle() {
    for _ in 0..4 {
        i_slint_backend_testing::mock_elapsed_time(Duration::from_millis(16));
    }
}

fn resize(ui: &MainWindow, w: f32, h: f32) {
    ui.window().set_size(LogicalSize::new(w, h));
    settle();
}

fn size_of(ui: &MainWindow) -> LogicalSize {
    ui.window().size().to_logical(ui.window().scale_factor())
}

/// Elements with this accessible label and role.
fn find(ui: &MainWindow, label: &str, role: AccessibleRole) -> Vec<ElementHandle> {
    ElementHandle::find_by_accessible_label(ui, label)
        .filter(|e| e.accessible_role() == Some(role))
        .collect()
}

fn by_id(ui: &MainWindow, id: &str) -> ElementHandle {
    ElementHandle::find_by_element_id(ui, id)
        .next()
        .unwrap_or_else(|| panic!("no element {id}"))
}

/// Every button labelled `label` lies wholly inside the window.
fn assert_in_reach(ui: &MainWindow, label: &str) {
    let buttons = find(ui, label, AccessibleRole::Button);
    assert!(!buttons.is_empty(), "no {label:?} button");
    let win = size_of(ui);
    for b in buttons {
        let (p, s) = (b.absolute_position(), b.size());
        assert!(
            p.x >= -0.5
                && p.y >= -0.5
                && p.x + s.width <= win.width + 0.5
                && p.y + s.height <= win.height + 0.5,
            "{label:?} at {p:?}, {s:?} is outside the {win:?} window"
        );
    }
}

fn strings(items: &[&str]) -> ModelRc<SharedString> {
    ModelRc::new(VecModel::from(
        items
            .iter()
            .map(|s| SharedString::from(*s))
            .collect::<Vec<_>>(),
    ))
}

fn stat(value: &str, label: &str) -> StatRow {
    StatRow {
        label: label.into(),
        value: value.into(),
    }
}

/// Ready after End, with everything that can sit above Go live.
fn ready_after_end(ui: &MainWindow) {
    ui.set_error_text("Couldn't start: the server refused the broadcast.".into());
    ui.set_probe_state(2);
    ui.set_probe_host("api.gawk.ioio.fi".into());
    ui.set_summary_visible(true);
    ui.set_summary_rows(ModelRc::new(VecModel::from(vec![
        stat("1:42:10", "Live for"),
        stat("6", "Most watching"),
        stat("8.1 Mbps", "Average upload"),
    ])));
    ui.set_resume_code("K7XQ2M".into());
    ui.set_update_version("v9.9.9".into());
    // Its tallest: the release page with the .deb's instructions (docs/48 D9).
    ui.set_update_note(
        "Installed from the .deb: download gawk-broadcast_9.9.9_amd64.deb, then run \
         sudo apt install ./gawk-broadcast_9.9.9_amd64.deb"
            .into(),
    );
    settle();
}

fn on_air(ui: &MainWindow, live: bool) {
    ui.set_busy(true);
    ui.set_live(live);
    ui.set_code("K7XQ2M".into());
    ui.set_code_chars(strings(&["K", "7", "X", "Q", "2", "M"]));
    ui.set_join_link("https://gawk.ioio.fi/#/view/K7XQ2M".into());
    ui.set_connection_line("8.4 Mbps".into());
    settle();
}

fn paused(ui: &MainWindow) {
    ui.set_busy(false);
    ui.set_live(false);
    ui.set_paused(true);
    ui.set_code("K7XQ2M".into());
    ui.set_code_chars(strings(&["K", "7", "X", "Q", "2", "M"]));
    ui.set_join_link("https://gawk.ioio.fi/#/view/K7XQ2M".into());
    settle();
}

fn in_room(ui: &MainWindow, streams: usize) {
    let rows: Vec<RoomRow> = (0..streams)
        .map(|i| RoomRow {
            name: format!("Stream {i}").into(),
            detail: "Live · 2 watching".into(),
            initial: "S".into(),
            tint: i as i32,
            you: i == 0,
            away: false,
            speaking: false,
            broadcast_id: format!("ID{i}").into(),
            watch_link: if i == 0 {
                "".into()
            } else {
                "https://gawk.ioio.fi/#/view/ID".into()
            },
            removable: false,
        })
        .collect();
    ui.set_room_active(true);
    ui.set_room_code("R4MBLE".into());
    ui.set_room_link("https://gawk.ioio.fi/#/room/R4MBLE".into());
    ui.set_room_streaming(streams as i32);
    ui.set_room_watching(7);
    ui.set_room_rows(ModelRc::new(VecModel::from(rows)));
    ui.set_room_watchers("Jere, Aino, Lumi and 4 more watching".into());
    ui.set_room_watcher_initials(strings(&["J", "A", "L", "+4"]));
    settle();
}

const LIVE_BAR: [&str; 4] = ["Copy link", "Copy code", "Pause", "End"];
const PAUSED_BAR: [&str; 4] = ["Copy link", "Copy code", "Resume", "End"];

// docs/66 WF1 (a): at the minimum size, every bar control is in the window,
// however much sits above it.
#[test]
fn the_bar_stays_in_reach_at_the_minimum_size() {
    let ui = window(440.0, 600.0);
    ready_after_end(&ui);
    assert!(ui.get_body_scrolls(), "the fixture must overflow");
    assert_in_reach(&ui, "Go live");
    // The update notice sits at the top (docs/47 D7, revised), and its
    // buttons fit the narrowest window.
    assert_in_reach(&ui, "Download");
    ui.set_update_note("".into());
    ui.set_update_phase(2);
    settle();
    for label in ["Install and relaunch", "What's new"] {
        assert_in_reach(&ui, label);
    }

    let ui = window(440.0, 600.0);
    ui.set_server_custom(true);
    ui.set_server_name("Home lab".into());
    ui.set_server_host("relay.example.net".into());
    on_air(&ui, true);
    in_room(&ui, 8);
    ui.set_uplink_warning("Your upload can't keep up with the stream.".into());
    settle();
    assert!(ui.get_body_scrolls());
    for label in LIVE_BAR {
        assert_in_reach(&ui, label);
    }

    let ui = window(440.0, 600.0);
    paused(&ui);
    ui.set_crash_resume(true);
    in_room(&ui, 3);
    for label in PAUSED_BAR {
        assert_in_reach(&ui, label);
    }

    // Starting: no code yet, nothing to pause, but the bar is there.
    let ui = window(440.0, 600.0);
    ui.set_busy(true);
    settle();
    for label in LIVE_BAR {
        assert_in_reach(&ui, label);
    }
}

/// At the height the page says it needs, nothing scrolls; a pixel less and
/// the body does (docs/66 D8, WF1 (b)).
fn assert_needs_exact(ui: &MainWindow, w: f32, what: &str) {
    let needs_min = ui.get_page_needs_min();
    let needs = ui.get_page_needs();
    assert!(needs_min > 0.0, "{what}: nothing published");
    assert!(needs >= needs_min, "{what}: {needs} < {needs_min}");
    resize(ui, w, needs_min);
    assert!(!ui.get_body_scrolls(), "{what}: scrolls at {needs_min}");
    resize(ui, w, needs_min - 1.0);
    assert!(
        ui.get_body_scrolls(),
        "{what}: doesn't scroll at {needs_min} - 1"
    );
    resize(ui, w, needs);
    assert!(!ui.get_body_scrolls(), "{what}: scrolls at {needs}");
}

#[test]
fn ready_publishes_what_it_needs() {
    let ui = window(520.0, 600.0);
    ready_after_end(&ui);
    assert_needs_exact(&ui, 520.0, "Ready after End");
    // At what it needs, the source card is at its natural size.
    assert_eq!(by_id(&ui, "MainWindow::source-card").size().height, 150.0);

    // A source preview (docs/65 D2) is the card's natural size too, and
    // gives up height, to its own floor, like the icon card.
    ui.set_has_preview(true);
    settle();
    assert_needs_exact(&ui, 520.0, "Ready after End, with a preview");
    assert_eq!(by_id(&ui, "MainWindow::source-card").size().height, 220.0);
    // With every notice up, 440 × 600 scrolls the card out of view, and the
    // testing backend finds only what is in view. The floor doesn't depend
    // on what is above the card, so one notice fewer.
    ui.set_update_version("".into());
    resize(&ui, 440.0, 600.0);
    assert_eq!(by_id(&ui, "MainWindow::source-card").size().height, 160.0);
}

#[test]
fn live_publishes_what_it_needs_alone_in_a_room_and_wide() {
    let ui = window(520.0, 800.0);
    on_air(&ui, true);
    assert_needs_exact(&ui, 520.0, "Live alone");
    assert_eq!(by_id(&ui, "MainWindow::preview").size().height, 248.0);
    // The preview is the only thing between the two heights.
    assert_eq!(ui.get_page_needs() - ui.get_page_needs_min(), 88.0);

    let ui = window(520.0, 800.0);
    on_air(&ui, true);
    in_room(&ui, 3);
    assert_needs_exact(&ui, 520.0, "Live in a room of three");

    let ui = window(960.0, 700.0);
    on_air(&ui, true);
    in_room(&ui, 3);
    assert_needs_exact(&ui, 960.0, "wide, in a room of three");
}

// docs/66 D5: at the default size Live alone fits, the preview giving up
// what it must; at the minimum the preview is at its floor.
#[test]
fn the_preview_gives_first() {
    let ui = window(520.0, 800.0);
    on_air(&ui, true);
    assert!(!ui.get_body_scrolls(), "Live alone fits the default window");
    let h = by_id(&ui, "MainWindow::preview").size().height;
    assert!((160.0..248.0).contains(&h), "preview {h}");

    resize(&ui, 440.0, 600.0);
    assert_eq!(by_id(&ui, "MainWindow::preview").size().height, 160.0);
}

// docs/66 D17, WF1 (c): the Paused overlay fits the preview at its minimum.
#[test]
fn the_paused_overlay_fits_the_smallest_preview() {
    let ui = window(440.0, 600.0);
    paused(&ui);
    in_room(&ui, 3);
    let preview = by_id(&ui, "MainWindow::preview");
    assert_eq!(preview.size().height, 160.0);
    let (pp, ps) = (preview.absolute_position(), preview.size());
    let words = find(&ui, "Paused", AccessibleRole::Text);
    assert!(!words.is_empty(), "no Paused overlay");
    for w in words {
        let (p, s) = (w.absolute_position(), w.size());
        assert!(
            p.y >= pp.y && p.y + s.height <= pp.y + ps.height,
            "Paused at {p:?}, {s:?} is outside the preview at {pp:?}, {ps:?}"
        );
    }
}

// docs/66 D4, WF1 (d): one strip, the most urgent first.
#[test]
fn the_alert_slot_shows_one_strip() {
    const RECONNECTING: &str = "Reconnecting. People watching see your last frame.";
    const UPLOAD: &str = "Your upload can't keep up";
    const NETWORK: &str = "Your Wi-Fi is dropping some video";
    let strips = |ui: &MainWindow| {
        [RECONNECTING, UPLOAD, NETWORK].map(|l| !find(ui, l, AccessibleRole::Text).is_empty())
    };

    let ui = window(520.0, 800.0);
    on_air(&ui, true);
    assert_eq!(strips(&ui), [false, false, false]);
    ui.set_resuming(true);
    ui.set_uplink_warning("Your upload can't keep up with the stream.".into());
    ui.set_network_notice(NETWORK.into());
    settle();
    assert_eq!(strips(&ui), [true, false, false]);
    ui.set_resuming(false);
    settle();
    assert_eq!(strips(&ui), [false, true, false]);
    ui.set_uplink_warning("".into());
    settle();
    assert_eq!(strips(&ui), [false, false, true]);
    // Paused: nothing is sent, so no strip.
    ui.set_live(false);
    ui.set_busy(false);
    ui.set_paused(true);
    settle();
    assert_eq!(strips(&ui), [false, false, false]);
}

// docs/66 D2: Copy code is an icon, its name kept, only where the labelled
// bar doesn't fit; and the bar fits at every width, in any font. A wider
// font (Linux's DejaVu Sans) once pushed End off the default 520 px window.
#[test]
fn copy_code_is_an_icon_only_where_the_labelled_bar_wont_fit() {
    let ui = window(960.0, 800.0);
    on_air(&ui, true);
    let labelled = find(&ui, "Copy code", AccessibleRole::Button);
    assert!(!labelled.is_empty());
    assert!(labelled.iter().all(|b| b.size().width > 40.0));

    resize(&ui, 440.0, 800.0);
    let icon = find(&ui, "Copy code", AccessibleRole::Button);
    assert!(!icon.is_empty());
    assert!(icon.iter().all(|b| b.size().width == 40.0));

    for w in [440.0, 480.0, 520.0, 560.0, 600.0] {
        resize(&ui, w, 800.0);
        for label in LIVE_BAR {
            assert_in_reach(&ui, label);
        }
    }
    let ui = window(520.0, 800.0);
    paused(&ui);
    ui.set_crash_resume(true);
    settle();
    for w in [440.0, 480.0, 520.0, 560.0, 600.0] {
        resize(&ui, w, 800.0);
        for label in PAUSED_BAR {
            assert_in_reach(&ui, label);
        }
    }
}
