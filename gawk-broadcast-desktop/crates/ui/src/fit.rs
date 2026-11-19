//! Window fit (R64, docs/66 D8–D14, D16): the window grows to show the main
//! page whole when the screen has room, shrinks back only on a change of
//! state, never fights a manual resize, and remembers the size the user
//! chose, and where it was (D18). Everything here is pure: the shell feeds [`FitState::step`] what
//! it observed on each UI tick and applies the [`Move`] it returns.
//!
//! Platform-free like the rest of the crate: each platform measures a
//! [`Placement`], and the geometry helpers at the bottom convert its native
//! coordinates into one.

use std::time::{Duration, Instant};

/// The window's size before the user ever sets one: `MainWindow`'s
/// preferred size.
pub const DEFAULT_SIZE: (f32, f32) = (520.0, 800.0);
/// `MainWindow`'s min-width and min-height.
pub const MIN_SIZE: (f32, f32) = (440.0, 600.0);
/// A size change this soon after the shell's own is the shell's (D10).
pub const OWN_RESIZE_GRACE: Duration = Duration::from_millis(500);
/// Your size is saved this long after the last manual resize (D10).
pub const SAVE_DELAY: Duration = Duration::from_secs(1);

/// A rectangle: logical px with a top-left origin, unless a helper below
/// says otherwise.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Rect {
    pub x: f32,
    pub y: f32,
    pub w: f32,
    pub h: f32,
}

impl Rect {
    pub fn new(x: f32, y: f32, w: f32, h: f32) -> Self {
        Rect { x, y, w, h }
    }

    pub fn bottom(&self) -> f32 {
        self.y + self.h
    }
}

/// Where the window is and where the screen ends, in logical px with a
/// top-left origin: the space `slint::Window::set_position` takes (D15).
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Placement {
    /// The window's outer frame, title bar included.
    pub frame: Rect,
    /// The client area's height: what `set_size` sets.
    pub client_h: f32,
    /// The screen less its taskbar, Dock or menu bar.
    pub work: Rect,
    /// False where the platform can't say where the window is (Wayland):
    /// fit then never grows the window and never moves it (D14).
    pub positioned: bool,
    /// Maximized, full screen, minimized, snapped or zoomed: hands off (D12).
    pub arranged: bool,
}

/// What the main page needs, as client heights (D8).
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Needs {
    /// Nothing scrolls with the page's elastic element at its minimum.
    pub min: f32,
    /// The same with it at its natural size.
    pub full: f32,
}

/// Why the window changes, for the debug.log line (D16).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Reason {
    /// The page needs more than the window has.
    Grow,
    /// A change of state let the window go back toward your size.
    Shrink,
    /// The window is taller than the screen allows.
    Clamp,
}

/// A change to apply: the new client height and, when the window has to move
/// up to stay on screen, its new top.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Move {
    pub client_h: f32,
    pub y: Option<f32>,
    pub reason: Reason,
}

/// One evaluation of D8–D9 for a window that isn't waiting on the user.
/// `your_h` is the height the user chose; `state_changed` drops the floor
/// to it, otherwise the current height is the floor and nothing shrinks.
pub fn fit(needs: Needs, your_h: f32, state_changed: bool, p: &Placement) -> Option<Move> {
    if p.arranged {
        return None;
    }
    let deco = (p.frame.h - p.client_h).max(0.0);
    let cap = (p.work.h - deco).max(MIN_SIZE.1);
    let current = p.client_h;
    let floor = if state_changed {
        your_h
    } else {
        your_h.max(current)
    }
    .clamp(MIN_SIZE.1, cap);
    let mut target = if needs.min > floor + 0.5 {
        needs.full.max(floor).min(cap)
    } else {
        floor
    };
    if !p.positioned {
        // D14: without knowing where the bottom is, never grow.
        target = target.min(current);
    }
    if (target - current).abs() < 1.0 {
        return None;
    }
    let reason = if target > current {
        Reason::Grow
    } else if current > cap {
        Reason::Clamp
    } else {
        Reason::Shrink
    };
    let y = if p.positioned && p.frame.y + deco + target > p.work.bottom() + 0.5 {
        Some((p.work.bottom() - deco - target).max(p.work.y))
    } else {
        None
    };
    Some(Move {
        client_h: target,
        y,
        reason,
    })
}

/// What the shell saw on one UI tick.
#[derive(Clone, Copy, Debug)]
pub struct Observed {
    /// The client size now, logical.
    pub size: (f32, f32),
    /// The main page (Ready, Live, Paused) shows: fit runs only then (D11).
    pub main_page: bool,
    /// On air, in a room: a change of either is a change of state (D9).
    pub state: (bool, bool),
    pub needs: Needs,
    /// `None` where the platform can't say where the screen ends (D12).
    pub placement: Option<Placement>,
    /// Maximized, full screen or minimized by Slint's own account.
    pub arranged: bool,
    pub now: Instant,
}

