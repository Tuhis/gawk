//! The app-audio control plane's brain (docs/58 D8, the Go `pwgraph` and the
//! `gawk-pw-helper` state machine, rebuilt): registry bookkeeping, the
//! emitting-application list, the port-link plan that captures one
//! application's audio, and the request/reconcile loop that keeps the links
//! true. Pure and portable — no daemon — because the parts that break under a
//! game launch are ordering and identity, not the C calls; `pwctl` (Linux)
//! only forwards registry events in and executes what [`Controller`] asks of
//! its [`Daemon`].
//!
//! The one rule worth stating twice: **links are a tee, never a re-route.**
//! PipeWire output ports fan out, so linking an application's ports into our
//! sink gives us a copy while the application keeps playing to the speakers.
//! Nothing here ever moves a stream, and no failure mode can leave a user
//! with silent speakers.

use std::collections::{BTreeMap, HashMap};

// PipeWire property keys. Spelled once: a typo in one of these is a feature
// that silently does nothing.
pub const KEY_MEDIA_CLASS: &str = "media.class";
pub const KEY_NODE_NAME: &str = "node.name";
pub const KEY_NODE_DESC: &str = "node.description";
pub const KEY_CLIENT_ID: &str = "client.id";
pub const KEY_NODE_ID: &str = "node.id";
pub const KEY_SERIAL: &str = "object.serial";
pub const KEY_PORT_DIR: &str = "port.direction";
pub const KEY_PORT_ID: &str = "port.id";
pub const KEY_PORT_MONITOR: &str = "port.monitor";
pub const KEY_CHANNEL: &str = "audio.channel";
pub const KEY_APP_NAME: &str = "application.name";
pub const KEY_APP_BINARY: &str = "application.process.binary";

/// The media class of an application playing audio.
pub const CLASS_STREAM_OUTPUT: &str = "Stream/Output/Audio";

/// Which registry interface a global is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Node,
    Port,
    Client,
}

/// One application emitting audio, as the whose-audio card lists it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct App {
    pub binary: String,
    pub name: String,
    pub streams: usize,
}

/// One port-to-port connection to create.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct Link {
    pub out_node: u32,
    pub out_port: u32,
    pub in_node: u32,
    pub in_port: u32,
}

#[derive(Debug, Clone)]
struct Object {
    kind: Kind,
    props: HashMap<String, String>,
}

/// A tracked node's typed view.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Node {
    pub id: u32,
    pub serial: u32,
    pub media_class: String,
    pub name: String,
    pub desc: String,
    pub client_id: u32,
    pub binary: String,
    pub app_name: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct Port {
    id: u32,
    node_id: u32,
    index: u32,
    out: bool,
    monitor: bool,
    channel: String,
}

/// The tracked slice of the registry. Objects are raw property maps and the
/// typed views are derived on read — which is what makes [`Graph::merge`]
/// cheap and correct: an object's identity arrives in two instalments (the
/// registry global first, the bound object's fuller list a moment later).
#[derive(Debug, Default, Clone)]
pub struct Graph {
    objects: BTreeMap<u32, Object>,
}

fn u32_of(s: Option<&String>) -> u32 {
    s.and_then(|v| v.parse().ok()).unwrap_or(0)
}

impl Graph {
    pub fn new() -> Self {
        Self::default()
    }

    /// Records a global, replacing any previous record for that id:
    /// PipeWire reuses ids after a removal, and treating a reused id as a
    /// duplicate would go blind to the new object — exactly what a game
    /// restart produces.
    pub fn add(&mut self, id: u32, kind: Kind, props: HashMap<String, String>) {
        self.objects.insert(id, Object { kind, props });
    }

    /// Overlays a BOUND object's fuller property list (docs/39 F1: a
    /// client's registry globals stop at `application.name`; the binary
    /// appears only here). An id never seen is a no-op, not an insert — an
    /// info event without its global is a bind racing a removal.
    pub fn merge(&mut self, id: u32, props: HashMap<String, String>) {
        if let Some(o) = self.objects.get_mut(&id) {
            o.props.extend(props);
        }
    }

    pub fn remove(&mut self, id: u32) {
        self.objects.remove(&id);
    }

    fn node_of(&self, id: u32, o: &Object) -> Node {
        let p = &o.props;
        let s = |k: &str| p.get(k).cloned().unwrap_or_default();
        Node {
            id,
            serial: u32_of(p.get(KEY_SERIAL)),
            media_class: s(KEY_MEDIA_CLASS),
            name: s(KEY_NODE_NAME),
            desc: s(KEY_NODE_DESC),
            client_id: u32_of(p.get(KEY_CLIENT_ID)),
            binary: s(KEY_APP_BINARY),
            app_name: s(KEY_APP_NAME),
        }
    }

    fn port_of(&self, id: u32, o: &Object) -> Port {
        let p = &o.props;
        Port {
            id,
            node_id: u32_of(p.get(KEY_NODE_ID)),
            index: u32_of(p.get(KEY_PORT_ID)),
            out: p.get(KEY_PORT_DIR).is_some_and(|d| d == "out"),
            monitor: p.get(KEY_PORT_MONITOR).is_some_and(|m| m == "true"),
            channel: p.get(KEY_CHANNEL).cloned().unwrap_or_default(),
        }
    }

