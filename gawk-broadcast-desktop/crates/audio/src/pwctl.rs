//! The app-audio control plane, in-process (docs/58 D8, OD4): the docs/39
//! mechanism — a virtual `Audio/Sink/Internal` sink per broadcast, the target
//! application's output ports LINKED into it as a tee, the sink's monitor
//! read by the audio pipeline — on a dedicated thread with its own
//! `pipewire` loop and **its own core connection**, separate from every
//! `pipewiresrc` GStreamer opens.
//!
//! Cleanup is by construction, as it was for the Go helper: the sink and
//! every link are proxies on this connection, never linger-flagged, so any
//! process exit — clean, panic, SIGKILL, OOM — closes the connection and the
//! daemon destroys them. The process boundary of docs/39 D4 became the
//! connection boundary; CI's kill matrix asserts it (G11).
//!
//! The engine talks to it over channels. Every request has the helper's 5 s
//! round-trip timeout, so a wedged loop degrades audio (docs/39 D6) and can
//! never block the UI or video thread. The graph logic is
//! [`crate::pwgraph`]'s, unit-tested daemon-free; this file only moves
//! registry events in and executes what the controller asks.

use crate::pwgraph::{Controller, Daemon, Event, Graph, Kind, Link, RegistryEvent};
use pipewire as pw;
use pw::types::ObjectType;
use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::rc::Rc;
use std::sync::mpsc;
use std::time::{Duration, Instant};

/// The helper's round-trip budget (docs/39 D6's wedged-loop row).
pub const ROUNDTRIP_TIMEOUT: Duration = Duration::from_secs(5);

/// The capture sink's `node.name` for this process.
pub fn sink_name() -> String {
    format!("gawk-app-capture-{}", std::process::id())
}

enum Request {
    Watch,
    Capture(Option<String>, mpsc::Sender<Result<u32, String>>),
    Release,
    Stop,
}

/// What the engine sees of the control plane.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Notice {
    /// Registry state as the controller reports it.
    Event(Event),
    /// The loop or the daemon connection died: audio degrades per D6.
    Fatal(String),
}

/// A running control plane.
pub struct PwCtl {
    requests: mpsc::Sender<Request>,
    notices: mpsc::Receiver<Notice>,
    thread: Option<std::thread::JoinHandle<()>>,
    /// Test seam: the requests' reply timeout (D6's 5 s in production).
    timeout: Duration,
}

impl PwCtl {
    /// Connects and completes TWO registry round trips before returning
    /// (docs/39 F1: the first delivers the globals and triggers the binds;
    /// the bound objects' properties — the binaries — land after it). The
    /// initial app list is the first notice.
    pub fn start() -> Result<Self, String> {
        let (req_tx, req_rx) = mpsc::channel();
        let (note_tx, note_rx) = mpsc::channel();
        let (ready_tx, ready_rx) = mpsc::channel();
        let thread = std::thread::Builder::new()
            .name("pwctl".into())
            .spawn(move || {
                if let Err(e) = run(req_rx, note_tx.clone(), ready_tx) {
                    let _ = note_tx.send(Notice::Fatal(e));
                }
            })
            .map_err(|e| format!("could not start the PipeWire thread: {e}"))?;
        match ready_rx.recv_timeout(ROUNDTRIP_TIMEOUT * 2) {
            Ok(Ok(())) => Ok(Self {
                requests: req_tx,
                notices: note_rx,
                thread: Some(thread),
                timeout: ROUNDTRIP_TIMEOUT,
            }),
            Ok(Err(e)) => {
                let _ = thread.join();
                Err(e)
            }
            Err(_) => {
                // A wedged connect: detach rather than block the caller;
                // the thread ends with the process or its connection.
                let _ = req_tx.send(Request::Stop);
                Err("the PipeWire control connection did not come up in time".into())
            }
        }
    }

    /// Re-sends the current app list.
    pub fn watch(&self) {
        let _ = self.requests.send(Request::Watch);
    }

