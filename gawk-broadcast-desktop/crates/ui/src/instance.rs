//! Single instance (R66 OD1, docs/68 D7, D8): the portable half. One
//! per-user endpoint per platform, which is also the lock — whoever owns
//! it is the primary. A second launch, with or without a link, hands over
//! to the primary and exits.
//!
//! The platforms supply an [`Endpoint`]: a named mutex and pipe on Windows,
//! a session D-Bus name on Linux. macOS has none of its own:
//! LaunchServices already sends a second launch to the running bundle.

use std::ffi::OsString;
use std::sync::mpsc;
use std::time::{Duration, Instant};

/// A message on the wire is one line of at most this many bytes.
pub const MAX_MESSAGE: usize = 4096;

/// The primary's answer to every message it accepted.
pub const OK: &str = "ok";

/// The update relaunch's marker (D7): `--relaunched-from <pid>`.
pub const RELAUNCHED_FROM: &str = "--relaunched-from";

/// How long a relaunch waits for the process it replaces to let go.
pub const RELAUNCH_WAIT: Duration = Duration::from_secs(10);

/// What a launch brings: its link, if any.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Request {
    /// No link: bring the window to the front.
    Raise,
    /// A link, still unparsed: the primary treats it as untrusted and runs
    /// it through `gawk_engine::link::parse` like any other. `Err` is a
    /// launch that named more than one link (D7), which the primary reports
    /// as a link it couldn't open.
    Open(Result<String, ()>),
}

/// One request as the primary receives it, with the Wayland activation
/// token the launcher gave the secondary, when there is one (D8).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Incoming {
    pub request: Request,
    pub activation: Option<String>,
}

impl Request {
    /// This launch's request, from its arguments (D7).
    pub fn from_args<S: AsRef<str>>(args: &[S]) -> Request {
        match gawk_engine::link::link_argument(args) {
            None => Request::Raise,
            Some(Ok(link)) => Request::Open(Ok(cut_past_cap(link))),
            Some(Err(())) => Request::Open(Err(())),
        }
    }

    /// The line format (D8): `raise` or `open <link>`, an empty link
    /// standing for a rejected one. No newline; the transport adds its own
    /// framing.
    pub fn encode(&self) -> String {
        match self {
            Request::Raise => "raise".to_owned(),
            Request::Open(Ok(link)) => format!("open {link}"),
            Request::Open(Err(())) => "open ".to_owned(),
        }
    }

    /// Reads one line. `None` for anything over [`MAX_MESSAGE`], not UTF-8,
    /// or not one of the two forms: the primary answers nothing and drops
    /// it. A trailing `\n` or `\r\n` is allowed.
    pub fn decode(line: &[u8]) -> Option<Request> {
        if line.len() > MAX_MESSAGE {
            return None;
        }
        let s = std::str::from_utf8(line).ok()?;
        let s = s
            .strip_suffix('\n')
            .map(|s| s.strip_suffix('\r').unwrap_or(s))
            .unwrap_or(s);
        if s == "raise" {
            return Some(Request::Raise);
        }
        let link = s.strip_prefix("open ")?;
        if link.contains(['\n', '\r']) {
            return None;
        }
        Some(Request::Open(if link.is_empty() {
            Err(())
        } else {
            Ok(link.to_owned())
        }))
    }
}

/// A link over the parser's cap is cut at the first character boundary past
/// it (review of #452). The parser rejects it as too long either way, and
/// the cut keeps `open <link>` under [`MAX_MESSAGE`]. A link that didn't
/// fit failed the handoff on every try, and its launch then ran as a
/// second instance.
fn cut_past_cap(mut link: String) -> String {
    let cap = gawk_engine::link::MAX_LINK_LEN;
    if link.len() > cap {
        let end = (cap + 1..=link.len())
            .find(|&i| link.is_char_boundary(i))
            .unwrap_or(link.len());
        link.truncate(end);
    }
    link
}

/// The `--relaunched-from <pid>` an update relaunch carries (D7).
pub fn relaunched_from<S: AsRef<str>>(args: &[S]) -> Option<u32> {
    let i = args.iter().position(|a| a.as_ref() == RELAUNCHED_FROM)?;
    args.get(i + 1)?.as_ref().parse().ok()
}

