//! The video half of a Linux broadcast (docs/58 D4–D6): the encoder cascade
//! walked against the capture ladder, the live start as the final probe, and
//! the mid-session rebuild on the held portal grant.
//!
//! The walk is written against [`Launcher`] so its rules — last-good first,
//! trial before live, every rung per encoder, the pipewiresrc-only
//! diagnosis, the pin's single candidate, the rebuild's rate limit — are unit
//! tests with scripted outcomes, as `cascade::choose` is on the other two
//! platforms. [`GstLauncher`] is the real one.

use gawk_encode::gst::BusError;
use gawk_encode::gst_policy::{self, Candidate, CaptureRung, Culprit, RUNGS, RebuildLimiter};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError};
use std::time::{Duration, Instant};

/// One running attempt.
pub trait Running: Send {
    fn force_idr(&self);
    /// Synchronous: nothing from this attempt arrives after it returns.
    fn stop(self: Box<Self>);
}

/// Starts things. `attempt` tags every error the attempt later reports, so a
/// late error from a stopped attempt can never be pinned on its successor.
pub trait Launcher: Send {
    /// The trial gate for one candidate (D5): `Ok` means it passed the shared
    /// validator.
    fn trial(&mut self, c: Candidate) -> Result<(), String>;
    fn launch(
        &mut self,
        c: Candidate,
        rung: CaptureRung,
        attempt: u64,
    ) -> Result<Box<dyn Running>, String>;
}

/// An error from a running attempt.
pub type AttemptError = (u64, BusError);

/// Why a walk ended without a pipeline.
#[derive(Debug, PartialEq, Eq)]
pub enum WalkError {
    /// Nothing encodes: the refusal (G4). The trail is for the debug log.
    NoHardwareEncoder(Vec<String>),
    /// Every live attempt died inside `pipewiresrc`: the encoders are
    /// innocent, and the diagnosis is the capture-format sentence.
    CaptureFormat(Vec<String>),
    /// The pinned encoder alone failed (the user asked for exactly it).
    PinnedFailed(String, Vec<String>),
    /// Stop landed mid-walk.
    Stopped,
}

/// What a walk adopted.
pub struct Adopted {
    pub candidate: Candidate,
    pub rung: CaptureRung,
    pub running: Box<dyn Running>,
    pub attempt: u64,
}

/// The walk's inputs.
pub struct Walk<'a> {
    /// The cascade order: the pin alone, or all three.
    pub order: &'a [Candidate],
    /// Re-verified first (never trusted), when it is in `order`.
    pub last_good: Option<Candidate>,
    /// A rebuild's encoder was working a moment ago: it skips the trial.
    pub trusted: Option<Candidate>,
    pub pinned: bool,
    pub probe: Duration,
}

/// Walks encoders × capture rungs once and adopts the first attempt that
/// survives its probe window.
pub fn walk(
    launcher: &mut dyn Launcher,
    errors: &Receiver<AttemptError>,
    next_attempt: &mut u64,
    stopped: &dyn Fn() -> bool,
    w: Walk<'_>,
) -> Result<Adopted, WalkError> {
    let mut order: Vec<Candidate> = Vec::with_capacity(w.order.len());
    for first in [w.trusted, w.last_good].into_iter().flatten() {
        if w.order.contains(&first) && !order.contains(&first) {
            order.push(first);
        }
    }
    order.extend(
        w.order
            .iter()
            .filter(|c| !order.contains(c))
            .copied()
            .collect::<Vec<_>>(),
    );

    let mut trail = Vec::new();
    let mut live_failures = Vec::new();
    for c in order {
        if stopped() {
            return Err(WalkError::Stopped);
        }
        if Some(c) != w.trusted {
            log::info!(
                "trial: {}{}",
                c.element(),
                if Some(c) == w.last_good {
                    " (last-good, re-verified first)"
                } else {
                    ""
                }
            );
            if let Err(why) = launcher.trial(c) {
                log::warn!("rejected: {}: {why}", c.element());
                trail.push(format!("{}: {why}", c.element()));
                continue;
            }
        }
        for rung in RUNGS {
            if stopped() {
                return Err(WalkError::Stopped);
            }
            *next_attempt += 1;
            let attempt = *next_attempt;
            // Anything still queued belongs to an attempt already stopped.
            while errors.try_recv().is_ok() {}
            let running = match launcher.launch(c, rung, attempt) {
                Ok(r) => r,
                Err(why) => {
                    let line = format!("{} (capture {}): {why}", c.element(), rung.label());
                    log::warn!("live pipeline failed to start: {line}");
                    live_failures.push(line.clone());
                    trail.push(line);
                    continue;
                }
            };
            match probe(errors, attempt, w.probe) {
                None => {
                    log::info!(
                        "encoding in hardware: {} ({}), capture {}",
                        c.element(),
                        c.api(),
                        rung.label()
                    );
                    return Ok(Adopted {
                        candidate: c,
                        rung,
                        running,
                        attempt,
                    });
                }
                Some(err) => {
                    running.stop();
                    let line = format!("{} (capture {}): {}", c.element(), rung.label(), err.text);
                    log::warn!("live pipeline died inside its probe window: {line}");
                    live_failures.push(line.clone());
                    trail.push(line);
                }
            }
        }
    }
    if gst_policy::all_inside_pipewiresrc(&live_failures) {
        return Err(WalkError::CaptureFormat(trail));
    }
    if w.pinned
        && let Some(c) = w.order.first()
    {
        return Err(WalkError::PinnedFailed(c.element().into(), trail));
    }
    Err(WalkError::NoHardwareEncoder(trail))
}