    fn nodes(&self) -> impl Iterator<Item = Node> + '_ {
        self.objects
            .iter()
            .filter(|(_, o)| o.kind == Kind::Node)
            .map(|(&id, o)| self.node_of(id, o))
    }

    fn client(&self, id: u32) -> Option<(String, String)> {
        let o = self.objects.get(&id).filter(|o| o.kind == Kind::Client)?;
        let s = |k: &str| o.props.get(k).cloned().unwrap_or_default();
        Some((s(KEY_APP_BINARY), s(KEY_APP_NAME)))
    }

    /// A tracked node.
    pub fn node(&self, id: u32) -> Option<Node> {
        let o = self.objects.get(&id).filter(|o| o.kind == Kind::Node)?;
        Some(self.node_of(id, o))
    }

    /// The sink we asked the daemon for, by its exact `node.name`: the
    /// create call hands back a proxy, and the global arrives separately.
    pub fn find_sink_by_name(&self, name: &str) -> Option<Node> {
        self.nodes()
            .find(|n| n.name == name && n.media_class.starts_with("Audio/Sink"))
    }

    /// A node's owning binary: its own property when it has one, else its
    /// client's — the common path (docs/39 F1; GStreamer's `pipewiresink`
    /// sets nothing on the node).
    pub fn binary(&self, n: &Node) -> String {
        if !n.binary.is_empty() {
            return n.binary.clone();
        }
        self.client(n.client_id).map(|(b, _)| b).unwrap_or_default()
    }

    /// What the card shows: never empty for a listed node.
    pub fn display_name(&self, n: &Node) -> String {
        if !n.app_name.is_empty() {
            return n.app_name.clone();
        }
        if let Some((_, name)) = self.client(n.client_id)
            && !name.is_empty()
        {
            return name;
        }
        if !n.desc.is_empty() {
            return n.desc.clone();
        }
        if !n.name.is_empty() {
            return n.name.clone();
        }
        self.binary(n)
    }

    /// The applications emitting audio now, one row per binary, sorted for
    /// a stable render. Built from LIVE output streams — the user picks
    /// whoever is actually making sound (docs/39 D5). A node whose binary
    /// cannot be resolved is skipped: it could never be re-linked after a
    /// restart, since that missing string is the identity.
    pub fn apps(&self) -> Vec<App> {
        let mut by: BTreeMap<String, App> = BTreeMap::new();
        for n in self.nodes() {
            if n.media_class != CLASS_STREAM_OUTPUT {
                continue;
            }
            let bin = self.binary(&n);
            if bin.is_empty() {
                continue;
            }
            let name = self.display_name(&n);
            by.entry(bin.clone())
                .or_insert(App {
                    binary: bin,
                    name,
                    streams: 0,
                })
                .streams += 1;
        }
        let mut out: Vec<App> = by.into_values().collect();
        out.sort_by(|a, b| {
            a.name
                .to_lowercase()
                .cmp(&b.name.to_lowercase())
                .then_with(|| a.binary.cmp(&b.binary))
        });
        out
    }

    fn stream_nodes(&self, binary: &str) -> Vec<Node> {
        if binary.is_empty() {
            return Vec::new();
        }
        let mut v: Vec<Node> = self
            .nodes()
            .filter(|n| n.media_class == CLASS_STREAM_OUTPUT && self.binary(n) == binary)
            .collect();
        v.sort_by_key(|n| n.id);
        v
    }

    fn node_ports(&self, node: u32, out: bool, monitor: bool) -> Vec<Port> {
        let mut ps: Vec<Port> = self
            .objects
            .iter()
            .filter(|(_, o)| o.kind == Kind::Port)
            .map(|(&id, o)| self.port_of(id, o))
            .filter(|p| p.node_id == node && p.out == out && p.monitor == monitor)
            .collect();
        ps.sort_by_key(|p| (p.index, p.id));
        ps
    }

    fn sink_inputs(&self, sink: u32) -> Vec<Port> {
        self.node_ports(sink, false, false)
    }

    /// Every link that should exist from `binary`'s streams into the sink —
    /// a pure function of the graph, diffed by the caller, so churn is one
    /// recompute and a small delta. Channels match by name (raw port links
    /// do no channel mixing: a mismatch drops a channel, and the centre is
    /// where the dialogue lives); a single unmatched port spreads to every
    /// input (mono audible on both sides); otherwise positional pairing.
    pub fn plan(&self, binary: &str, sink: u32) -> Vec<Link> {
        let ins = self.sink_inputs(sink);
        if ins.is_empty() {
            return Vec::new();
        }
        let by_channel: HashMap<&str, &Port> = ins
            .iter()
            .filter(|p| !p.channel.is_empty())
            .map(|p| (p.channel.as_str(), p))
            .collect();
        let mut links = Vec::new();
        for n in self.stream_nodes(binary) {
            let outs = self.node_ports(n.id, true, false);
            if outs.is_empty() {
                continue;
            }
            let mut matched = 0;
            for op in &outs {
                if !op.channel.is_empty()
                    && let Some(ip) = by_channel.get(op.channel.as_str())
                {
                    links.push(Link {
                        out_node: n.id,
                        out_port: op.id,
                        in_node: sink,
                        in_port: ip.id,
                    });
                    matched += 1;
                }
            }
            if matched > 0 {
                continue;
            }
            if outs.len() == 1 {
                for ip in &ins {
                    links.push(Link {
                        out_node: n.id,
                        out_port: outs[0].id,
                        in_node: sink,
                        in_port: ip.id,
                    });
                }
                continue;
            }
            for (op, ip) in outs.iter().zip(ins.iter()) {
                links.push(Link {
                    out_node: n.id,
                    out_port: op.id,
                    in_node: sink,
                    in_port: ip.id,
                });
            }
        }
        links.sort_by_key(|l| (l.out_port, l.in_port));
        links
    }

    /// The layout `binary`'s streams speak, widest stream first — the sink's
    /// layout (docs/39 F3: an application's stream node is an adapter whose
    /// output ports already speak the layout it negotiated toward its sink).
    pub fn stream_channels(&self, binary: &str) -> Vec<String> {
        let mut widest: Vec<String> = Vec::new();
        for n in self.stream_nodes(binary) {
            let chans: Vec<String> = self
                .node_ports(n.id, true, false)
                .into_iter()
                .map(|p| p.channel)
                .filter(|c| !c.is_empty() && c != "UNK")
                .collect();
            if chans.len() > widest.len() {
                widest = chans;
            }
        }
        widest
    }

    fn real_sinks(&self) -> impl Iterator<Item = Node> + '_ {
        // Exactly "Audio/Sink": ours is Audio/Sink/Internal, as is anyone
        // else's capture plumbing.
        self.nodes().filter(|n| n.media_class == "Audio/Sink")
    }

    /// The widest layout among the machine's real sinks — the fallback for
    /// an application with no stream open when capture starts.
    pub fn widest_sink_channels(&self) -> Vec<String> {
        let mut widest: Vec<String> = Vec::new();
        for n in self.real_sinks() {
            let chans: Vec<String> = self
                .sink_inputs(n.id)
                .into_iter()
                .map(|p| p.channel)
                .filter(|c| !c.is_empty())
                .collect();
            if chans.len() > widest.len() {
                widest = chans;
            }
        }
        widest
    }

    /// The whole system's sound: the widest real sink's MONITOR ports into
    /// the capture sink (docs/39 F4). The same node the audio pipeline is
    /// already reading, so the mid-session switch is a re-link, never a
    /// renegotiation — and equally incapable of re-routing anything.
    pub fn plan_system_audio(&self, sink: u32) -> Vec<Link> {
        let ins = self.sink_inputs(sink);
        if ins.is_empty() {
            return Vec::new();
        }
        let Some(source) = self
            .real_sinks()
            .map(|n| {
                let width = self.sink_inputs(n.id).len();
                (n, width)
            })
            .filter(|(_, w)| *w > 0)
            .max_by_key(|(n, w)| (*w, std::cmp::Reverse(n.id)))
            .map(|(n, _)| n)
        else {
            return Vec::new();
        };
        let by_channel: HashMap<&str, &Port> = ins
            .iter()
            .filter(|p| !p.channel.is_empty())
            .map(|p| (p.channel.as_str(), p))
            .collect();
        let mut links = Vec::new();
        for (i, mp) in self.node_ports(source.id, true, true).iter().enumerate() {
            let target = if mp.channel.is_empty() {
                None
            } else {
                by_channel.get(mp.channel.as_str()).copied()
            };
            if let Some(ip) = target.or_else(|| ins.get(i)) {
                links.push(Link {
                    out_node: source.id,
                    out_port: mp.id,
                    in_node: sink,
                    in_port: ip.id,
                });
            }
        }
        links.sort_by_key(|l| (l.out_port, l.in_port));
        links
    }
}

