// Package pwgraph is the PipeWire helper's brain: registry bookkeeping, the
// emitting-application list, and the port-link plan that captures one
// application's audio (R35, docs/39 D3).
//
// It is deliberately pure Go with no cgo and no daemon. Everything that is
// *logic* — who is emitting, which ports pair with which, what changed since
// last time — lives here and is unit-tested daemon-free, because the parts that
// break under a game launch are ordering and identity, not the C calls. The
// libpipewire side (cmd/gawk-pw-helper) only forwards registry globals into
// Add/Remove and executes the plan this package computes.
//
// The one rule worth stating twice: **links are a tee, never a re-route**.
// PipeWire output ports fan out, so linking an application's ports into our
// sink gives us a copy while the application keeps playing to the speakers.
// Nothing here ever moves a stream, and no failure mode of this package can
// leave a user with silent speakers.
package pwgraph

import (
	"cmp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
)

// PipeWire property keys this package reads. Spelled once, here, because a
// typo in one of these is a feature that silently does nothing.
const (
	KeyMediaClass = "media.class"
	KeyNodeName   = "node.name"
	KeyNodeDesc   = "node.description"
	KeyClientID   = "client.id"
	KeyNodeID     = "node.id"
	KeySerial     = "object.serial"
	KeyPortDir    = "port.direction"
	KeyPortID     = "port.id"
	KeyPortMon    = "port.monitor"
	KeyChannel    = "audio.channel"
	KeyAppName    = "application.name"
	KeyAppBinary  = "application.process.binary"
)

// ClassStreamOutput is the media class of an application playing audio.
const ClassStreamOutput = "Stream/Output/Audio"

// Kind is which registry interface a global is. Everything the helper does not
// track is KindOther and costs one map lookup to ignore.
type Kind int

const (
	KindOther Kind = iota
	KindNode
	KindPort
	KindClient
)

// Global is one registry object exactly as PipeWire announced it.
//
// Props is the registry's *global* property dict, which is a **subset** of what
// the object knows. That distinction is load-bearing and was measured rather
// than assumed (PipeWire 1.0.5, 2026-08-01): neither a Stream node's globals nor
// its Client's carry `application.process.binary` — a Client's stop at
// `application.name` — so AD2's identity is reachable only by *binding* the
// object and reading the info event's properties, which arrive here through
// Merge. A helper that trusted registry globals alone would show an app list
// it could not link.
type Global struct {
	ID    uint32
	Kind  Kind
	Props map[string]string
}

// Node is a tracked PipeWire node.
type Node struct {
	ID         uint32
	Serial     uint32
	MediaClass string
	Name       string
	Desc       string
	ClientID   uint32
	// Binary and AppName are whatever the *node* claimed. Empty is normal;
	// Graph resolves through the client.
	Binary  string
	AppName string
}

// Port is a tracked PipeWire port.
type Port struct {
	ID     uint32
	NodeID uint32
	// Index is `port.id`, the node-local ordinal. It is the tie-breaker for
	// positional matching when channels do not name themselves.
	Index   uint32
	Out     bool
	Monitor bool
	Channel string
}

// Client is a tracked PipeWire client — the object that reliably knows which
// binary is making the noise.
type Client struct {
	ID      uint32
	Binary  string
	AppName string
}

// Graph is the tracked slice of the registry. Not safe for concurrent use: the
// helper drives it from one goroutine, which is cheaper to guarantee than a
// lock is to reason about.
//
// Objects are stored as raw property maps and the typed views below are derived
// on read. That is what makes Merge cheap and correct: an object's identity
// arrives in two instalments — the registry's global properties first, the
// bound object's fuller list a moment later — and a graph that had already
// baked the first into a struct would have to know how to un-bake it.
type Graph struct {
	objects map[uint32]object
}

type object struct {
	kind  Kind
	props map[string]string
}

func New() *Graph {
	return &Graph{objects: map[uint32]object{}}
}

// Add records a global, replacing any previous record for that id.
//
// Replacing rather than rejecting is deliberate: PipeWire reuses ids after a
// removal, and a helper that treated a reused id as a duplicate would go blind
// to the new object for the rest of the session — which is exactly what a game
// restart produces.
func (g *Graph) Add(gl Global) {
	props := make(map[string]string, len(gl.Props))
	for k, v := range gl.Props {
		props[k] = v
	}
	g.objects[gl.ID] = object{kind: gl.Kind, props: props}
}