/// The arguments an update relaunch passes on (D7): this launch's, minus
/// every link (it was applied once already, G10) and any earlier
/// relaunch marker. The caller appends its own marker.
pub fn relaunch_args(args: &[OsString]) -> Vec<OsString> {
    let mut out = Vec::with_capacity(args.len());
    let mut skip = false;
    for a in args {
        if std::mem::take(&mut skip) {
            continue;
        }
        match a.to_str() {
            Some(RELAUNCHED_FROM) => skip = true,
            Some(s) if gawk_engine::link::is_gawk_link(s) => {}
            _ => out.push(a.clone()),
        }
    }
    out
}

/// A platform's per-user endpoint (D8).
pub trait Endpoint: Send {
    /// Takes the endpoint: `Ok(true)` when this process is now the
    /// primary, `Ok(false)` when another process holds it.
    fn claim(&mut self) -> std::io::Result<bool>;
    /// Hands `request` to the running primary and waits for its
    /// [`OK`]. An error when nothing answers.
    fn send(&mut self, request: &Request) -> std::io::Result<()>;
    /// As the primary: serves the endpoint from now on (a thread of its
    /// own), sending every request it receives to `inbox`.
    fn serve(self: Box<Self>, inbox: mpsc::Sender<Incoming>) -> std::io::Result<()>;
}

/// What a launch does after [`startup`].
#[derive(Debug, PartialEq, Eq)]
pub enum Startup {
    /// The request reached the running primary: exit 0.
    HandedOff,
    /// This process is the primary; serve the endpoint.
    Primary,
    /// No single instance this run (no session bus, a broken endpoint):
    /// run anyway, and say why in the log.
    Alone(String),
}

/// D8's startup rule. Try the running primary first; if nothing answers,
/// claim the endpoint. A lost race (another launch claimed it between the
/// two) retries the send once. An update relaunch (`relaunched`) skips the
/// handoff and waits up to [`RELAUNCH_WAIT`] for the process it replaces to
/// let go, retrying the claim; if it never does, the normal rule applies.
pub fn startup(ep: &mut dyn Endpoint, request: &Request, relaunched: bool) -> Startup {
    startup_with(ep, request, relaunched, RELAUNCH_WAIT, || {
        std::thread::sleep(Duration::from_millis(100))
    })
}

fn startup_with(
    ep: &mut dyn Endpoint,
    request: &Request,
    relaunched: bool,
    wait: Duration,
    mut pause: impl FnMut(),
) -> Startup {
    if relaunched {
        let deadline = Instant::now() + wait;
        loop {
            match ep.claim() {
                Ok(true) => return Startup::Primary,
                Ok(false) if Instant::now() < deadline => pause(),
                Ok(false) => break,
                Err(e) => return Startup::Alone(format!("single instance unavailable: {e}")),
            }
        }
        log::warn!("the replaced process still holds the instance after the wait");
    }
    if ep.send(request).is_ok() {
        return Startup::HandedOff;
    }
    match ep.claim() {
        Ok(true) => Startup::Primary,
        Ok(false) => match ep.send(request) {
            Ok(()) => Startup::HandedOff,
            Err(e) => Startup::Alone(format!(
                "another instance holds the endpoint but does not answer: {e}"
            )),
        },
        Err(e) => Startup::Alone(format!("single instance unavailable: {e}")),
    }
}

/// What the shell gets from `main` (D6): the launch's own request, and the
/// inbox later requests arrive on — from the endpoint, or on macOS from the
/// Apple Event handler.
pub struct Launch {
    pub request: Request,
    pub inbox: Option<mpsc::Receiver<Incoming>>,
    /// Why single instance is off this run. The shell logs it: a windowed
    /// EXE has no stderr.
    pub note: Option<String>,
    /// This launch's own link comes through the inbox, not `request`:
    /// macOS delivers it as an Apple Event once the app runs (D10). The
    /// shell then treats a viewer link that arrives first, right after
    /// launch, as a cold start (D3).
    pub link_in_inbox: bool,
}

impl Launch {
    /// A launch with this request and inbox, nothing to note.
    pub fn new(request: Request, inbox: Option<mpsc::Receiver<Incoming>>) -> Launch {
        Launch {
            request,
            inbox,
            note: None,
            link_in_inbox: false,
        }
    }

    /// A launch with no link and no inbox: a dev run, or a test.
    pub fn plain() -> Launch {
        Launch::new(Request::Raise, None)
    }
}