/// A registry change, in arrival order. Adds and removes stay interleaved:
/// "add 33, remove 33, add 33" collapsed into two lists in the wrong order is
/// a graph that thinks a dead node is alive.
#[derive(Debug, Clone)]
pub enum RegistryEvent {
    Add(u32, Kind, HashMap<String, String>),
    Merge(u32, HashMap<String, String>),
    Remove(u32),
}

/// What the control plane tells the engine (the helper protocol's events,
/// as a type).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Event {
    /// The emitting-app list changed (or was asked for).
    Apps(Vec<App>),
    /// The capture sink exists: the audio pipeline reads its monitor by
    /// `object.serial` (unique for the daemon's lifetime).
    Sink {
        serial: u32,
        node_id: u32,
        channels: usize,
    },
    /// How many of the target's ports are linked — zero is the silence
    /// hint's signal (docs/39 D6). `binary` is empty for system audio.
    Links {
        binary: String,
        links: usize,
        error: Option<String>,
    },
}

/// The daemon operations the controller needs; `pwctl` implements them on a
/// real PipeWire connection, tests script them.
pub trait Daemon {
    type Handle;
    /// Creates the capture sink (an `Audio/Sink/Internal` null sink, never
    /// linger-flagged).
    fn create_sink(&mut self, name: &str, channels: &[String]) -> Result<Self::Handle, String>;
    fn create_link(&mut self, link: Link) -> Result<Self::Handle, String>;
    fn destroy(&mut self, handle: Self::Handle);
    /// Waits until the daemon has processed everything sent, applying the
    /// registry events that arrived meanwhile to `graph`.
    fn roundtrip(&mut self, graph: &mut Graph) -> Result<(), String>;
}

/// What the capture sink is following.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Target {
    None,
    App(String),
    System,
}