/// Waits out the probe window: `Some` is the attempt's error inside it.
fn probe(errors: &Receiver<AttemptError>, attempt: u64, window: Duration) -> Option<BusError> {
    let deadline = Instant::now() + window;
    loop {
        let left = deadline.saturating_duration_since(Instant::now());
        match errors.recv_timeout(left) {
            Ok((a, err)) if a == attempt && err.culprit != Culprit::Other => return Some(err),
            Ok((a, err)) => {
                if a == attempt {
                    log::warn!("ignored during probe: {}", err.text);
                }
            }
            Err(RecvTimeoutError::Timeout) => return None,
            Err(RecvTimeoutError::Disconnected) => return None,
        }
        if left.is_zero() {
            return None;
        }
    }
}

/// What the supervisor tells the rest of the pipeline.
pub trait Report: Send {
    /// A rebuild completed (D6's `captureRestarts`).
    fn rebuilt(&self, candidate: Candidate, rung: CaptureRung);
    /// The broadcast must end with this sentence.
    fn failed(&self, text: String);
}

/// Owns the adopted attempt after start: rebuilds on capture errors, ends
/// the broadcast on encode errors or a rebuild budget spent.
pub struct Supervisor {
    stop_tx: mpsc::Sender<Command>,
    /// Read by a rebuild's walk between attempts, so Stop never waits out a
    /// whole cascade (the Go app's `aborted` check).
    stopping: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}

enum Command {
    ForceIdr,
    Stop,
}

pub struct SupervisorParams {
    pub order: Vec<Candidate>,
    pub pinned: bool,
    pub probe: Duration,
    pub budget: usize,
}