// Merge overlays the fuller property list a *bound* object reports over what
// the registry global carried.
//
// This is not an optimisation; it is the only path to
// `application.process.binary` for a large class of applications. Verified
// against PipeWire 1.0.5 on 2026-08-01: a Client's registry globals stop at
// `application.name`, and the binary — AD2's identity — appears only here.
//
// Merging an id we have never seen is a no-op rather than an insert: an info
// event without its global is a bind racing a removal, and inventing an object
// of unknown kind from it would put a phantom in the list.
func (g *Graph) Merge(id uint32, props map[string]string) {
	o, ok := g.objects[id]
	if !ok {
		return
	}
	for k, v := range props {
		o.props[k] = v
	}
	g.objects[id] = o
}

// Remove forgets a global. Ids are unique across kinds in PipeWire's registry,
// so one call clears whichever kind holds it — and removing an id we never saw
// is a no-op, which is what an out-of-order start-up burst looks like.
func (g *Graph) Remove(id uint32) {
	delete(g.objects, id)
}

// node derives the typed view of a node.
func (g *Graph) node(id uint32, o object) Node {
	return Node{
		ID:         id,
		Serial:     u32(o.props[KeySerial]),
		MediaClass: o.props[KeyMediaClass],
		Name:       o.props[KeyNodeName],
		Desc:       o.props[KeyNodeDesc],
		ClientID:   u32(o.props[KeyClientID]),
		Binary:     o.props[KeyAppBinary],
		AppName:    o.props[KeyAppName],
	}
}

func (g *Graph) port(id uint32, o object) Port {
	return Port{
		ID:      id,
		NodeID:  u32(o.props[KeyNodeID]),
		Index:   u32(o.props[KeyPortID]),
		Out:     o.props[KeyPortDir] == "out",
		Monitor: o.props[KeyPortMon] == "true",
		Channel: o.props[KeyChannel],
	}
}

func (g *Graph) client(id uint32, o object) Client {
	return Client{
		ID:      id,
		Binary:  o.props[KeyAppBinary],
		AppName: o.props[KeyAppName],
	}
}

// nodes iterates the tracked nodes.
func (g *Graph) nodes(fn func(Node)) {
	for id, o := range g.objects {
		if o.kind == KindNode {
			fn(g.node(id, o))
		}
	}
}

// clientByID returns a tracked client.
func (g *Graph) clientByID(id uint32) (Client, bool) {
	o, ok := g.objects[id]
	if !ok || o.kind != KindClient {
		return Client{}, false
	}
	return g.client(id, o), true
}

// Node returns a tracked node.
func (g *Graph) Node(id uint32) (Node, bool) {
	o, ok := g.objects[id]
	if !ok || o.kind != KindNode {
		return Node{}, false
	}
	return g.node(id, o), true
}

// NodeBySerial finds a node by its object.serial — the id a gst pipeline
// addresses, and the one that is never reused within a daemon's lifetime.
func (g *Graph) NodeBySerial(serial uint32) (Node, bool) {
	var found Node
	var ok bool
	g.nodes(func(n Node) {
		if !ok && n.Serial == serial {
			found, ok = n, true
		}
	})
	return found, ok
}

// FindSinkByName returns the node with this exact node.name, which is how the
// helper recognizes the sink it just asked the daemon to create: create_object
// hands back a proxy, and the *global* arrives separately through the registry.
func (g *Graph) FindSinkByName(name string) (Node, bool) {
	var found Node
	var ok bool
	g.nodes(func(n Node) {
		if !ok && n.Name == name && strings.HasPrefix(n.MediaClass, "Audio/Sink") {
			found, ok = n, true
		}
	})
	return found, ok
}

// Binary resolves a node's owning binary: the node's own property when it has
// one, otherwise its client's. See Global's comment for why the second path is
// the common one rather than the fallback.
func (g *Graph) Binary(n Node) string {
	if n.Binary != "" {
		return n.Binary
	}
	if c, ok := g.clientByID(n.ClientID); ok {
		return c.Binary
	}
	return ""
}

// DisplayName is what the whose-audio card shows. Never empty for a node the
// list contains, because a blank row is unpickable.
func (g *Graph) DisplayName(n Node) string {
	if n.AppName != "" {
		return n.AppName
	}
	if c, ok := g.clientByID(n.ClientID); ok && c.AppName != "" {
		return c.AppName
	}
	if n.Desc != "" {
		return n.Desc
	}
	if n.Name != "" {
		return n.Name
	}
	return g.Binary(n)
}