/// The helper's state machine (docs/39 D3, D5 and §8's findings), driven by
/// registry events and requests, against a [`Daemon`].
pub struct Controller<D: Daemon> {
    pub graph: Graph,
    sink_name: String,
    sink: Option<(D::Handle, u32, u32)>, // handle, node id, serial
    target: Target,
    links: BTreeMap<Link, D::Handle>,
    last_apps: Option<Vec<App>>,
    last_links: Option<usize>,
}

impl<D: Daemon> Controller<D> {
    /// `sink_name` is `gawk-app-capture-<pid>`: the pid makes it unique per
    /// process and findable in `pw-cli ls` when diagnosing.
    pub fn new(sink_name: String) -> Self {
        Self {
            graph: Graph::new(),
            sink_name,
            sink: None,
            target: Target::None,
            links: BTreeMap::new(),
            last_apps: None,
            last_links: None,
        }
    }

    /// Applies registry changes; the sink's own removal forgets it.
    pub fn apply(&mut self, events: impl IntoIterator<Item = RegistryEvent>) {
        for e in events {
            match e {
                RegistryEvent::Add(id, kind, props) => self.graph.add(id, kind, props),
                RegistryEvent::Merge(id, props) => self.graph.merge(id, props),
                RegistryEvent::Remove(id) => {
                    if self.sink.as_ref().is_some_and(|(_, n, _)| *n == id) {
                        self.sink = None;
                    }
                    self.graph.remove(id);
                }
            }
        }
    }

    /// After registry changes: a changed app list, and links re-planned.
    pub fn changed(&mut self, daemon: &mut D) -> Vec<Event> {
        let mut out = Vec::new();
        out.extend(self.apps_event(false));
        out.extend(self.reconcile(daemon));
        out
    }

    /// The current app list, as an event (`force` re-sends an unchanged one:
    /// the `watch` request).
    pub fn apps_event(&mut self, force: bool) -> Option<Event> {
        let apps = self.graph.apps();
        if !force && self.last_apps.as_ref() == Some(&apps) {
            return None;
        }
        self.last_apps = Some(apps.clone());
        Some(Event::Apps(apps))
    }

    /// Captures `binary`'s audio (or, `None`, the whole system's through the
    /// same sink — docs/39 F4). The sink is created once and never
    /// recreated (F2): a new target re-links into it, so the serial the
    /// audio pipeline reads never changes. Always answered with a link count
    /// (F5), zero included.
    pub fn capture(&mut self, daemon: &mut D, binary: Option<&str>) -> Result<Vec<Event>, String> {
        let target = match binary {
            Some("") => return Err("capture requested with no application binary".into()),
            Some(b) => Target::App(b.to_owned()),
            None => Target::System,
        };
        let mut out = Vec::new();
        if self.sink.is_none() {
            out.push(self.create_sink(daemon, &target)?);
        }
        if self.target != target {
            self.release_links(daemon);
            self.target = target;
        }
        let mut events = self.reconcile(daemon);
        events.retain(|e| !matches!(e, Event::Links { error: None, .. }));
        out.extend(events);
        out.push(self.links_event(true, None).expect("forced"));
        Ok(out)
    }

    /// Drops every link and stops following anyone; the sink stays.
    pub fn release(&mut self, daemon: &mut D) -> Event {
        self.release_links(daemon);
        self.target = Target::None;
        self.links_event(true, None).expect("forced")
    }

    /// The sink's serial, once it exists.
    pub fn sink_serial(&self) -> Option<u32> {
        self.sink.as_ref().map(|(_, _, s)| *s)
    }

    /// Links held now.
    pub fn link_count(&self) -> usize {
        self.links.len()
    }

    /// Tears everything down (also what closing the connection does).
    pub fn teardown(&mut self, daemon: &mut D) {
        self.release_links(daemon);
        if let Some((h, _, _)) = self.sink.take() {
            daemon.destroy(h);
        }
    }

    fn create_sink(&mut self, daemon: &mut D, target: &Target) -> Result<Event, String> {
        // The sink speaks the target application's own layout (F3), falling
        // back to the widest real sink, then stereo.
        let mut chans = match target {
            Target::App(b) => self.graph.stream_channels(b),
            _ => Vec::new(),
        };
        if chans.is_empty() {
            chans = self.graph.widest_sink_channels();
        }
        if chans.is_empty() {
            chans = vec!["FL".into(), "FR".into()];
        }
        let handle = daemon.create_sink(&self.sink_name, &chans)?;
        // The proxy comes back at once; the node's global a round trip later.
        let mut found = None;
        for _ in 0..2 {
            daemon.roundtrip(&mut self.graph)?;
            found = self.graph.find_sink_by_name(&self.sink_name);
            if found.is_some() {
                break;
            }
        }
        let Some(node) = found else {
            daemon.destroy(handle);
            return Err("the capture sink was created but never appeared in the registry".into());
        };
        self.sink = Some((handle, node.id, node.serial));
        Ok(Event::Sink {
            serial: node.serial,
            node_id: node.id,
            channels: chans.len(),
        })
    }