impl Supervisor {
    pub fn start(
        mut launcher: Box<dyn Launcher>,
        errors: Receiver<AttemptError>,
        adopted: Adopted,
        params: SupervisorParams,
        report: Box<dyn Report>,
        now_us: Box<dyn Fn() -> u64 + Send>,
    ) -> Self {
        let (stop_tx, commands) = mpsc::channel();
        let stopping = Arc::new(AtomicBool::new(false));
        let stop_flag = stopping.clone();
        let thread = std::thread::Builder::new()
            .name("video-supervisor".into())
            .spawn(move || {
                let mut current = Some(adopted);
                let mut limiter = RebuildLimiter::new(params.budget);
                let mut next_attempt = current.as_ref().map_or(0, |a| a.attempt);
                'outer: loop {
                    match commands.try_recv() {
                        Ok(Command::Stop) | Err(mpsc::TryRecvError::Disconnected) => break,
                        Ok(Command::ForceIdr) => {
                            if let Some(a) = &current {
                                a.running.force_idr();
                            }
                            continue;
                        }
                        Err(mpsc::TryRecvError::Empty) => {}
                    }
                    let (attempt, err) = match errors.recv_timeout(Duration::from_millis(50)) {
                        Ok(e) => e,
                        Err(RecvTimeoutError::Timeout) => continue,
                        Err(RecvTimeoutError::Disconnected) => break,
                    };
                    let Some(cur) = &current else { break };
                    if attempt != cur.attempt {
                        continue; // a stopped attempt's late error
                    }
                    match err.culprit {
                        Culprit::Other => {
                            log::warn!("pipeline warning-level error ignored: {}", err.text);
                        }
                        Culprit::Encode => {
                            log::error!("encoder failed mid-broadcast: {}", err.text);
                            report.failed(format!("The hardware encoder stopped: {}", err.text));
                            break;
                        }
                        Culprit::Capture => {
                            if !limiter.admit(now_us()) {
                                log::error!(
                                    "capture is dying faster than it can be rebuilt; ending the broadcast: {}",
                                    err.text
                                );
                                report.failed(format!(
                                    "Screen capture kept failing and could not be rebuilt: {}",
                                    err.text
                                ));
                                break;
                            }
                            log::warn!(
                                "capture died mid-broadcast; rebuilding on the same portal grant: {}",
                                err.text
                            );
                            let old = current.take().expect("checked above");
                            let trusted = old.candidate;
                            old.running.stop();
                            let stopped = || stop_flag.load(Ordering::Acquire);
                            let result = walk(
                                &mut *launcher,
                                &errors,
                                &mut next_attempt,
                                &stopped,
                                Walk {
                                    order: &params.order,
                                    last_good: None,
                                    trusted: Some(trusted),
                                    pinned: params.pinned,
                                    probe: params.probe,
                                },
                            );
                            match result {
                                Ok(a) => {
                                    report.rebuilt(a.candidate, a.rung);
                                    current = Some(a);
                                }
                                Err(WalkError::Stopped) => break 'outer,
                                Err(e) => {
                                    log::error!("rebuilding capture failed: {e:?}");
                                    report.failed(
                                        "Screen capture stopped and could not be rebuilt.".into(),
                                    );
                                    break 'outer;
                                }
                            }
                        }
                    }
                }
                if let Some(a) = current.take() {
                    a.running.stop();
                }
            })
            .expect("spawn the video supervisor");
        Self {
            stop_tx,
            stopping,
            thread: Some(thread),
        }
    }

    pub fn force_idr(&self) {
        let _ = self.stop_tx.send(Command::ForceIdr);
    }

    /// Stops synchronously: the attempt is NULL when this returns.
    pub fn stop(mut self) {
        self.stopping.store(true, Ordering::Release);
        let _ = self.stop_tx.send(Command::Stop);
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}

impl Drop for Supervisor {
    fn drop(&mut self) {
        self.stopping.store(true, Ordering::Release);
        let _ = self.stop_tx.send(Command::Stop);
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::Mutex;

    type Script = HashMap<(Candidate, CaptureRung), Option<BusError>>;

    /// Launches report their scripted probe error (or none) through the
    /// channel, and log every call.
    struct Scripted {
        trials: HashMap<Candidate, Result<(), String>>,
        live: Script,
        tx: mpsc::Sender<AttemptError>,
        log: Arc<Mutex<Vec<String>>>,
        forced: Arc<Mutex<u32>>,
    }

    struct Run {
        log: Arc<Mutex<Vec<String>>>,
        forced: Arc<Mutex<u32>>,
        name: String,
    }

    impl Running for Run {
        fn force_idr(&self) {
            *self.forced.lock().unwrap() += 1;
        }
        fn stop(self: Box<Self>) {
            self.log.lock().unwrap().push(format!("stop {}", self.name));
        }
    }

    impl Launcher for Scripted {
        fn trial(&mut self, c: Candidate) -> Result<(), String> {
            self.log
                .lock()
                .unwrap()
                .push(format!("trial {}", c.element()));
            self.trials.get(&c).cloned().unwrap_or(Ok(()))
        }
        fn launch(
            &mut self,
            c: Candidate,
            rung: CaptureRung,
            attempt: u64,
        ) -> Result<Box<dyn Running>, String> {
            let name = format!("{}/{}", c.element(), rung.label());
            self.log.lock().unwrap().push(format!("launch {name}"));
            if let Some(Some(err)) = self.live.get(&(c, rung)) {
                self.tx.send((attempt, err.clone())).unwrap();
            }
            Ok(Box::new(Run {
                log: self.log.clone(),
                forced: self.forced.clone(),
                name,
            }))
        }
    }

    fn err(culprit: Culprit, text: &str) -> Option<BusError> {
        Some(BusError {
            culprit,
            text: text.into(),
        })
    }

    fn scripted() -> (Scripted, Receiver<AttemptError>, Arc<Mutex<Vec<String>>>) {
        let (tx, rx) = mpsc::channel();
        let log = Arc::new(Mutex::new(Vec::new()));
        (
            Scripted {
                trials: HashMap::new(),
                live: HashMap::new(),
                tx,
                log: log.clone(),
                forced: Arc::default(),
            },
            rx,
            log,
        )
    }

    fn walk_all(
        l: &mut Scripted,
        rx: &Receiver<AttemptError>,
        w: Walk<'_>,
    ) -> Result<Adopted, WalkError> {
        let mut n = 0;
        walk(l, rx, &mut n, &|| false, w)
    }

    fn cascade(last_good: Option<Candidate>) -> Walk<'static> {
        Walk {
            order: &gst_policy::CASCADE,
            last_good,
            trusted: None,
            pinned: false,
            probe: Duration::from_millis(20),
        }
    }