    /// Captures `binary`'s audio, or (None) the whole system's through the
    /// same sink. Returns the sink's `object.serial` — the audio pipeline's
    /// target — within the round-trip budget, or why not.
    pub fn capture(&self, binary: Option<&str>) -> Result<u32, String> {
        let (tx, rx) = mpsc::channel();
        self.requests
            .send(Request::Capture(binary.map(str::to_owned), tx))
            .map_err(|_| "the PipeWire control plane has stopped".to_owned())?;
        match rx.recv_timeout(self.timeout) {
            Ok(r) => r,
            Err(_) => Err(format!(
                "the PipeWire control plane did not answer within {}s",
                self.timeout.as_secs()
            )),
        }
    }

    /// Drops every link; the sink stays for a later capture.
    pub fn release(&self) {
        let _ = self.requests.send(Request::Release);
    }

    /// Notices since the last call.
    pub fn drain(&self) -> Vec<Notice> {
        self.notices.try_iter().collect()
    }

    /// Stops: every link and the sink are destroyed, the connection closed.
    /// Bounded: a wedged loop is abandoned after the round-trip budget
    /// rather than joined forever.
    pub fn stop(mut self) {
        self.shutdown();
    }

    fn shutdown(&mut self) {
        let _ = self.requests.send(Request::Stop);
        if let Some(t) = self.thread.take() {
            let deadline = Instant::now() + ROUNDTRIP_TIMEOUT;
            while !t.is_finished() && Instant::now() < deadline {
                std::thread::sleep(Duration::from_millis(10));
            }
            if t.is_finished() {
                let _ = t.join();
            } else {
                log::warn!("the PipeWire control thread did not stop in time; abandoning it");
            }
        }
    }
}

impl Drop for PwCtl {
    fn drop(&mut self) {
        self.shutdown();
    }
}

/// A proxy this connection created: the sink node or a link. Dropping it
/// destroys the object on the daemon (no linger).
enum Handle {
    Node(pw::node::Node),
    Link(pw::link::Link),
}

/// What the registry callbacks share with the loop, all on this thread.
struct Shared {
    queue: RefCell<Vec<RegistryEvent>>,
    done_seq: Cell<i32>,
    fatal: RefCell<Option<String>>,
}

struct PwDaemon {
    loop_: pw::main_loop::MainLoopRc,
    core: pw::core::CoreRc,
    shared: Rc<Shared>,
}

impl PwDaemon {
    fn iterate(&self, timeout: Duration) {
        self.loop_
            .loop_()
            .iterate(pw::loop_::Timeout::Finite(timeout));
    }

    fn check_fatal(&self) -> Result<(), String> {
        match self.shared.fatal.borrow().as_ref() {
            Some(e) => Err(e.clone()),
            None => Ok(()),
        }
    }
}

impl Daemon for PwDaemon {
    type Handle = Handle;

    fn create_sink(&mut self, name: &str, channels: &[String]) -> Result<Handle, String> {
        let mut props = pw::properties::PropertiesBox::new();
        // The adapter factory wraps this SPA factory — what `pw-cli
        // create-node adapter` does, and OBS's audio capture.
        props.insert("factory.name", "support.null-audio-sink");
        props.insert("node.name", name);
        props.insert("node.description", "gawk application audio capture");
        // Internal: a real sink for linking, hidden from the device lists
        // applications and volume controls show.
        props.insert("media.class", "Audio/Sink/Internal");
        props.insert("audio.position", format!("[ {} ]", channels.join(",")));
        props.insert("audio.channels", channels.len().to_string());
        props.insert("monitor.channel-volumes", "true");
        props.insert("node.virtual", "true");
        // Deliberately absent: object.linger. Its absence IS the crash
        // safety (docs/39 D4, docs/58 D8).
        self.core
            .create_object::<pw::node::Node>("adapter", &props)
            .map(Handle::Node)
            .map_err(|e| format!("PipeWire refused to create the capture sink: {e}"))
    }

    fn create_link(&mut self, l: Link) -> Result<Handle, String> {
        let mut props = pw::properties::PropertiesBox::new();
        props.insert("link.output.node", l.out_node.to_string());
        props.insert("link.output.port", l.out_port.to_string());
        props.insert("link.input.node", l.in_node.to_string());
        props.insert("link.input.port", l.in_port.to_string());
        // Again no linger: a link outliving us would be a routing change we
        // made to someone's machine and did not undo.
        self.core
            .create_object::<pw::link::Link>("link-factory", &props)
            .map(Handle::Link)
            .map_err(|e| {
                format!(
                    "PipeWire refused to link port {} into port {}: {e}",
                    l.out_port, l.in_port
                )
            })
    }