    fn reconcile(&mut self, daemon: &mut D) -> Vec<Event> {
        let Some((_, sink, _)) = self.sink.as_ref().map(|(h, n, s)| (h, *n, *s)) else {
            return Vec::new();
        };
        let want = match &self.target {
            Target::None => return Vec::new(),
            Target::App(b) => self.graph.plan(b, sink),
            Target::System => self.graph.plan_system_audio(sink),
        };
        let stale: Vec<Link> = self
            .links
            .keys()
            .filter(|l| !want.contains(l))
            .copied()
            .collect();
        for l in stale {
            if let Some(h) = self.links.remove(&l) {
                daemon.destroy(h);
            }
        }
        let mut out = Vec::new();
        for l in want {
            if self.links.contains_key(&l) {
                continue;
            }
            match daemon.create_link(l) {
                Ok(h) => {
                    self.links.insert(l, h);
                }
                // A refused link is reported and retried on the next
                // registry change; it never ends the control plane.
                Err(e) => out.extend(self.links_event(true, Some(e))),
            }
        }
        out.extend(self.links_event(false, None));
        out
    }

    fn links_event(&mut self, force: bool, error: Option<String>) -> Option<Event> {
        let n = self.links.len();
        if !force && self.last_links == Some(n) {
            return None;
        }
        self.last_links = Some(n);
        Some(Event::Links {
            binary: match &self.target {
                Target::App(b) => b.clone(),
                _ => String::new(),
            },
            links: n,
            error,
        })
    }

    fn release_links(&mut self, daemon: &mut D) {
        for (_, h) in std::mem::take(&mut self.links) {
            daemon.destroy(h);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn props(kv: &[(&str, &str)]) -> HashMap<String, String> {
        kv.iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    fn port(g: &mut Graph, id: u32, node: u32, index: u32, dir: &str, ch: &str, monitor: bool) {
        let (n, i) = (node.to_string(), index.to_string());
        g.add(
            id,
            Kind::Port,
            props(&[
                (KEY_NODE_ID, &n),
                (KEY_PORT_ID, &i),
                (KEY_PORT_DIR, dir),
                (KEY_CHANNEL, ch),
                (KEY_PORT_MONITOR, if monitor { "true" } else { "false" }),
            ]),
        );
    }

    /// A stereo stream owned by a client — the common shape, identity only
    /// on the client (as GStreamer's pipewiresink produces).
    fn emitter(g: &mut Graph, client: u32, node: u32, binary: &str, name: &str, ports: &[u32]) {
        g.add(
            client,
            Kind::Client,
            props(&[(KEY_APP_BINARY, binary), (KEY_APP_NAME, name)]),
        );
        let (c, s) = (client.to_string(), (node * 3).to_string());
        g.add(
            node,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_NODE_NAME, binary),
                (KEY_CLIENT_ID, &c),
                (KEY_SERIAL, &s),
            ]),
        );
        for (i, p) in ports.iter().enumerate() {
            let ch = ["FL", "FR"].get(i).copied().unwrap_or("UNK");
            port(g, *p, node, i as u32, "out", ch, false);
        }
    }

    const SURROUND: [&str; 6] = ["FL", "FR", "FC", "LFE", "SL", "SR"];