// Apps lists the applications currently emitting audio, one row per binary,
// sorted for a stable render.
//
// The list is built from **live output streams**, which is the property that
// makes the whole two-step flow safe: the user picks whoever is actually making
// sound, so the Windows sibling's "picked the game, audio lives in a helper
// process" trap (docs/38 V-3a) mostly cannot happen here (docs/39 D5). A node
// whose binary cannot be resolved at all is skipped rather than shown as a
// blank row — it could not be re-linked after a restart anyway, since AD2's
// identity is exactly that missing string.
func (g *Graph) Apps() []pwproto.App {
	byBinary := map[string]*pwproto.App{}
	g.nodes(func(n Node) {
		if n.MediaClass != ClassStreamOutput {
			return
		}
		bin := g.Binary(n)
		if bin == "" {
			return
		}
		app, ok := byBinary[bin]
		if !ok {
			app = &pwproto.App{Binary: bin, Name: g.DisplayName(n)}
			byBinary[bin] = app
		}
		app.Streams++
	})
	out := make([]pwproto.App, 0, len(byBinary))
	for _, a := range byBinary {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(a, b pwproto.App) int {
		if c := cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return cmp.Compare(a.Binary, b.Binary)
	})
	return out
}

// StreamNodes returns the audio-emitting nodes belonging to one binary.
func (g *Graph) StreamNodes(binary string) []Node {
	var out []Node
	g.nodes(func(n Node) {
		if n.MediaClass == ClassStreamOutput && binary != "" && g.Binary(n) == binary {
			out = append(out, n)
		}
	})
	slices.SortFunc(out, func(a, b Node) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// nodePorts returns a node's ports in a stable order (by node-local index,
// then id), filtered by direction and monitor-ness.
func (g *Graph) nodePorts(nodeID uint32, out, monitor bool) []Port {
	var ps []Port
	for id, o := range g.objects {
		if o.kind != KindPort {
			continue
		}
		p := g.port(id, o)
		if p.NodeID == nodeID && p.Out == out && p.Monitor == monitor {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Index != ps[j].Index {
			return ps[i].Index < ps[j].Index
		}
		return ps[i].ID < ps[j].ID
	})
	return ps
}

// SinkInputs returns the playback ports of the capture sink.
func (g *Graph) SinkInputs(sinkNodeID uint32) []Port {
	return g.nodePorts(sinkNodeID, false, false)
}

// Link is one port-to-port connection to create.
type Link struct {
	OutNode, OutPort uint32
	InNode, InPort   uint32
}

// Plan computes every link that should exist from binary's streams into the
// sink. It is a pure function of the current graph: the caller diffs it against
// what it has already created, so a stream appearing or dying is one recompute
// and a small delta — no bookkeeping to get out of step with reality.
//
// Channel matching is by name (`audio.channel`), which is what makes the sink's
// layout matter: raw port-to-port links perform no channel mixing, so a stereo
// sink under a 5.1 game would simply drop the centre channel — where the
// dialogue lives (D3). Two fallbacks cover streams that do not name channels
// the same way:
//
//   - a single unmatched output port is spread to *every* sink input, so a mono
//     application is audible on both sides rather than only on the left;
//   - otherwise ports pair positionally, which is better than dropping them and
//     is what a stream with generic channel names ("UNK") gets.
func (g *Graph) Plan(binary string, sinkNodeID uint32) []Link {
	sinkPorts := g.SinkInputs(sinkNodeID)
	if len(sinkPorts) == 0 {
		return nil
	}
	byChannel := map[string]Port{}
	for _, p := range sinkPorts {
		if p.Channel != "" {
			byChannel[p.Channel] = p
		}
	}

	var links []Link
	for _, n := range g.StreamNodes(binary) {
		outs := g.nodePorts(n.ID, true, false)
		if len(outs) == 0 {
			continue
		}
		matched := 0
		for _, op := range outs {
			if ip, ok := byChannel[op.Channel]; ok && op.Channel != "" {
				links = append(links, Link{OutNode: n.ID, OutPort: op.ID, InNode: sinkNodeID, InPort: ip.ID})
				matched++
			}
		}
		if matched > 0 {
			continue
		}
		if len(outs) == 1 {
			// Mono (or an unnamed single channel): audible on every side.
			for _, ip := range sinkPorts {
				links = append(links, Link{OutNode: n.ID, OutPort: outs[0].ID, InNode: sinkNodeID, InPort: ip.ID})
			}
			continue
		}
		for i := 0; i < len(outs) && i < len(sinkPorts); i++ {
			links = append(links, Link{OutNode: n.ID, OutPort: outs[i].ID, InNode: sinkNodeID, InPort: sinkPorts[i].ID})
		}
	}
	slices.SortFunc(links, func(a, b Link) int {
		if c := cmp.Compare(a.OutPort, b.OutPort); c != 0 {
			return c
		}
		return cmp.Compare(a.InPort, b.InPort)
	})
	return links
}

// StreamChannels reports the channel layout a binary's audio streams currently
// speak, widest stream first.
//
// This is the primary input to the capture sink's layout, and it is a
// refinement of D3 rather than a departure from it: D3 wants the sink to match
// the default sink's layout *because* that is the layout the application's
// ports negotiated, and raw port links perform no channel mixing — a narrower
// sink drops the centre channel, where the dialogue lives. Reading the
// application's own ports asks that question directly, needs no metadata
// binding, and is right even when the application is not playing to the
// default sink at all.
func (g *Graph) StreamChannels(binary string) []string {
	var widest []string
	for _, n := range g.StreamNodes(binary) {
		var chans []string
		for _, p := range g.nodePorts(n.ID, true, false) {
			if p.Channel == "" || p.Channel == "UNK" {
				continue
			}
			chans = append(chans, p.Channel)
		}
		if len(chans) > len(widest) {
			widest = chans
		}
	}
	return widest
}

// WidestSinkChannels reports the widest layout among the machine's real audio
// sinks, ignoring internal ones (ours, and anyone else's capture plumbing).
//
// It is the fallback for an application that has no streams open at the moment
// capture starts: matching the hardware's layout is the same bet D3 makes with
// the default sink, without needing the daemon's metadata.
func (g *Graph) WidestSinkChannels() []string {
	var widest []string
	g.nodes(func(n Node) {
		if n.MediaClass != "Audio/Sink" {
			return
		}
		var chans []string
		for _, p := range g.nodePorts(n.ID, false, false) {
			if p.Channel == "" {
				continue
			}
			chans = append(chans, p.Channel)
		}
		if len(chans) > len(widest) {
			widest = chans
		}
	})
	return widest
}

// PlanSystemAudio links the machine's real output sink's **monitor** ports into
// the capture sink — the whole system's sound, arriving through the same node
// the gst pipeline is already reading.
//
// This is what makes docs/39 D5's mid-session "switch to whole-system audio" a
// re-link rather than a renegotiation: the pipeline's target-object never
// changes, so the Opus stream, its sequence space and the viewer's decoder all
// carry on undisturbed and the switch costs nothing but the packets in the gap.
//
// A monitor is an output that carries what the sink is playing, so this is the
// same tee shape as the application case and equally incapable of re-routing
// anything.
func (g *Graph) PlanSystemAudio(sinkNodeID uint32) []Link {
	sinkPorts := g.SinkInputs(sinkNodeID)
	if len(sinkPorts) == 0 {
		return nil
	}
	source, ok := g.widestRealSink()
	if !ok {
		return nil
	}
	byChannel := map[string]Port{}
	for _, p := range sinkPorts {
		if p.Channel != "" {
			byChannel[p.Channel] = p
		}
	}
	monitors := g.nodePorts(source.ID, true, true)
	var links []Link
	for i, mp := range monitors {
		if ip, ok := byChannel[mp.Channel]; ok && mp.Channel != "" {
			links = append(links, Link{OutNode: source.ID, OutPort: mp.ID, InNode: sinkNodeID, InPort: ip.ID})
			continue
		}
		if i < len(sinkPorts) {
			links = append(links, Link{OutNode: source.ID, OutPort: mp.ID, InNode: sinkNodeID, InPort: sinkPorts[i].ID})
		}
	}
	slices.SortFunc(links, func(a, b Link) int {
		if c := cmp.Compare(a.OutPort, b.OutPort); c != 0 {
			return c
		}
		return cmp.Compare(a.InPort, b.InPort)
	})
	return links
}

// widestRealSink returns the machine's most capable actual output, ignoring
// internal sinks — ours, and anyone else's capture plumbing.
func (g *Graph) widestRealSink() (Node, bool) {
	var best Node
	bestPorts := 0
	g.nodes(func(n Node) {
		if n.MediaClass != "Audio/Sink" {
			return
		}
		if p := len(g.nodePorts(n.ID, false, false)); p > bestPorts {
			best, bestPorts = n, p
		}
	})
	return best, bestPorts > 0
}

// DefaultSinkChannels reports the channel layout of the node the daemon calls
// the default audio sink, as a positional list ("FL", "FR", …).
//
// The capture sink is created with this layout rather than a hardcoded stereo,
// which is D3's load-bearing detail. name comes from the daemon's
// `default.audio.sink` metadata; when it is empty or unknown the caller falls
// back to stereo, which is right far more often than it is wrong and is never
// worse than what a hardcoded stereo would have done anyway.
func (g *Graph) DefaultSinkChannels(name string) []string {
	if name == "" {
		return nil
	}
	var out []string
	found := false
	g.nodes(func(n Node) {
		if found || n.Name != name || !strings.HasPrefix(n.MediaClass, "Audio/Sink") {
			return
		}
		found = true
		for _, p := range g.nodePorts(n.ID, false, false) {
			if p.Channel == "" {
				continue
			}
			out = append(out, p.Channel)
		}
	})
	return out
}

func u32(s string) uint32 {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}