/// Window fit across ticks: your size, whether fit waits on the user, and
/// changes of state not yet applied.
#[derive(Clone, Debug)]
pub struct FitState {
    your: (f32, f32),
    waiting: bool,
    state: Option<(bool, bool)>,
    pending: bool,
    /// The main page showed on the last tick. It shows at launch.
    on_main: bool,
    seen: Option<(f32, f32)>,
    own: Option<Instant>,
    /// The last height asked for and the size the window had then. A window
    /// manager that ignores the request (a tiled window) isn't asked the
    /// same again until the size or the target changes.
    asked: Option<(f32, (f32, f32))>,
    save_at: Option<Instant>,
}

fn at_least_min(size: (f32, f32)) -> (f32, f32) {
    (size.0.max(MIN_SIZE.0), size.1.max(MIN_SIZE.1))
}

impl FitState {
    /// From the stored size, `(windowWidth, windowHeight)`: 0 = never set.
    pub fn new(stored: (u32, u32)) -> Self {
        let your = if stored.0 == 0 || stored.1 == 0 {
            DEFAULT_SIZE
        } else {
            at_least_min((stored.0 as f32, stored.1 as f32))
        };
        FitState {
            your,
            waiting: false,
            state: None,
            pending: false,
            on_main: true,
            seen: None,
            own: None,
            asked: None,
            save_at: None,
        }
    }

    /// Your size: the window's as the user last set it by hand.
    pub fn your(&self) -> (f32, f32) {
        self.your
    }

    /// Fit waits for the next change of state: the user resized by hand.
    pub fn waiting(&self) -> bool {
        self.waiting
    }

    /// The size the window opens at (D10, D13): your size, at least the
    /// minimum. The work area clamps it on the first tick.
    pub fn launch_size(&self) -> (f32, f32) {
        at_least_min(self.your)
    }

    /// One tick: notices a manual resize and a change of state, then fits.
    pub fn step(&mut self, o: &Observed) -> Option<Move> {
        if o.size.0 < 1.0 || o.size.1 < 1.0 {
            // The native window doesn't exist yet: its first real size is
            // not a resize.
            return None;
        }
        let arranged = o.arranged || o.placement.is_some_and(|p| p.arranged);
        if let Some(seen) = self.seen
            && ((o.size.0 - seen.0).abs() >= 1.0 || (o.size.1 - seen.1).abs() >= 1.0)
        {
            // The shell only ever changes the height, and only just now.
            let ours = (o.size.0 - seen.0).abs() < 1.0
                && self
                    .own
                    .is_some_and(|t| o.now.saturating_duration_since(t) < OWN_RESIZE_GRACE);
            if !ours && !arranged {
                self.your = at_least_min(o.size);
                self.waiting = true;
                self.save_at = Some(o.now + SAVE_DELAY);
            }
        }
        // Restoring from an arranged state jumps the size back, often to a
        // height fit chose: that isn't yours, so the first normal tick after
        // it starts a new baseline instead.
        self.seen = if arranged { None } else { Some(o.size) };

        if self.state != Some(o.state) {
            self.state = Some(o.state);
            self.pending = true;
            self.waiting = false;
        }

        // Back on the main page, its block has just been built: let its
        // heights settle for a tick before fitting to them.
        let returned = o.main_page && !self.on_main;
        self.on_main = o.main_page;
        if !o.main_page || returned || arranged || self.waiting {
            return None;
        }
        let p = o.placement.as_ref()?;
        if o.needs.min <= 0.0 {
            // The page hasn't published its heights yet.
            return None;
        }
        let changed = std::mem::take(&mut self.pending);
        let mv = fit(o.needs, self.your.1, changed, p)?;
        if self.asked == Some((mv.client_h, o.size)) {
            return None;
        }
        self.asked = Some((mv.client_h, o.size));
        self.own = Some(o.now);
        Some(mv)
    }

    /// Your size, rounded for the config, once its save is due.
    pub fn take_save(&mut self, now: Instant) -> Option<(u32, u32)> {
        match self.save_at {
            Some(at) if now >= at => {
                self.save_at = None;
                Some((self.your.0.round() as u32, self.your.1.round() as u32))
            }
            _ => None,
        }
    }
}

// ----- your position (D18) -----

/// How much of the title bar must lie on a screen for a remembered
/// position to be kept: its height, enough to grab it by (D18).
pub const GRAB_H: f32 = 32.0;