    #[test]
    fn last_good_is_reverified_first_and_the_first_survivor_is_adopted() {
        let (mut l, rx, log) = scripted();
        l.trials.insert(Candidate::Va, Err("no entrypoint".into()));
        let a = walk_all(&mut l, &rx, cascade(Some(Candidate::Nvenc))).unwrap();
        assert_eq!(a.candidate, Candidate::Nvenc);
        assert_eq!(a.rung, CaptureRung::AutoCapped);
        assert_eq!(
            *log.lock().unwrap(),
            ["trial nvh264enc", "launch nvh264enc/auto-capped"]
        );
    }

    #[test]
    fn a_rung_that_dies_in_its_probe_window_walks_down_the_ladder() {
        let (mut l, rx, log) = scripted();
        l.live.insert(
            (Candidate::Vulkan, CaptureRung::AutoCapped),
            err(Culprit::Capture, "pipewiresrc: not negotiated"),
        );
        l.live.insert(
            (Candidate::Vulkan, CaptureRung::Auto),
            err(Culprit::Capture, "pipewiresrc: unhandled format"),
        );
        let a = walk_all(&mut l, &rx, cascade(None)).unwrap();
        assert_eq!(a.rung, CaptureRung::SystemMemory);
        let log = log.lock().unwrap();
        assert!(log.contains(&"stop vulkanh264enc/auto-capped".to_string()));
        assert!(log.contains(&"stop vulkanh264enc/auto".to_string()));
    }

    #[test]
    fn an_encoder_that_fails_live_on_every_rung_advances_the_cascade() {
        let (mut l, rx, _) = scripted();
        for rung in RUNGS {
            l.live.insert(
                (Candidate::Vulkan, rung),
                err(Culprit::Encode, "vulkanh264enc: device lost"),
            );
        }
        let a = walk_all(&mut l, &rx, cascade(None)).unwrap();
        assert_eq!(a.candidate, Candidate::Nvenc);
    }

    #[test]
    fn nothing_surviving_is_the_refusal_with_the_trail() {
        let (mut l, rx, _) = scripted();
        for c in gst_policy::CASCADE {
            l.trials.insert(c, Err(format!("{} broke", c.element())));
        }
        match walk_all(&mut l, &rx, cascade(None)) {
            Err(WalkError::NoHardwareEncoder(trail)) => assert_eq!(trail.len(), 3),
            other => panic!("{:?}", other.err()),
        }
    }

    #[test]
    fn deaths_only_inside_pipewiresrc_are_a_capture_diagnosis_not_a_refusal() {
        let (mut l, rx, _) = scripted();
        for c in gst_policy::CASCADE {
            for rung in RUNGS {
                l.live.insert(
                    (c, rung),
                    err(Culprit::Capture, "pipewiresrc: unhandled format"),
                );
            }
        }
        assert!(matches!(
            walk_all(&mut l, &rx, cascade(None)),
            Err(WalkError::CaptureFormat(_))
        ));
    }

    #[test]
    fn a_pin_runs_exactly_one_candidate_and_says_so() {
        let (mut l, rx, log) = scripted();
        l.trials.insert(Candidate::Va, Err("no VA driver".into()));
        let w = Walk {
            order: &[Candidate::Va],
            last_good: Some(Candidate::Nvenc),
            trusted: None,
            pinned: true,
            probe: Duration::from_millis(10),
        };
        match walk_all(&mut l, &rx, w) {
            Err(WalkError::PinnedFailed(name, _)) => assert_eq!(name, "vah264enc"),
            other => panic!("{:?}", other.err()),
        }
        assert_eq!(
            *log.lock().unwrap(),
            ["trial vah264enc"],
            "last-good outside the pin is ignored"
        );
    }