    fn destroy(&mut self, h: Handle) {
        match h {
            Handle::Node(n) => drop(n),
            Handle::Link(l) => drop(l),
        }
    }

    fn roundtrip(&mut self, graph: &mut Graph) -> Result<(), String> {
        let seq = self
            .core
            .sync(0)
            .map_err(|e| format!("PipeWire sync failed: {e}"))?
            .seq();
        let deadline = Instant::now() + ROUNDTRIP_TIMEOUT;
        while self.shared.done_seq.get() < seq {
            self.check_fatal()?;
            if Instant::now() > deadline {
                return Err(format!(
                    "timed out waiting for PipeWire ({}s)",
                    ROUNDTRIP_TIMEOUT.as_secs()
                ));
            }
            self.iterate(Duration::from_millis(20));
        }
        for e in self.shared.queue.borrow_mut().drain(..) {
            match e {
                RegistryEvent::Add(id, k, p) => graph.add(id, k, p),
                RegistryEvent::Merge(id, p) => graph.merge(id, p),
                RegistryEvent::Remove(id) => graph.remove(id),
            }
        }
        Ok(())
    }
}

fn dict_map(d: Option<&pw::spa::utils::dict::DictRef>) -> HashMap<String, String> {
    d.map(|d| {
        d.iter()
            .map(|(k, v)| (k.to_owned(), v.to_owned()))
            .collect()
    })
    .unwrap_or_default()
}

/// A bound object's proxy and listener, kept for as long as its global
/// lives: binding is the only path to `application.process.binary` (F1).
/// Held, never read — dropping them is what unbinds.
#[allow(dead_code)]
enum Bound {
    Client(pw::client::Client, pw::client::ClientListener),
    Node(pw::node::Node, pw::node::NodeListener),
}