/// Where a window whose frame's top-left was last at `saved` reopens
/// (D18), given every monitor's work area, all in the platform's one
/// coordinate space; `width` is the frame's width there. `None` — the
/// system places it — when no work area holds any of its title bar: the
/// monitor it was on is gone or has moved. Otherwise the frame is pulled
/// in to lie wholly across that work area, and its top inside it, so the
/// title bar is never under a taskbar or off an edge. The bottom is left
/// to window fit's launch clamp (D13).
pub fn launch_position(saved: (f32, f32), width: f32, works: &[Rect]) -> Option<(f32, f32)> {
    let bar = Rect::new(saved.0, saved.1, width.max(1.0), GRAB_H);
    let overlap = |w: &Rect| {
        let dx = (bar.x + bar.w).min(w.x + w.w) - bar.x.max(w.x);
        let dy = bar.bottom().min(w.bottom()) - bar.y.max(w.y);
        if dx > 0.0 && dy > 0.0 { dx * dy } else { 0.0 }
    };
    let work = works
        .iter()
        .map(|w| (overlap(w), w))
        .filter(|(a, _)| *a > 0.0)
        .max_by(|a, b| a.0.total_cmp(&b.0))?
        .1;
    let x = saved.0.min(work.x + work.w - bar.w).max(work.x);
    let y = saved.1.min(work.bottom() - GRAB_H).max(work.y);
    Some((x, y))
}

/// Your position across ticks (D18): the last one the platform reported,
/// saved [`SAVE_DELAY`] after it stops changing, and at quit.
#[derive(Clone, Debug, Default)]
pub struct PositionMemory {
    stored: Option<(i32, i32)>,
    seen: Option<(i32, i32)>,
    save_at: Option<Instant>,
}

impl PositionMemory {
    /// From the stored `windowPosition`: `None` = never saved.
    pub fn new(stored: Option<(i32, i32)>) -> Self {
        PositionMemory {
            stored,
            ..Default::default()
        }
    }

    /// One tick: where the window would restore to now, or `None` while
    /// the platform can't say (no window yet, or a platform that never
    /// knows), which changes nothing.
    pub fn observe(&mut self, pos: Option<(i32, i32)>, now: Instant) {
        let Some(pos) = pos else {
            return;
        };
        if self.seen != Some(pos) {
            self.seen = Some(pos);
            self.save_at = (Some(pos) != self.stored).then_some(now + SAVE_DELAY);
        }
    }

    /// The position to store, once its save is due.
    pub fn take_save(&mut self, now: Instant) -> Option<(i32, i32)> {
        match self.save_at {
            Some(at) if now >= at => {
                self.save_at = None;
                let pos = self.seen.filter(|p| Some(*p) != self.stored)?;
                self.stored = Some(pos);
                Some(pos)
            }
            _ => None,
        }
    }
}

// ----- geometry helpers for the platforms (D15) -----

/// Physical px to logical, by the window's scale factor.
pub fn to_logical(r: Rect, scale: f32) -> Rect {
    let s = if scale > 0.0 { scale } else { 1.0 };
    Rect::new(r.x / s, r.y / s, r.w / s, r.h / s)
}

/// An AppKit rectangle (bottom-left origin, y up from the primary screen's
/// bottom edge) to a top-left one, given the primary screen's height.
pub fn from_appkit(r: Rect, primary_h: f32) -> Rect {
    Rect::new(r.x, primary_h - (r.y + r.h), r.w, r.h)
}

/// `GetWindowPlacement`'s `rcNormalPosition` is in workspace coordinates,
/// whose (0, 0) is the top-left of the PRIMARY monitor's work area, not of
/// the screen. Shifts it into screen coordinates.
pub fn workspace_to_screen(r: Rect, primary_work_origin: (f32, f32)) -> Rect {
    Rect::new(
        r.x + primary_work_origin.0,
        r.y + primary_work_origin.1,
        r.w,
        r.h,
    )
}