/// The whole startup for a platform with an endpoint: decide, exit on a
/// handoff, and otherwise serve and return the [`Launch`] for
/// `shell::run`. `args` are the process arguments (`std::env::args`).
pub fn launch(args: &[String], mut ep: Box<dyn Endpoint>) -> Launch {
    let request = Request::from_args(args);
    let relaunched = relaunched_from(args).is_some();
    match startup(&mut *ep, &request, relaunched) {
        Startup::HandedOff => std::process::exit(0),
        Startup::Primary => {
            let (tx, rx) = mpsc::channel();
            match ep.serve(tx) {
                Ok(()) => Launch::new(request, Some(rx)),
                Err(e) => Launch {
                    note: Some(format!(
                        "could not serve the instance endpoint ({e}); running without single instance"
                    )),
                    ..Launch::new(request, None)
                },
            }
        }
        Startup::Alone(why) => Launch {
            note: Some(format!("{why}; running without single instance")),
            ..Launch::new(request, None)
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::rc::Rc;

    #[test]
    fn requests_round_trip_on_the_line_format() {
        for r in [
            Request::Raise,
            Request::Open(Ok("gawk://broadcast?room=abc&nick=J%C3%BCrgen".into())),
            Request::Open(Err(())),
        ] {
            assert_eq!(Request::decode(r.encode().as_bytes()), Some(r.clone()));
            let framed = format!("{}\r\n", r.encode());
            assert_eq!(Request::decode(framed.as_bytes()), Some(r));
        }
    }

    #[test]
    fn a_bad_line_is_dropped() {
        assert_eq!(Request::decode(b"RAISE"), None);
        assert_eq!(Request::decode(b"open"), None);
        assert_eq!(Request::decode(b"quit"), None);
        assert_eq!(Request::decode(b"open gawk://x\nraise"), None);
        assert_eq!(Request::decode(&[b'o', b'p', b'e', b'n', b' ', 0xff]), None);
        let long = format!("open gawk://broadcast?nick={}", "x".repeat(MAX_MESSAGE));
        assert_eq!(Request::decode(long.as_bytes()), None);
    }

    #[test]
    fn the_request_is_the_one_link_argument() {
        assert_eq!(Request::from_args(&["app"]), Request::Raise);
        assert_eq!(
            Request::from_args(&["app", "--flag", "gawk://broadcast?room=abc"]),
            Request::Open(Ok("gawk://broadcast?room=abc".into()))
        );
        assert_eq!(
            Request::from_args(&["app", "gawk://a", "GAWK://b"]),
            Request::Open(Err(()))
        );
    }

    // Review of #452: a link over the parser's cap must still fit one
    // message, or the handoff fails and a second instance starts. It is
    // cut just past the cap, so the primary still says "too long".
    #[test]
    fn an_oversized_link_still_fits_one_message_and_stays_too_long() {
        use gawk_engine::link::{self, LinkError, MAX_LINK_LEN};
        for filler in ["x", "é", "€"] {
            let long = format!("gawk://broadcast?nick={}", filler.repeat(3000));
            let request = Request::from_args(&["app", long.as_str()]);
            let Request::Open(Ok(sent)) = &request else {
                panic!("{request:?}");
            };
            assert!(sent.len() > MAX_LINK_LEN && sent.len() <= MAX_LINK_LEN + 4);
            assert!(request.encode().len() <= MAX_MESSAGE);
            assert_eq!(link::parse(sent), Err(LinkError::TooLong));
        }
        let fits = format!("gawk://broadcast?nick={}", "x".repeat(MAX_LINK_LEN - 22));
        assert_eq!(fits.len(), MAX_LINK_LEN);
        assert_eq!(
            Request::from_args(&["app", fits.as_str()]),
            Request::Open(Ok(fits.clone()))
        );
    }

    // G10: the update relaunch never applies the launch link again, and
    // carries one marker, not a growing chain.
    #[test]
    fn the_relaunch_drops_links_and_old_markers() {
        let args: Vec<String> = [
            "--minimized",
            "gawk://broadcast?room=abc",
            RELAUNCHED_FROM,
            "41",
            "--keep",
        ]
        .map(String::from)
        .to_vec();
        let os: Vec<OsString> = args.iter().map(OsString::from).collect();
        assert_eq!(
            relaunch_args(&os),
            vec![OsString::from("--minimized"), "--keep".into()]
        );
        assert_eq!(relaunched_from(&args), Some(41));
        assert_eq!(relaunched_from(&["app", RELAUNCHED_FROM]), None);
        assert_eq!(relaunched_from(&["app", RELAUNCHED_FROM, "x"]), None);
    }

    /// A scripted endpoint: who answers, and what a claim finds.
    struct Fake {
        primary_answers: Vec<bool>,
        claims: Vec<std::io::Result<bool>>,
        log: Rc<RefCell<Vec<String>>>,
    }

    // The fake never crosses threads; Endpoint's Send bound is for real ones.
    unsafe impl Send for Fake {}

    impl Endpoint for Fake {
        fn claim(&mut self) -> std::io::Result<bool> {
            self.log.borrow_mut().push("claim".into());
            self.claims.remove(0)
        }
        fn send(&mut self, r: &Request) -> std::io::Result<()> {
            self.log.borrow_mut().push(format!("send {}", r.encode()));
            if self.primary_answers.remove(0) {
                Ok(())
            } else {
                Err(std::io::ErrorKind::NotFound.into())
            }
        }
        fn serve(self: Box<Self>, _: mpsc::Sender<Incoming>) -> std::io::Result<()> {
            Ok(())
        }
    }

    fn fake(
        answers: &[bool],
        claims: Vec<std::io::Result<bool>>,
    ) -> (Fake, Rc<RefCell<Vec<String>>>) {
        let log = Rc::new(RefCell::new(Vec::new()));
        (
            Fake {
                primary_answers: answers.to_vec(),
                claims,
                log: log.clone(),
            },
            log,
        )
    }

    fn run(ep: &mut Fake, relaunched: bool) -> Startup {
        startup_with(
            ep,
            &Request::Raise,
            relaunched,
            Duration::from_millis(50),
            || std::thread::sleep(Duration::from_millis(10)),
        )
    }

    #[test]
    fn a_running_primary_takes_the_request() {
        let (mut ep, log) = fake(&[true], vec![]);
        assert_eq!(run(&mut ep, false), Startup::HandedOff);
        assert_eq!(*log.borrow(), ["send raise"]);
    }

    #[test]
    fn nobody_answering_makes_this_the_primary() {
        let (mut ep, log) = fake(&[false], vec![Ok(true)]);
        assert_eq!(run(&mut ep, false), Startup::Primary);
        assert_eq!(*log.borrow(), ["send raise", "claim"]);
    }

    #[test]
    fn a_lost_race_sends_once_more() {
        let (mut ep, _) = fake(&[false, true], vec![Ok(false)]);
        assert_eq!(run(&mut ep, false), Startup::HandedOff);
        let (mut ep, _) = fake(&[false, false], vec![Ok(false)]);
        assert!(matches!(run(&mut ep, false), Startup::Alone(_)));
    }

    #[test]
    fn a_broken_endpoint_runs_alone() {
        let (mut ep, _) = fake(&[false], vec![Err(std::io::ErrorKind::Other.into())]);
        assert!(matches!(run(&mut ep, false), Startup::Alone(_)));
    }

    // G11: the update relaunch never hands over to the process it
    // replaces; it waits for it to let go and becomes the primary.
    #[test]
    fn a_relaunch_waits_for_the_old_process_instead_of_handing_off() {
        let (mut ep, log) = fake(&[], vec![Ok(false), Ok(false), Ok(true)]);
        // A loaded CI runner: each pause takes far longer than asked. This
        // case is about waiting, not the deadline, so the deadline is the
        // production one; a 50 ms one flaked on CI once two slow pauses
        // outlasted it and the relaunch fell through to an unscripted send.
        let startup = startup_with(&mut ep, &Request::Raise, true, RELAUNCH_WAIT, || {
            std::thread::sleep(Duration::from_millis(60))
        });
        assert_eq!(startup, Startup::Primary);
        assert_eq!(*log.borrow(), ["claim", "claim", "claim"]);
    }

    #[test]
    fn a_relaunch_that_times_out_falls_back_to_the_normal_rule() {
        let claims = (0..20).map(|_| Ok(false)).collect();
        let (mut ep, log) = fake(&[true], claims);
        assert_eq!(run(&mut ep, true), Startup::HandedOff);
        assert_eq!(log.borrow().last().map(String::as_str), Some("send raise"));
    }
}