fn run(
    requests: mpsc::Receiver<Request>,
    notices: mpsc::Sender<Notice>,
    ready: mpsc::Sender<Result<(), String>>,
) -> Result<(), String> {
    pw::init();
    let setup = || -> Result<_, String> {
        let loop_ = pw::main_loop::MainLoopRc::new(None)
            .map_err(|e| format!("could not create the PipeWire loop: {e}"))?;
        let context = pw::context::ContextRc::new(&loop_, None)
            .map_err(|e| format!("could not create the PipeWire context: {e}"))?;
        let core = context.connect_rc(None).map_err(|e| {
            format!("could not connect to the PipeWire daemon (is it running?): {e}")
        })?;
        let registry = core
            .get_registry_rc()
            .map_err(|e| format!("could not get the PipeWire registry: {e}"))?;
        Ok((loop_, context, core, registry))
    };
    let (loop_, _context, core, registry) = match setup() {
        Ok(v) => v,
        Err(e) => {
            let _ = ready.send(Err(e.clone()));
            return Err(e);
        }
    };

    let shared = Rc::new(Shared {
        queue: RefCell::new(Vec::new()),
        done_seq: Cell::new(-1),
        fatal: RefCell::new(None),
    });

    let _core_listener = {
        let (done, fatal) = (shared.clone(), shared.clone());
        core.add_listener_local()
            .done(move |id, seq| {
                if id == pw::core::PW_ID_CORE {
                    done.done_seq.set(done.done_seq.get().max(seq.seq()));
                }
            })
            .error(move |id, _seq, res, message| {
                // Errors against other objects are per-object failures — a
                // refused link, most often — and must not end the plane.
                if id == pw::core::PW_ID_CORE {
                    *fatal.fatal.borrow_mut() = Some(format!("PipeWire error {res}: {message}"));
                }
            })
            .register()
    };

    // The registry listener is registered before the loop ever iterates, so
    // the opening burst — every existing global, announced exactly once —
    // cannot be dropped (docs/39 F8's defect is structurally absent here).
    let bound: Rc<RefCell<HashMap<u32, Bound>>> = Rc::default();
    let _registry_listener = {
        let (s_add, s_rm) = (shared.clone(), shared.clone());
        let reg = registry.clone();
        let (b_add, b_rm) = (bound.clone(), bound.clone());
        registry
            .add_listener_local()
            .global(move |g| {
                let kind = match g.type_ {
                    ObjectType::Node => Kind::Node,
                    ObjectType::Port => Kind::Port,
                    ObjectType::Client => Kind::Client,
                    _ => return,
                };
                let props = dict_map(g.props);
                let stream = props
                    .get(crate::pwgraph::KEY_MEDIA_CLASS)
                    .map(String::as_str)
                    == Some(crate::pwgraph::CLASS_STREAM_OUTPUT);
                s_add
                    .queue
                    .borrow_mut()
                    .push(RegistryEvent::Add(g.id, kind, props));
                let id = g.id;
                match kind {
                    Kind::Client => {
                        if let Ok(c) = reg.bind::<pw::client::Client, _>(g) {
                            let q = s_add.clone();
                            let l = c
                                .add_listener_local()
                                .info(move |info| {
                                    q.queue
                                        .borrow_mut()
                                        .push(RegistryEvent::Merge(id, dict_map(info.props())));
                                })
                                .register();
                            b_add.borrow_mut().insert(id, Bound::Client(c, l));
                        }
                    }
                    Kind::Node if stream => {
                        if let Ok(n) = reg.bind::<pw::node::Node, _>(g) {
                            let q = s_add.clone();
                            let l = n
                                .add_listener_local()
                                .info(move |info| {
                                    q.queue
                                        .borrow_mut()
                                        .push(RegistryEvent::Merge(id, dict_map(info.props())));
                                })
                                .register();
                            b_add.borrow_mut().insert(id, Bound::Node(n, l));
                        }
                    }
                    _ => {}
                }
            })
            .global_remove(move |id| {
                b_rm.borrow_mut().remove(&id);
                s_rm.queue.borrow_mut().push(RegistryEvent::Remove(id));
            })
            .register()
    };

    let mut daemon = PwDaemon {
        loop_,
        core,
        shared: shared.clone(),
    };
    let mut ctl: Controller<PwDaemon> = Controller::new(sink_name());
    let start = (|| -> Result<(), String> {
        let mut g = std::mem::take(&mut ctl.graph);
        daemon.roundtrip(&mut g)?;
        daemon.roundtrip(&mut g)?;
        ctl.graph = g;
        Ok(())
    })();
    if let Err(e) = start {
        let _ = ready.send(Err(e.clone()));
        return Err(e);
    }
    let send = |events: Vec<Event>| {
        for e in events {
            let _ = notices.send(Notice::Event(e));
        }
    };
    // The first list is queued BEFORE `start` returns, so a caller that
    // drains at once sees it — an app already playing is never "not looked
    // yet" (docs/39 F1/F8).
    send(ctl.apps_event(true).into_iter().collect());
    let _ = ready.send(Ok(()));

    let result = loop {
        daemon.iterate(Duration::from_millis(50));
        if let Err(e) = daemon.check_fatal() {
            break Err(e);
        }
        let events: Vec<RegistryEvent> = shared.queue.borrow_mut().drain(..).collect();
        if !events.is_empty() {
            ctl.apply(events);
            send(ctl.changed(&mut daemon));
        }
        match requests.try_recv() {
            Ok(Request::Watch) => send(ctl.apps_event(true).into_iter().collect()),
            Ok(Request::Capture(binary, reply)) => {
                let r = ctl.capture(&mut daemon, binary.as_deref());
                let answer = match r {
                    Ok(events) => {
                        send(events);
                        ctl.sink_serial()
                            .ok_or_else(|| "the capture sink is gone".to_owned())
                    }
                    Err(e) => Err(e),
                };
                let _ = reply.send(answer);
            }
            Ok(Request::Release) => send(vec![ctl.release(&mut daemon)]),
            Ok(Request::Stop) | Err(mpsc::TryRecvError::Disconnected) => break Ok(()),
            Err(mpsc::TryRecvError::Empty) => {}
        }
    };
    ctl.teardown(&mut daemon);
    // Let the destroys reach the daemon before the connection closes (it
    // would reap them anyway; this keeps a clean stop tidy in the logs).
    let mut g = Graph::new();
    let _ = daemon.roundtrip(&mut g);
    bound.borrow_mut().clear();
    result
}