    fn sink(g: &mut Graph, node: u32, name: &str, class: &str, ins: &[u32], mons: &[u32]) {
        let s = (node * 3).to_string();
        g.add(
            node,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, class),
                (KEY_NODE_NAME, name),
                (KEY_SERIAL, &s),
            ]),
        );
        for (i, p) in ins.iter().enumerate() {
            port(g, *p, node, i as u32, "in", SURROUND[i % 6], false);
        }
        for (i, p) in mons.iter().enumerate() {
            port(g, *p, node, i as u32, "out", SURROUND[i % 6], true);
        }
    }

    #[test]
    fn identity_resolves_through_the_owning_client() {
        let mut g = Graph::new();
        emitter(&mut g, 32, 33, "supertuxkart", "SuperTuxKart", &[43, 44]);
        assert_eq!(
            g.apps(),
            [App {
                binary: "supertuxkart".into(),
                name: "SuperTuxKart".into(),
                streams: 1
            }]
        );
    }

    /// docs/39 F1: the binary arrives only through the BOUND object's info.
    #[test]
    fn the_binary_arrives_through_merge_and_merge_never_invents_objects() {
        let mut g = Graph::new();
        g.add(32, Kind::Client, props(&[(KEY_APP_NAME, "Game")]));
        g.add(
            33,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_CLIENT_ID, "32"),
            ]),
        );
        assert!(g.apps().is_empty(), "globals alone carry no binary");
        g.merge(32, props(&[(KEY_APP_BINARY, "game")]));
        assert_eq!(g.apps()[0].binary, "game");
        g.merge(999, props(&[(KEY_APP_BINARY, "ghost")]));
        assert!(g.node(999).is_none());
    }

    #[test]
    fn a_node_property_beats_the_client_property() {
        let mut g = Graph::new();
        g.add(
            10,
            Kind::Client,
            props(&[
                (KEY_APP_BINARY, "pipewire-pulse"),
                (KEY_APP_NAME, "PulseAudio"),
            ]),
        );
        g.add(
            11,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_CLIENT_ID, "10"),
                (KEY_APP_BINARY, "firefox"),
                (KEY_APP_NAME, "Firefox"),
            ]),
        );
        let apps = g.apps();
        assert_eq!(
            (apps[0].binary.as_str(), apps[0].name.as_str()),
            ("firefox", "Firefox")
        );
    }

    #[test]
    fn event_ordering_does_not_change_the_outcome() {
        type Step = Box<dyn Fn(&mut Graph)>;
        let build = |reverse: bool| {
            let mut steps: Vec<Step> = vec![
                Box::new(|g| port(g, 43, 33, 0, "out", "FL", false)),
                Box::new(|g| {
                    g.add(
                        33,
                        Kind::Node,
                        props(&[
                            (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                            (KEY_CLIENT_ID, "32"),
                        ]),
                    )
                }),
                Box::new(|g| port(g, 44, 33, 1, "out", "FR", false)),
                Box::new(|g| g.add(32, Kind::Client, props(&[(KEY_APP_BINARY, "game")]))),
            ];
            if reverse {
                steps.reverse();
            }
            let mut g = Graph::new();
            for s in steps {
                s(&mut g);
            }
            sink(
                &mut g,
                48,
                "gawk",
                "Audio/Sink/Internal",
                &[49, 51],
                &[50, 52],
            );
            g
        };
        for g in [build(false), build(true)] {
            assert_eq!(g.apps()[0].binary, "game");
            assert_eq!(g.plan("game", 48).len(), 2);
        }
    }

    #[test]
    fn reused_ids_replace_rather_than_duplicate() {
        let mut g = Graph::new();
        emitter(&mut g, 32, 33, "game", "Game", &[43, 44]);
        for id in [33, 43, 44, 32] {
            g.remove(id);
        }
        emitter(&mut g, 32, 33, "browser", "Browser", &[43, 44]);
        let apps = g.apps();
        assert_eq!(apps.len(), 1);
        assert_eq!(apps[0].binary, "browser");
    }

    #[test]
    fn multiple_streams_from_one_binary_are_one_row_and_all_linked() {
        let mut g = Graph::new();
        emitter(&mut g, 32, 33, "game", "Game", &[43, 44]);
        g.add(
            60,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_CLIENT_ID, "32"),
            ]),
        );
        port(&mut g, 61, 60, 0, "out", "FL", false);
        port(&mut g, 62, 60, 1, "out", "FR", false);
        assert_eq!(g.apps()[0].streams, 2);
        sink(
            &mut g,
            48,
            "gawk",
            "Audio/Sink/Internal",
            &[49, 51],
            &[50, 52],
        );
        assert_eq!(g.plan("game", 48).len(), 4);
    }

    #[test]
    fn the_plan_matches_channels_by_name() {
        let mut g = Graph::new();
        g.add(32, Kind::Client, props(&[(KEY_APP_BINARY, "game")]));
        g.add(
            33,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_CLIENT_ID, "32"),
            ]),
        );
        port(&mut g, 43, 33, 0, "out", "FR", false);
        port(&mut g, 44, 33, 1, "out", "FL", false);
        sink(
            &mut g,
            48,
            "gawk",
            "Audio/Sink/Internal",
            &[49, 51],
            &[50, 52],
        );
        let pairs: Vec<(u32, u32)> = g
            .plan("game", 48)
            .iter()
            .map(|l| (l.out_port, l.in_port))
            .collect();
        assert_eq!(pairs, [(43, 51), (44, 49)], "FR→FR, FL→FL");
    }

    #[test]
    fn a_mono_port_spreads_and_unnamed_ports_pair_positionally() {
        let mut g = Graph::new();
        g.add(32, Kind::Client, props(&[(KEY_APP_BINARY, "beeper")]));
        g.add(
            33,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_CLIENT_ID, "32"),
            ]),
        );
        port(&mut g, 43, 33, 0, "out", "MONO", false);
        g.add(35, Kind::Client, props(&[(KEY_APP_BINARY, "odd")]));
        g.add(
            36,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_CLIENT_ID, "35"),
            ]),
        );
        port(&mut g, 45, 36, 0, "out", "UNK", false);
        port(&mut g, 46, 36, 1, "out", "UNK", false);
        sink(
            &mut g,
            48,
            "gawk",
            "Audio/Sink/Internal",
            &[49, 51],
            &[50, 52],
        );
        let mono: Vec<(u32, u32)> = g
            .plan("beeper", 48)
            .iter()
            .map(|l| (l.out_port, l.in_port))
            .collect();
        assert_eq!(mono, [(43, 49), (43, 51)]);
        let odd: Vec<(u32, u32)> = g
            .plan("odd", 48)
            .iter()
            .map(|l| (l.out_port, l.in_port))
            .collect();
        assert_eq!(odd, [(45, 49), (46, 51)]);
    }

    #[test]
    fn inputs_unidentifiable_and_monitor_ports_never_appear() {
        let mut g = Graph::new();
        // Unidentifiable stream.
        g.add(
            33,
            Kind::Node,
            props(&[(KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT), (KEY_SERIAL, "40")]),
        );
        port(&mut g, 43, 33, 0, "out", "FL", false);
        // A recording stream is an input, not an emitter.
        g.add(50, Kind::Client, props(&[(KEY_APP_BINARY, "recorder")]));
        g.add(
            51,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, "Stream/Input/Audio"),
                (KEY_CLIENT_ID, "50"),
            ]),
        );
        assert!(g.apps().is_empty());
        sink(
            &mut g,
            48,
            "gawk",
            "Audio/Sink/Internal",
            &[49, 51],
            &[52, 53],
        );
        assert!(g.sink_inputs(48).iter().all(|p| !p.out && !p.monitor));
    }

    #[test]
    fn apps_are_sorted_stably() {
        let mut g = Graph::new();
        emitter(&mut g, 10, 11, "zsh-bell", "zsh", &[12]);
        emitter(&mut g, 20, 21, "aisleriot", "Aisleriot", &[22]);
        emitter(&mut g, 30, 31, "mpv", "mpv", &[32]);
        let bins: Vec<String> = g.apps().into_iter().map(|a| a.binary).collect();
        assert_eq!(bins, ["aisleriot", "mpv", "zsh-bell"]);
    }

    #[test]
    fn the_plan_follows_churn_and_needs_a_sink() {
        let mut g = Graph::new();
        sink(
            &mut g,
            48,
            "gawk",
            "Audio/Sink/Internal",
            &[49, 51],
            &[50, 52],
        );
        emitter(&mut g, 32, 33, "game", "Game", &[43, 44]);
        assert_eq!(g.plan("game", 48).len(), 2);
        for id in [43, 44, 33] {
            g.remove(id);
        }
        assert!(g.plan("game", 48).is_empty());
        g.add(
            70,
            Kind::Node,
            props(&[
                (KEY_MEDIA_CLASS, CLASS_STREAM_OUTPUT),
                (KEY_CLIENT_ID, "32"),
            ]),
        );
        port(&mut g, 71, 70, 0, "out", "FL", false);
        port(&mut g, 72, 70, 1, "out", "FR", false);
        assert!(g.plan("game", 48).iter().all(|l| l.out_node == 70));
        assert!(g.plan("game", 999).is_empty());
        assert!(g.plan("", 48).is_empty());
    }

    /// docs/39 F3: a surround machine is modelled by surround SPEAKERS; the
    /// app's own ports come first, the widest real sink second.
    #[test]
    fn channel_layouts_come_from_the_app_then_the_widest_real_sink() {
        let mut g = Graph::new();
        sink(
            &mut g,
            30,
            "speakers",
            "Audio/Sink",
            &[39, 41, 60, 61, 62, 63],
            &[40, 42],
        );
        sink(
            &mut g,
            48,
            "gawk",
            "Audio/Sink/Internal",
            &[49, 51],
            &[50, 52],
        );
        assert_eq!(
            g.widest_sink_channels(),
            SURROUND,
            "ours is not a real sink"
        );
        emitter(&mut g, 32, 33, "game", "Game", &[43, 44]);
        assert_eq!(g.stream_channels("game"), ["FL", "FR"]);
        assert!(g.stream_channels("absent").is_empty());
    }

    /// docs/39 F4: system audio is the real sink's monitors into our sink.
    #[test]
    fn system_audio_links_the_widest_real_sinks_monitors() {
        let mut g = Graph::new();
        sink(&mut g, 30, "speakers", "Audio/Sink", &[39, 41], &[40, 42]);
        sink(
            &mut g,
            48,
            "gawk",
            "Audio/Sink/Internal",
            &[49, 51],
            &[50, 52],
        );
        let pairs: Vec<(u32, u32)> = g
            .plan_system_audio(48)
            .iter()
            .map(|l| (l.out_port, l.in_port))
            .collect();
        assert_eq!(pairs, [(40, 49), (42, 51)]);
        assert!(g.plan_system_audio(999).is_empty());
    }

    // ----- the controller, against a scripted daemon -----

    #[derive(Default)]
    struct FakeDaemon {
        next: u32,
        sinks_created: u32,
        live: Vec<u32>,
        /// Registry events the next roundtrip delivers.
        pending: Vec<RegistryEvent>,
        refuse_links: bool,
        sink_channels: Vec<String>,
    }

    impl Daemon for FakeDaemon {
        type Handle = u32;
        fn create_sink(&mut self, name: &str, channels: &[String]) -> Result<u32, String> {
            self.sinks_created += 1;
            self.sink_channels = channels.to_vec();
            self.next += 1;
            let h = self.next;
            self.live.push(h);
            // The global arrives a round trip later, as in the daemon.
            let mut p = props(&[
                (KEY_MEDIA_CLASS, "Audio/Sink/Internal"),
                (KEY_NODE_NAME, name),
                (KEY_SERIAL, "4242"),
            ]);
            p.insert("x".into(), "y".into());
            self.pending.push(RegistryEvent::Add(900, Kind::Node, p));
            for (i, ch) in channels.iter().enumerate() {
                let (idx, pid) = (i.to_string(), 901 + i as u32);
                self.pending.push(RegistryEvent::Add(
                    pid,
                    Kind::Port,
                    props(&[
                        (KEY_NODE_ID, "900"),
                        (KEY_PORT_ID, &idx),
                        (KEY_PORT_DIR, "in"),
                        (KEY_CHANNEL, ch),
                    ]),
                ));
            }
            Ok(h)
        }
        fn create_link(&mut self, _l: Link) -> Result<u32, String> {
            if self.refuse_links {
                return Err("link refused".into());
            }
            self.next += 1;
            self.live.push(self.next);
            Ok(self.next)
        }
        fn destroy(&mut self, h: u32) {
            self.live.retain(|&x| x != h);
        }
        fn roundtrip(&mut self, graph: &mut Graph) -> Result<(), String> {
            for e in std::mem::take(&mut self.pending) {
                match e {
                    RegistryEvent::Add(id, k, p) => graph.add(id, k, p),
                    RegistryEvent::Merge(id, p) => graph.merge(id, p),
                    RegistryEvent::Remove(id) => graph.remove(id),
                }
            }
            Ok(())
        }
    }

    fn links_of(events: &[Event]) -> Vec<usize> {
        events
            .iter()
            .filter_map(|e| match e {
                Event::Links { links, .. } => Some(*links),
                _ => None,
            })
            .collect()
    }

    fn with_game() -> (Controller<FakeDaemon>, FakeDaemon) {
        let mut c = Controller::new("gawk-app-capture-7".into());
        emitter(&mut c.graph, 32, 33, "game", "Game", &[43, 44]);
        (c, FakeDaemon::default())
    }

    #[test]
    fn capture_creates_the_sink_in_the_apps_layout_and_links_it() {
        let (mut c, mut d) = with_game();
        let ev = c.capture(&mut d, Some("game")).unwrap();
        assert!(
            matches!(
                ev[0],
                Event::Sink {
                    serial: 4242,
                    channels: 2,
                    ..
                }
            ),
            "{ev:?}"
        );
        assert_eq!(links_of(&ev), [2]);
        assert_eq!(d.sink_channels, ["FL", "FR"]);
        assert_eq!(c.sink_serial(), Some(4242));
    }

    /// docs/39 F2: re-targeting — another app, or system audio — re-links
    /// into the SAME sink, whose serial the audio pipeline holds.
    #[test]
    fn the_sink_is_created_once_and_never_recreated() {
        let (mut c, mut d) = with_game();
        emitter(&mut c.graph, 34, 35, "browser", "Browser", &[45, 46]);
        sink(
            &mut c.graph,
            30,
            "speakers",
            "Audio/Sink",
            &[39, 41],
            &[40, 42],
        );
        c.capture(&mut d, Some("game")).unwrap();
        c.capture(&mut d, Some("browser")).unwrap();
        let ev = c.capture(&mut d, None).unwrap();
        assert_eq!(d.sinks_created, 1);
        assert_eq!(c.sink_serial(), Some(4242));
        assert_eq!(links_of(&ev), [2], "the speakers' two monitors");
        assert!(matches!(&ev[..], [Event::Links { binary, .. }] if binary.is_empty()));
    }

    /// docs/39 F5: a capture of a silent app is answered with zero, not
    /// silence on the protocol too.
    #[test]
    fn a_capture_is_always_answered_with_a_link_count() {
        let mut c: Controller<FakeDaemon> = Controller::new("s".into());
        let mut d = FakeDaemon::default();
        let ev = c.capture(&mut d, Some("quiet-game")).unwrap();
        assert_eq!(links_of(&ev), [0]);
        assert_eq!(
            d.sink_channels,
            ["FL", "FR"],
            "stereo when nothing says otherwise"
        );
        let again = c.capture(&mut d, Some("quiet-game")).unwrap();
        assert_eq!(links_of(&again), [0], "even unchanged");
    }

    #[test]
    fn churn_re_links_and_reports_only_changes() {
        let (mut c, mut d) = with_game();
        c.capture(&mut d, Some("game")).unwrap();
        // The game's stream closes (menu): links drop to zero.
        c.apply([33, 43, 44].map(RegistryEvent::Remove));
        let ev = c.changed(&mut d);
        assert_eq!(links_of(&ev), [0]);
        assert!(
            ev.iter()
                .any(|e| matches!(e, Event::Apps(a) if a.is_empty()))
        );
        // Nothing changed: nothing reported.
        assert!(c.changed(&mut d).is_empty());
        // Back in game with new ids.
        let mut g2 = Graph::new();
        emitter(&mut g2, 32, 70, "game", "Game", &[71, 72]);
        c.apply([
            RegistryEvent::Add(70, Kind::Node, g2.objects[&70].props.clone()),
            RegistryEvent::Add(71, Kind::Port, g2.objects[&71].props.clone()),
            RegistryEvent::Add(72, Kind::Port, g2.objects[&72].props.clone()),
        ]);
        assert_eq!(links_of(&c.changed(&mut d)), [2]);
    }

    #[test]
    fn a_refused_link_is_reported_and_never_fatal() {
        let (mut c, mut d) = with_game();
        d.refuse_links = true;
        let ev = c.capture(&mut d, Some("game")).unwrap();
        assert!(
            ev.iter()
                .any(|e| matches!(e, Event::Links { error: Some(_), .. }))
        );
        assert_eq!(c.link_count(), 0);
    }

    #[test]
    fn release_drops_links_and_teardown_drops_everything() {
        let (mut c, mut d) = with_game();
        c.capture(&mut d, Some("game")).unwrap();
        assert_eq!(d.live.len(), 3, "sink + two links");
        assert_eq!(
            c.release(&mut d),
            Event::Links {
                binary: String::new(),
                links: 0,
                error: None
            }
        );
        assert_eq!(d.live.len(), 1, "the sink stays");
        c.teardown(&mut d);
        assert!(d.live.is_empty());
        assert!(c.capture(&mut d, Some("")).is_err());
    }

    #[test]
    fn the_sinks_removal_forgets_it() {
        let (mut c, mut d) = with_game();
        c.capture(&mut d, Some("game")).unwrap();
        c.apply([RegistryEvent::Remove(900)]);
        assert_eq!(c.sink_serial(), None);
    }
}