/// A window that isn't maximized but whose rectangle isn't its restore
/// rectangle has been arranged by the system: Windows Snap (§4.1's fallback
/// where `IsWindowArranged` is missing). Both in screen coordinates.
pub fn differs_from_restore(window: Rect, restore: Rect) -> bool {
    (window.x - restore.x).abs() > 2.0
        || (window.y - restore.y).abs() > 2.0
        || (window.w - restore.w).abs() > 2.0
        || (window.h - restore.h).abs() > 2.0
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A 1080p monitor at 100 % with a 48 px taskbar: work area 1032 tall.
    const WORK: Rect = Rect {
        x: 0.0,
        y: 0.0,
        w: 1920.0,
        h: 1032.0,
    };
    /// The title bar.
    const DECO: f32 = 32.0;

    fn placed(y: f32, client_h: f32) -> Placement {
        Placement {
            frame: Rect::new(400.0, y, 520.0, client_h + DECO),
            client_h,
            work: WORK,
            positioned: true,
            arranged: false,
        }
    }

    fn needs(min: f32, full: f32) -> Needs {
        Needs { min, full }
    }

    // ----- fit(): D8 -----

    #[test]
    fn a_page_that_fits_with_the_elastic_at_its_minimum_doesnt_grow() {
        // Live alone at 800: the preview shrinks to 213 and nothing scrolls.
        assert_eq!(
            fit(needs(747.0, 835.0), 800.0, false, &placed(60.0, 800.0)),
            None
        );
    }

    #[test]
    fn a_page_that_cannot_fit_grows_to_its_full_height() {
        // A room of three: ≈ 999 even with a 160 px preview.
        let mv = fit(needs(999.0, 1087.0), 800.0, true, &placed(-32.0, 800.0)).unwrap();
        assert_eq!(mv.client_h, 1000.0, "capped: 1032 less the title bar");
        assert_eq!(mv.reason, Reason::Grow);
    }

    #[test]
    fn growth_is_capped_at_the_work_area_less_the_frame() {
        let mut p = placed(0.0, 800.0);
        p.work = Rect::new(0.0, 0.0, 2560.0, 1392.0);
        let mv = fit(needs(999.0, 1087.0), 800.0, true, &p).unwrap();
        assert_eq!(mv.client_h, 1087.0, "room enough: the full height");
        let mv = fit(needs(1600.0, 1700.0), 800.0, true, &p).unwrap();
        assert_eq!(mv.client_h, 1392.0 - DECO);
    }

    #[test]
    fn the_top_edge_stays_when_the_growth_fits_below() {
        let mut p = placed(20.0, 800.0);
        p.work = Rect::new(0.0, 0.0, 2560.0, 1392.0);
        let mv = fit(needs(999.0, 1087.0), 800.0, true, &p).unwrap();
        assert_eq!(mv.y, None);
    }

    #[test]
    fn the_window_moves_up_only_as_far_as_its_bottom_needs() {
        // A room the user created, window sitting low (canvas, panel 2).
        let mv = fit(needs(895.0, 983.0), 800.0, true, &placed(150.0, 800.0)).unwrap();
        assert_eq!(mv.client_h, 983.0);
        assert_eq!(mv.y, Some(1032.0 - DECO - 983.0));
    }

    #[test]
    fn the_window_never_moves_above_the_work_areas_top() {
        let mut p = placed(300.0, 800.0);
        p.work = Rect::new(0.0, 25.0, 1440.0, 875.0); // a Mac: the menu bar above
        let mv = fit(needs(1200.0, 1300.0), 800.0, true, &p).unwrap();
        assert_eq!(mv.client_h, 875.0 - DECO);
        assert_eq!(mv.y, Some(25.0));
    }

    #[test]
    fn nothing_shrinks_within_a_state() {
        // Grown to 1000 in a room; a stream leaves and the page needs less.
        assert_eq!(
            fit(needs(947.0, 1035.0), 800.0, false, &placed(0.0, 1000.0)),
            None
        );
        assert_eq!(
            fit(needs(747.0, 835.0), 800.0, false, &placed(0.0, 1000.0)),
            None
        );
    }

    #[test]
    fn a_change_of_state_goes_back_to_your_size() {
        // Left the room: Live alone fits at 800 again.
        let mv = fit(needs(747.0, 835.0), 800.0, true, &placed(0.0, 1000.0)).unwrap();
        assert_eq!(mv.client_h, 800.0);
        assert_eq!(mv.reason, Reason::Shrink);
        assert_eq!(mv.y, None);
    }

    #[test]
    fn a_change_of_state_never_ends_below_your_size() {
        // Your size is 900; the page would fit in 750.
        let mv = fit(needs(750.0, 800.0), 900.0, true, &placed(0.0, 1000.0)).unwrap();
        assert_eq!(mv.client_h, 900.0);
    }

    #[test]
    fn a_change_of_state_that_still_needs_more_keeps_growing_from_your_size() {
        // Your size 700, joining a room: D8 from a 700 floor.
        let mv = fit(needs(999.0, 1087.0), 700.0, true, &placed(-32.0, 700.0)).unwrap();
        assert_eq!(mv.client_h, 1000.0);
    }

    #[test]
    fn a_window_taller_than_the_screen_is_clamped() {
        // The launch on a 1080p laptop at 150 %: 672 px of work area.
        let mut p = placed(0.0, 800.0);
        p.work = Rect::new(0.0, 0.0, 1280.0, 672.0);
        let mv = fit(needs(593.0, 623.0), 800.0, true, &p).unwrap();
        assert_eq!(mv.client_h, 672.0 - DECO);
        assert_eq!(mv.reason, Reason::Clamp);
    }

    #[test]
    fn the_window_never_goes_below_its_minimum() {
        let mut p = placed(0.0, 600.0);
        p.work = Rect::new(0.0, 0.0, 1024.0, 560.0);
        assert_eq!(fit(needs(800.0, 900.0), 600.0, true, &p), None);
    }

    #[test]
    fn hands_off_an_arranged_window() {
        let mut p = placed(0.0, 800.0);
        p.arranged = true;
        assert_eq!(fit(needs(999.0, 1087.0), 800.0, true, &p), None);
    }

    #[test]
    fn without_a_position_the_window_never_grows_or_moves() {
        let mut p = placed(0.0, 800.0);
        p.positioned = false;
        assert_eq!(fit(needs(999.0, 1087.0), 800.0, true, &p), None);
        // But the launch clamp still applies (D14).
        p.work = Rect::new(0.0, 0.0, 1280.0, 656.0);
        let mv = fit(needs(593.0, 623.0), 800.0, true, &p).unwrap();
        assert_eq!(mv.client_h, 656.0 - DECO);
        assert_eq!(mv.y, None);
    }

    // ----- FitState: D9–D11 across ticks -----

    struct Ticker {
        fit: FitState,
        t0: Instant,
        ms: u64,
        size: (f32, f32),
        y: f32,
        main_page: bool,
        state: (bool, bool),
        needs: Needs,
        arranged: bool,
        work: Rect,
        /// The window manager does as asked; false stands for one that
        /// ignores the request (a tiled window).
        apply: bool,
    }

    impl Ticker {
        fn new(stored: (u32, u32)) -> Self {
            let fit = FitState::new(stored);
            let size = fit.launch_size();
            Ticker {
                fit,
                t0: Instant::now(),
                ms: 0,
                size,
                y: 0.0,
                main_page: true,
                state: (false, false),
                needs: needs(593.0, 623.0),
                arranged: false,
                work: Rect::new(0.0, 0.0, 2560.0, 1392.0),
                apply: true,
            }
        }

        /// One 250 ms tick; a move is applied to the window at once.
        fn tick(&mut self) -> Option<Move> {
            self.ms += 250;
            let now = self.t0 + Duration::from_millis(self.ms);
            let o = Observed {
                size: self.size,
                main_page: self.main_page,
                state: self.state,
                needs: self.needs,
                placement: Some(Placement {
                    frame: Rect::new(100.0, self.y, self.size.0, self.size.1 + DECO),
                    client_h: self.size.1,
                    work: self.work,
                    positioned: true,
                    arranged: false,
                }),
                arranged: self.arranged,
                now,
            };
            let mv = self.fit.step(&o);
            if let Some(m) = mv.filter(|_| self.apply) {
                self.size.1 = m.client_h;
                if let Some(y) = m.y {
                    self.y = y;
                }
            }
            mv
        }

        fn now(&self) -> Instant {
            self.t0 + Duration::from_millis(self.ms)
        }
    }

    #[test]
    fn a_window_never_resized_opens_at_the_default_size() {
        assert_eq!(FitState::new((0, 0)).launch_size(), DEFAULT_SIZE);
        assert_eq!(FitState::new((0, 700)).launch_size(), DEFAULT_SIZE);
    }

    #[test]
    fn a_stored_size_is_raised_to_the_minimum() {
        assert_eq!(FitState::new((300, 300)).launch_size(), MIN_SIZE);
        assert_eq!(FitState::new((560, 720)).launch_size(), (560.0, 720.0));
    }

    #[test]
    fn the_first_tick_clamps_a_remembered_size_to_a_short_screen() {
        let mut t = Ticker::new((520, 1200));
        t.work = Rect::new(0.0, 0.0, 1280.0, 672.0);
        let mv = t.tick().unwrap();
        assert_eq!(mv.client_h, 672.0 - DECO);
        // Your size is what you chose, not the clamp: a bigger screen next
        // time gets it back.
        assert_eq!(t.fit.your(), (520.0, 1200.0));
        assert_eq!(t.fit.take_save(t.now() + SAVE_DELAY), None);
    }

    #[test]
    fn joining_a_room_grows_and_leaving_it_comes_back() {
        let mut t = Ticker::new((0, 0));
        assert_eq!(t.tick(), None, "Ready fits at 800");
        t.state = (true, false);
        t.needs = needs(747.0, 835.0);
        assert_eq!(t.tick(), None, "Live alone fits at 800");
        t.state = (true, true);
        t.needs = needs(999.0, 1087.0);
        assert_eq!(t.tick().unwrap().client_h, 1087.0);
        assert_eq!(
            t.tick(),
            None,
            "settled: the shell's own resize isn't yours"
        );
        assert!(!t.fit.waiting());
        t.state = (true, false);
        t.needs = needs(747.0, 835.0);
        assert_eq!(t.tick().unwrap().client_h, 800.0);
    }

    #[test]
    fn streams_coming_and_going_never_shrink_the_window() {
        let mut t = Ticker::new((0, 0));
        t.state = (true, true);
        t.needs = needs(999.0, 1087.0);
        t.tick();
        t.needs = needs(1051.0, 1139.0); // a fourth stream: the preview gives
        assert_eq!(t.tick(), None);
        t.needs = needs(1155.0, 1243.0); // two more: grows again
        assert_eq!(t.tick().unwrap().client_h, 1243.0);
        t.needs = needs(1103.0, 1191.0); // one leaves
        assert_eq!(t.tick(), None);
        assert_eq!(t.size.1, 1243.0);
    }

    #[test]
    fn a_manual_resize_becomes_your_size_and_fit_waits() {
        let mut t = Ticker::new((0, 0));
        t.state = (true, false);
        t.needs = needs(747.0, 835.0);
        t.tick();
        t.size = (520.0, 700.0); // you drag it shorter
        assert_eq!(t.tick(), None);
        assert!(t.fit.waiting());
        assert_eq!(t.fit.your(), (520.0, 700.0));
        // A notice appears: the page needs more, but fit waits.
        t.needs = needs(799.0, 887.0);
        assert_eq!(t.tick(), None);
        // The next change of state wakes it, from your new size.
        t.state = (true, true);
        t.needs = needs(999.0, 1087.0);
        assert_eq!(t.tick().unwrap().client_h, 1087.0);
        assert!(!t.fit.waiting());
    }

    #[test]
    fn your_size_is_saved_a_second_after_the_last_manual_resize() {
        let mut t = Ticker::new((0, 0));
        t.tick();
        t.size = (600.0, 700.0);
        t.tick();
        assert_eq!(t.fit.take_save(t.now()), None, "not yet");
        t.size = (610.0, 720.0); // still dragging
        t.tick();
        assert_eq!(t.fit.take_save(t.now() + Duration::from_millis(900)), None);
        assert_eq!(t.fit.take_save(t.now() + SAVE_DELAY), Some((610, 720)));
        assert_eq!(t.fit.take_save(t.now() + SAVE_DELAY * 2), None, "once");
    }

    #[test]
    fn a_resize_long_after_the_shells_own_is_the_users() {
        let mut t = Ticker::new((0, 0));
        t.state = (true, true);
        t.needs = needs(999.0, 1087.0);
        t.tick(); // grows to 1087
        t.tick();
        t.tick();
        t.size = (520.0, 1000.0);
        t.tick();
        assert!(t.fit.waiting());
        assert_eq!(t.fit.your(), (520.0, 1000.0));
    }

    #[test]
    fn a_maximized_window_is_left_alone_and_its_size_isnt_yours() {
        let mut t = Ticker::new((0, 0));
        t.tick();
        t.arranged = true;
        t.size = (1920.0, 1000.0);
        t.state = (true, true);
        t.needs = needs(1200.0, 1300.0);
        assert_eq!(t.tick(), None);
        assert_eq!(t.fit.your(), DEFAULT_SIZE);
        assert!(!t.fit.waiting());
    }

    // Review of #433: a window manager that ignores the request (a tiled
    // window, which on Linux nothing reports) must not get the same move
    // every tick for the rest of the session.
    #[test]
    fn a_move_the_window_manager_ignores_is_not_asked_again() {
        let mut t = Ticker::new((960, 1050));
        t.work = Rect::new(0.0, 0.0, 1920.0, 1016.0);
        t.apply = false;
        assert_eq!(t.tick().unwrap().reason, Reason::Clamp);
        for _ in 0..8 {
            assert_eq!(t.tick(), None);
        }
        // A different move is asked for once too: the page needs more
        // after a change of state, and the cap still holds it.
        t.state = (true, true);
        t.needs = needs(1200.0, 1300.0);
        assert_eq!(t.tick(), None, "the same clamp, still ignored");
        // When the size does change, fit may ask again.
        t.apply = true;
        t.size = (960.0, 900.0);
        t.arranged = true; // say the tiling changed it
        t.tick();
        t.arranged = false;
        assert_eq!(t.tick().unwrap().client_h, 1016.0 - DECO);
    }

    // Review of #433: restoring an arranged window jumps its size back to
    // where it was, often a height fit chose. That is not a manual resize:
    // it must not become your size, be saved, or make fit wait.
    #[test]
    fn restoring_a_maximized_window_is_not_a_resize() {
        let mut t = Ticker::new((0, 0));
        t.state = (true, true);
        t.needs = needs(999.0, 1087.0);
        assert_eq!(t.tick().unwrap().client_h, 1087.0);
        t.tick();
        t.tick();
        t.tick(); // long past the shell's own resize
        t.arranged = true;
        t.size = (1920.0, 1000.0);
        assert_eq!(t.tick(), None);
        t.arranged = false;
        t.size = (520.0, 1087.0);
        assert_eq!(t.tick(), None);
        assert!(!t.fit.waiting());
        assert_eq!(t.fit.your(), DEFAULT_SIZE);
        assert_eq!(t.fit.take_save(t.now() + SAVE_DELAY), None);
        // And leaving the room still goes back to your size.
        t.state = (true, false);
        t.needs = needs(747.0, 835.0);
        assert_eq!(t.tick().unwrap().client_h, 800.0);
    }

    #[test]
    fn navigation_never_resizes_but_a_change_made_elsewhere_lands_on_return() {
        let mut t = Ticker::new((0, 0));
        t.state = (true, true);
        t.needs = needs(999.0, 1087.0);
        t.tick(); // 1087 in a room
        // Open Quality; the room ends while you're there.
        t.main_page = false;
        t.state = (true, false);
        assert_eq!(t.tick(), None);
        // Back on the main page. Its block is new, so the first tick lets
        // its heights settle (a wrong one here would grow the window for
        // nothing); the change applies on the next.
        t.main_page = true;
        t.needs = needs(5000.0, 5000.0);
        assert_eq!(t.tick(), None);
        t.needs = needs(747.0, 835.0);
        assert_eq!(t.tick().unwrap().client_h, 800.0);
        // Away and back with nothing changed: nothing happens.
        t.main_page = false;
        t.tick();
        t.main_page = true;
        assert_eq!(t.tick(), None);
        assert_eq!(t.tick(), None);
    }

    #[test]
    fn a_window_not_created_yet_is_not_a_resize() {
        // The first turn of the event loop can come before the native window
        // has a size; its first real one must not read as the user's.
        let mut t = Ticker::new((520, 1200));
        t.work = Rect::new(0.0, 0.0, 1280.0, 672.0);
        t.size = (0.0, 0.0);
        assert_eq!(t.tick(), None);
        t.size = (520.0, 1200.0);
        assert_eq!(t.tick().unwrap().client_h, 672.0 - DECO);
        assert!(!t.fit.waiting());
    }

    #[test]
    fn nothing_happens_before_the_page_has_published_its_heights() {
        let mut t = Ticker::new((520, 1200));
        t.work = Rect::new(0.0, 0.0, 1280.0, 672.0);
        t.needs = Needs::default();
        assert_eq!(t.tick(), None);
        t.needs = needs(593.0, 623.0);
        assert!(t.tick().is_some(), "the pending launch clamp still applies");
    }

    // ----- geometry helpers -----

    #[test]
    fn physical_px_become_logical_by_the_scale_factor() {
        let r = to_logical(Rect::new(300.0, 150.0, 780.0, 1200.0), 1.5);
        assert_eq!(r, Rect::new(200.0, 100.0, 520.0, 800.0));
        let r = to_logical(Rect::new(125.0, 0.0, 650.0, 1290.0), 1.25);
        assert_eq!(r, Rect::new(100.0, 0.0, 520.0, 1032.0));
        assert_eq!(
            to_logical(Rect::new(1.0, 2.0, 3.0, 4.0), 0.0),
            Rect::new(1.0, 2.0, 3.0, 4.0)
        );
    }

    #[test]
    fn appkit_rectangles_flip_to_a_top_left_origin() {
        // Primary 1440 × 900, menu bar 25, Dock 70: visibleFrame is
        // (0, 70, 1440, 805) in AppKit's coordinates.
        let work = from_appkit(Rect::new(0.0, 70.0, 1440.0, 805.0), 900.0);
        assert_eq!(work, Rect::new(0.0, 25.0, 1440.0, 805.0));
        assert_eq!(work.bottom(), 830.0);
        // A second screen above the primary: negative y, top-left.
        let above = from_appkit(Rect::new(0.0, 900.0, 1920.0, 1080.0), 900.0);
        assert_eq!(above, Rect::new(0.0, -1080.0, 1920.0, 1080.0));
        // A window 800 tall whose bottom is 100 up from the primary's bottom.
        let win = from_appkit(Rect::new(200.0, 100.0, 520.0, 800.0), 900.0);
        assert_eq!(win.y, 0.0);
    }

    // ----- launch_position() and PositionMemory: D18 -----

    /// Two 1080p monitors side by side, taskbars 48 px at the bottom.
    const LEFT: Rect = WORK;
    const RIGHT: Rect = Rect {
        x: 1920.0,
        y: 0.0,
        w: 1920.0,
        h: 1032.0,
    };

    #[test]
    fn a_position_on_a_screen_is_kept() {
        assert_eq!(
            launch_position((400.0, 100.0), 520.0, &[LEFT, RIGHT]),
            Some((400.0, 100.0))
        );
        assert_eq!(
            launch_position((2400.0, 50.0), 520.0, &[LEFT, RIGHT]),
            Some((2400.0, 50.0))
        );
    }

    #[test]
    fn a_position_on_a_monitor_that_is_gone_is_left_to_the_system() {
        assert_eq!(launch_position((2400.0, 50.0), 520.0, &[LEFT]), None);
        // Above every screen, and with no screens at all.
        assert_eq!(launch_position((400.0, -500.0), 520.0, &[LEFT]), None);
        assert_eq!(launch_position((400.0, 100.0), 520.0, &[]), None);
    }

    #[test]
    fn a_window_partly_off_its_screen_is_pulled_in() {
        // Hanging off the right edge of the only screen.
        assert_eq!(
            launch_position((1700.0, 100.0), 520.0, &[LEFT]),
            Some((1400.0, 100.0))
        );
        // Title bar under the taskbar: its top goes up to grab height.
        assert_eq!(
            launch_position((400.0, 1020.0), 520.0, &[LEFT]),
            Some((400.0, 1000.0))
        );
        // Wider than the screen: the left edge wins.
        assert_eq!(
            launch_position((100.0, 100.0), 2500.0, &[LEFT]),
            Some((0.0, 100.0))
        );
    }

    #[test]
    fn a_window_across_two_screens_goes_to_the_one_with_more_of_its_title_bar() {
        // 400 px on the left screen, 120 on the right.
        assert_eq!(
            launch_position((1520.0, 100.0), 520.0, &[LEFT, RIGHT]),
            Some((1400.0, 100.0))
        );
        assert_eq!(
            launch_position((1800.0, 100.0), 520.0, &[LEFT, RIGHT]),
            Some((1920.0, 100.0))
        );
    }

    #[test]
    fn a_screen_left_of_the_primary_has_negative_coordinates() {
        let left = Rect::new(-2560.0, -200.0, 2560.0, 1392.0);
        assert_eq!(
            launch_position((-1200.0, -150.0), 520.0, &[left, WORK]),
            Some((-1200.0, -150.0))
        );
    }

    #[test]
    fn a_position_is_saved_a_second_after_it_settles() {
        let t0 = Instant::now();
        let mut m = PositionMemory::new(None);
        m.observe(None, t0);
        assert_eq!(m.take_save(t0 + SAVE_DELAY), None);
        m.observe(Some((10, 20)), t0);
        m.observe(Some((30, 40)), t0 + Duration::from_millis(500));
        assert_eq!(m.take_save(t0 + SAVE_DELAY), None, "still moving");
        // The same position again doesn't push the save back.
        m.observe(Some((30, 40)), t0 + Duration::from_millis(1000));
        assert_eq!(
            m.take_save(t0 + Duration::from_millis(1500)),
            Some((30, 40))
        );
        assert_eq!(m.take_save(t0 + Duration::from_secs(5)), None, "once");
    }

    #[test]
    fn the_stored_position_is_not_saved_again() {
        let t0 = Instant::now();
        let mut m = PositionMemory::new(Some((30, 40)));
        m.observe(Some((30, 40)), t0);
        assert_eq!(m.take_save(t0 + SAVE_DELAY), None);
        // Moved away and back before the save was due.
        m.observe(Some((50, 60)), t0);
        m.observe(Some((30, 40)), t0 + Duration::from_millis(100));
        assert_eq!(m.take_save(t0 + Duration::from_secs(3)), None);
    }

    #[test]
    fn a_quit_saves_a_move_not_yet_due() {
        let t0 = Instant::now();
        let mut m = PositionMemory::new(Some((0, 0)));
        m.observe(Some((70, 80)), t0);
        // The shell's quit asks as if the delay had passed.
        assert_eq!(m.take_save(t0 + SAVE_DELAY), Some((70, 80)));
    }

    #[test]
    fn workspace_coordinates_shift_by_the_primary_work_areas_origin() {
        // A taskbar on the left of the primary, 48 px wide.
        let r = workspace_to_screen(Rect::new(100.0, 50.0, 520.0, 800.0), (48.0, 0.0));
        assert_eq!(r, Rect::new(148.0, 50.0, 520.0, 800.0));
    }

    #[test]
    fn a_window_away_from_its_restore_rectangle_is_snapped() {
        let restore = Rect::new(148.0, 50.0, 520.0, 800.0);
        assert!(!differs_from_restore(
            Rect::new(149.0, 51.0, 520.0, 800.0),
            restore
        ));
        assert!(differs_from_restore(
            Rect::new(0.0, 0.0, 960.0, 1032.0),
            restore
        ));
    }
}