    #[test]
    fn a_non_attributable_error_does_not_fail_the_probe() {
        let (mut l, rx, _) = scripted();
        l.live.insert(
            (Candidate::Vulkan, CaptureRung::AutoCapped),
            err(Culprit::Other, "appsink: thumbnail hiccup"),
        );
        let a = walk_all(&mut l, &rx, cascade(None)).unwrap();
        assert_eq!(a.rung, CaptureRung::AutoCapped);
    }

    struct Reports(Arc<Mutex<Vec<String>>>);
    impl Report for Reports {
        fn rebuilt(&self, c: Candidate, rung: CaptureRung) {
            self.0
                .lock()
                .unwrap()
                .push(format!("rebuilt {}/{}", c.element(), rung.label()));
        }
        fn failed(&self, text: String) {
            self.0.lock().unwrap().push(format!("failed {text}"));
        }
    }

    fn wait_for(mut done: impl FnMut() -> bool) {
        let t = Instant::now();
        while !done() {
            assert!(t.elapsed() < Duration::from_secs(5), "timed out");
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    #[test]
    fn a_capture_death_mid_broadcast_rebuilds_without_retrialing_the_working_encoder() {
        let (mut l, rx, log) = scripted();
        let tx = l.tx.clone();
        let mut n = 0;
        let adopted = walk(&mut l, &rx, &mut n, &|| false, cascade(None)).unwrap();
        let first_attempt = adopted.attempt;
        log.lock().unwrap().clear();
        let reports = Arc::new(Mutex::new(Vec::new()));
        let sup = Supervisor::start(
            Box::new(l),
            rx,
            adopted,
            SupervisorParams {
                order: gst_policy::CASCADE.to_vec(),
                pinned: false,
                probe: Duration::from_millis(10),
                budget: 60,
            },
            Box::new(Reports(reports.clone())),
            Box::new(|| 0),
        );
        // A late error from an unrelated attempt is ignored.
        tx.send((first_attempt + 100, err(Culprit::Capture, "old").unwrap()))
            .unwrap();
        tx.send((
            first_attempt,
            err(Culprit::Capture, "pipewiresrc: format changed").unwrap(),
        ))
        .unwrap();
        wait_for(|| !reports.lock().unwrap().is_empty());
        assert_eq!(
            *reports.lock().unwrap(),
            ["rebuilt vulkanh264enc/auto-capped"]
        );
        let l = log.lock().unwrap().clone();
        assert!(!l.iter().any(|e| e.starts_with("trial")), "{l:?}");
        assert_eq!(l[0], "stop vulkanh264enc/auto-capped");
        sup.force_idr();
        sup.stop();
    }

    #[test]
    fn a_rebuild_budget_spent_ends_the_broadcast() {
        let (mut l, rx, _) = scripted();
        let tx = l.tx.clone();
        let mut n = 0;
        let adopted = walk(&mut l, &rx, &mut n, &|| false, cascade(None)).unwrap();
        let attempt = adopted.attempt;
        let reports = Arc::new(Mutex::new(Vec::new()));
        let _sup = Supervisor::start(
            Box::new(l),
            rx,
            adopted,
            SupervisorParams {
                order: gst_policy::CASCADE.to_vec(),
                pinned: false,
                probe: Duration::from_millis(10),
                budget: 0,
            },
            Box::new(Reports(reports.clone())),
            Box::new(|| 0),
        );
        tx.send((attempt, err(Culprit::Capture, "pipewiresrc: gone").unwrap()))
            .unwrap();
        wait_for(|| !reports.lock().unwrap().is_empty());
        assert!(reports.lock().unwrap()[0].starts_with("failed Screen capture kept failing"));
    }

    #[test]
    fn an_encoder_death_mid_broadcast_is_a_session_error() {
        let (mut l, rx, _) = scripted();
        let tx = l.tx.clone();
        let mut n = 0;
        let adopted = walk(&mut l, &rx, &mut n, &|| false, cascade(None)).unwrap();
        let attempt = adopted.attempt;
        let reports = Arc::new(Mutex::new(Vec::new()));
        let _sup = Supervisor::start(
            Box::new(l),
            rx,
            adopted,
            SupervisorParams {
                order: gst_policy::CASCADE.to_vec(),
                pinned: false,
                probe: Duration::from_millis(10),
                budget: 60,
            },
            Box::new(Reports(reports.clone())),
            Box::new(|| 0),
        );
        tx.send((
            attempt,
            err(Culprit::Encode, "vulkanh264enc: device lost").unwrap(),
        ))
        .unwrap();
        wait_for(|| !reports.lock().unwrap().is_empty());
        assert!(reports.lock().unwrap()[0].contains("hardware encoder stopped"));
    }
}
