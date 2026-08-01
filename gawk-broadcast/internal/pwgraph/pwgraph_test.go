package pwgraph

import (
	"testing"
)

// Helpers that spell registry globals the way a daemon actually sends them —
// every value a string, because a spa_dict has no other type.
func node(id uint32, props map[string]string) Global {
	return Global{ID: id, Kind: KindNode, Props: props}
}
func client(id uint32, props map[string]string) Global {
	return Global{ID: id, Kind: KindClient, Props: props}
}
func port(id, nodeID, index uint32, dir, channel string, monitor bool) Global {
	p := map[string]string{
		KeyNodeID:    itoa(nodeID),
		KeyPortID:    itoa(index),
		KeyPortDir:   dir,
		KeyChannel:   channel,
		KeySerial:    itoa(id * 10),
		KeyPortMon:   "false",
		"port.alias": "test",
	}
	if monitor {
		p[KeyPortMon] = "true"
	}
	return Global{ID: id, Kind: KindPort, Props: p}
}
func itoa(v uint32) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{digits[v%10]}, b...)
		v /= 10
	}
	return string(b)
}

// A stereo application stream owned by a client — the common shape.
func addEmitter(g *Graph, clientID, nodeID uint32, binary, appName string, ports ...uint32) {
	g.Add(client(clientID, map[string]string{KeyAppBinary: binary, KeyAppName: appName}))
	g.Add(node(nodeID, map[string]string{
		KeyMediaClass: ClassStreamOutput,
		KeyNodeName:   binary,
		KeyClientID:   itoa(clientID),
		KeySerial:     itoa(nodeID * 3),
	}))
	channels := []string{"FL", "FR"}
	for i, p := range ports {
		ch := "UNK"
		if i < len(channels) {
			ch = channels[i]
		}
		g.Add(port(p, nodeID, uint32(i), "out", ch, false))
	}
}

// A stereo sink with the monitor ports a null-audio-sink grows.
func addSink(g *Graph, nodeID uint32, name, class string, inPorts, monPorts []uint32) {
	g.Add(node(nodeID, map[string]string{
		KeyMediaClass: class,
		KeyNodeName:   name,
		KeySerial:     itoa(nodeID * 3),
	}))
	channels := []string{"FL", "FR", "FC", "LFE", "SL", "SR"}
	for i, p := range inPorts {
		g.Add(port(p, nodeID, uint32(i), "in", channels[i%len(channels)], false))
	}
	for i, p := range monPorts {
		g.Add(port(p, nodeID, uint32(i), "out", channels[i%len(channels)], true))
	}
}

// The measured fact this whole package is shaped around: a stream node's
// registry globals often carry no `application.process.binary` at all — only
// `client.id` — while the owning Client's globals always do. Verified against a
// real daemon 2026-08-01 with a GStreamer emitter, which is exactly what CI's
// synthetic emitters are.
func TestIdentityResolvesThroughTheOwningClient(t *testing.T) {
	g := New()
	addEmitter(g, 32, 33, "supertuxkart", "SuperTuxKart", 43, 44)

	apps := g.Apps()
	if len(apps) != 1 {
		t.Fatalf("Apps() = %+v, want one app", apps)
	}
	if apps[0].Binary != "supertuxkart" {
		t.Errorf("binary = %q, want supertuxkart (resolved through the client)", apps[0].Binary)
	}
	if apps[0].Name != "SuperTuxKart" {
		t.Errorf("name = %q, want SuperTuxKart", apps[0].Name)
	}
	if apps[0].Streams != 1 {
		t.Errorf("streams = %d, want 1", apps[0].Streams)
	}
}

// A node that carries the property itself wins over its client's — pulse
// clients set it per-stream, and per-stream is the more specific claim.
func TestNodePropertyBeatsTheClientProperty(t *testing.T) {
	g := New()
	g.Add(client(10, map[string]string{KeyAppBinary: "pipewire-pulse", KeyAppName: "PulseAudio"}))
	g.Add(node(11, map[string]string{
		KeyMediaClass: ClassStreamOutput,
		KeyClientID:   "10",
		KeyAppBinary:  "firefox",
		KeyAppName:    "Firefox",
	}))
	apps := g.Apps()
	if len(apps) != 1 || apps[0].Binary != "firefox" || apps[0].Name != "Firefox" {
		t.Fatalf("Apps() = %+v, want the node's own firefox identity", apps)
	}
}

// Registry bursts are not ordered: a game launching sends nodes, ports and
// clients interleaved, and a port can arrive before the node it belongs to.
// Every ordering must converge on the same graph.
func TestEventOrderingDoesNotChangeTheOutcome(t *testing.T) {
	globals := []Global{
		port(43, 33, 0, "out", "FL", false),
		node(33, map[string]string{KeyMediaClass: ClassStreamOutput, KeyClientID: "32", KeySerial: "40"}),
		port(44, 33, 1, "out", "FR", false),
		client(32, map[string]string{KeyAppBinary: "game", KeyAppName: "Game"}),
	}
	// Forwards, and backwards — the two orders that would break a graph that
	// resolved identity eagerly instead of on read.
	forward, backward := New(), New()
	for _, gl := range globals {
		forward.Add(gl)
	}
	for i := len(globals) - 1; i >= 0; i-- {
		backward.Add(globals[i])
	}

	for name, g := range map[string]*Graph{"forward": forward, "backward": backward} {
		apps := g.Apps()
		if len(apps) != 1 || apps[0].Binary != "game" {
			t.Fatalf("%s: Apps() = %+v, want one app named game", name, apps)
		}
		addSink(g, 48, "gawk-app-capture-1", "Audio/Sink/Internal", []uint32{49, 51}, []uint32{50, 52})
		if links := g.Plan("game", 48); len(links) != 2 {
			t.Errorf("%s: Plan() = %+v, want 2 links", name, links)
		}
	}
}

// PipeWire reuses global ids after a removal. A helper that treated a repeated
// id as a duplicate would go blind to the new object for the rest of the
// session — the exact failure a game restart produces.
func TestReusedIdsReplaceRatherThanDuplicate(t *testing.T) {
	g := New()
	addEmitter(g, 32, 33, "game", "Game", 43, 44)
	// The game exits and something else takes id 33 — with a different owner.
	g.Remove(33)
	g.Remove(43)
	g.Remove(44)
	g.Remove(32)
	addEmitter(g, 32, 33, "browser", "Browser", 43, 44)

	apps := g.Apps()
	if len(apps) != 1 || apps[0].Binary != "browser" {
		t.Fatalf("Apps() = %+v, want only browser", apps)
	}
}

// Two streams from one binary are one row with a count of two — the
// multi-stream case that single-node capture cannot serve (docs/39 §7), and
// which must produce links for *both*.
func TestMultipleStreamsFromOneBinary(t *testing.T) {
	g := New()
	addEmitter(g, 32, 33, "game", "Game", 43, 44)
	// The same client opens a second stream (music + effects).
	g.Add(node(60, map[string]string{
		KeyMediaClass: ClassStreamOutput,
		KeyClientID:   "32",
		KeySerial:     "70",
	}))
	g.Add(port(61, 60, 0, "out", "FL", false))
	g.Add(port(62, 60, 1, "out", "FR", false))

	apps := g.Apps()
	if len(apps) != 1 || apps[0].Streams != 2 {
		t.Fatalf("Apps() = %+v, want one row with 2 streams", apps)
	}
	addSink(g, 48, "gawk", "Audio/Sink/Internal", []uint32{49, 51}, []uint32{50, 52})
	links := g.Plan("game", 48)
	if len(links) != 4 {
		t.Fatalf("Plan() = %+v, want 4 links (2 streams x 2 channels)", links)
	}
}

// Channels are matched by name, because a raw port link performs no channel
// mixing: a mismatch does not resample, it silently drops a channel.
func TestPlanMatchesChannelsByName(t *testing.T) {
	g := New()
	g.Add(client(32, map[string]string{KeyAppBinary: "game"}))
	g.Add(node(33, map[string]string{KeyMediaClass: ClassStreamOutput, KeyClientID: "32"}))
	// A stream whose ports arrive in reverse channel order.
	g.Add(port(43, 33, 0, "out", "FR", false))
	g.Add(port(44, 33, 1, "out", "FL", false))
	addSink(g, 48, "gawk", "Audio/Sink/Internal", []uint32{49, 51}, []uint32{50, 52}) // 49=FL, 51=FR

	links := g.Plan("game", 48)
	if len(links) != 2 {
		t.Fatalf("Plan() = %+v, want 2 links", links)
	}
	want := map[uint32]uint32{43: 51, 44: 49} // FR→FR, FL→FL
	for _, l := range links {
		if want[l.OutPort] != l.InPort {
			t.Errorf("port %d linked to %d, want %d (channels crossed)", l.OutPort, l.InPort, want[l.OutPort])
		}
	}
}

// A mono application must be audible on both sides, not only the left.
func TestPlanSpreadsASingleUnmatchedPortAcrossTheSink(t *testing.T) {
	g := New()
	g.Add(client(32, map[string]string{KeyAppBinary: "beeper"}))
	g.Add(node(33, map[string]string{KeyMediaClass: ClassStreamOutput, KeyClientID: "32"}))
	g.Add(port(43, 33, 0, "out", "MONO", false))
	addSink(g, 48, "gawk", "Audio/Sink/Internal", []uint32{49, 51}, []uint32{50, 52})

	links := g.Plan("beeper", 48)
	if len(links) != 2 {
		t.Fatalf("Plan() = %+v, want the mono port spread across both sink inputs", links)
	}
	for _, l := range links {
		if l.OutPort != 43 {
			t.Errorf("unexpected output port %d", l.OutPort)
		}
	}
}

// Unnamed multi-channel streams pair positionally rather than being dropped.
func TestPlanFallsBackToPositionalPairing(t *testing.T) {
	g := New()
	g.Add(client(32, map[string]string{KeyAppBinary: "odd"}))
	g.Add(node(33, map[string]string{KeyMediaClass: ClassStreamOutput, KeyClientID: "32"}))
	g.Add(port(43, 33, 0, "out", "UNK", false))
	g.Add(port(44, 33, 1, "out", "UNK", false))
	addSink(g, 48, "gawk", "Audio/Sink/Internal", []uint32{49, 51}, []uint32{50, 52})

	links := g.Plan("odd", 48)
	if len(links) != 2 {
		t.Fatalf("Plan() = %+v, want 2 positional links", links)
	}
	if links[0].OutPort != 43 || links[0].InPort != 49 || links[1].OutPort != 44 || links[1].InPort != 51 {
		t.Errorf("positional pairing = %+v, want 43→49 and 44→51", links)
	}
}

// The sink's own monitor ports are outputs too. Linking them into the sink
// would be a feedback loop, and they must never appear in a plan.
func TestPlanNeverLinksMonitorPorts(t *testing.T) {
	g := New()
	addSink(g, 48, "gawk", "Audio/Sink/Internal", []uint32{49, 51}, []uint32{50, 52})
	// A hostile shape: a "stream" that is actually our own sink's monitor.
	if ins := g.SinkInputs(48); len(ins) != 2 {
		t.Fatalf("SinkInputs = %+v, want the two playback ports only", ins)
	}
	for _, p := range g.SinkInputs(48) {
		if p.Monitor || p.Out {
			t.Errorf("SinkInputs returned a monitor/output port: %+v", p)
		}
	}
}

// A stream whose owner cannot be identified is not shown: AD2's identity *is*
// that string, so a row the helper could never re-link after a restart would be
// a promise it cannot keep.
func TestUnidentifiableStreamsAreNotOffered(t *testing.T) {
	g := New()
	g.Add(node(33, map[string]string{KeyMediaClass: ClassStreamOutput, KeySerial: "40"}))
	g.Add(port(43, 33, 0, "out", "FL", false))
	if apps := g.Apps(); len(apps) != 0 {
		t.Errorf("Apps() = %+v, want none", apps)
	}
}

// Only playing applications are listed. A recording stream (our own capture,
// a voice-chat input) is an *input* and must never appear.
func TestOnlyOutputStreamsAreListed(t *testing.T) {
	g := New()
	g.Add(client(32, map[string]string{KeyAppBinary: "recorder", KeyAppName: "Recorder"}))
	g.Add(node(33, map[string]string{
		KeyMediaClass: "Stream/Input/Audio",
		KeyClientID:   "32",
	}))
	addSink(g, 48, "speakers", "Audio/Sink", []uint32{49, 51}, []uint32{50, 52})
	if apps := g.Apps(); len(apps) != 0 {
		t.Errorf("Apps() = %+v, want none (an input stream is not an emitter)", apps)
	}
}

// The list is stable: the GUI renders it live and rows must not dance when an
// unrelated stream appears.
func TestAppsAreSortedStably(t *testing.T) {
	g := New()
	addEmitter(g, 10, 11, "zsh-bell", "zsh", 12)
	addEmitter(g, 20, 21, "aisleriot", "Aisleriot", 22)
	addEmitter(g, 30, 31, "mpv", "mpv", 32)
	got := g.Apps()
	want := []string{"aisleriot", "mpv", "zsh-bell"}
	if len(got) != len(want) {
		t.Fatalf("Apps() = %+v, want %v", got, want)
	}
	for i := range want {
		if got[i].Binary != want[i] {
			t.Errorf("Apps()[%d] = %q, want %q", i, got[i].Binary, want[i])
		}
	}
}

// The capture sink's layout mirrors the *default* sink's, which is read from
// the graph rather than assumed (D3).
func TestDefaultSinkChannelsReadsTheRealLayout(t *testing.T) {
	g := New()
	addSink(g, 30, "speakers", "Audio/Sink", []uint32{39, 41, 60, 61, 62, 63}, []uint32{40, 42})
	got := g.DefaultSinkChannels("speakers")
	want := []string{"FL", "FR", "FC", "LFE", "SL", "SR"}
	if len(got) != len(want) {
		t.Fatalf("DefaultSinkChannels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("channel %d = %q, want %q", i, got[i], want[i])
		}
	}

	// An unknown or unset default is "no answer", and the caller falls back to
	// stereo rather than inventing a layout.
	if got := g.DefaultSinkChannels("nonexistent"); got != nil {
		t.Errorf("DefaultSinkChannels(unknown) = %v, want nil", got)
	}
	if got := g.DefaultSinkChannels(""); got != nil {
		t.Errorf("DefaultSinkChannels(\"\") = %v, want nil", got)
	}
}

// The whole churn story in one test: a plan is a pure function of the graph, so
// a stream dying and coming back needs no bookkeeping to stay correct.
func TestPlanFollowsChurn(t *testing.T) {
	g := New()
	addSink(g, 48, "gawk", "Audio/Sink/Internal", []uint32{49, 51}, []uint32{50, 52})
	addEmitter(g, 32, 33, "game", "Game", 43, 44)

	if links := g.Plan("game", 48); len(links) != 2 {
		t.Fatalf("initial plan = %+v, want 2 links", links)
	}
	// In-game menu: the stream closes.
	g.Remove(43)
	g.Remove(44)
	g.Remove(33)
	if links := g.Plan("game", 48); len(links) != 0 {
		t.Fatalf("plan with no streams = %+v, want none", links)
	}
	// Back in game: a new node with new ids, same binary.
	g.Add(node(70, map[string]string{KeyMediaClass: ClassStreamOutput, KeyClientID: "32", KeySerial: "80"}))
	g.Add(port(71, 70, 0, "out", "FL", false))
	g.Add(port(72, 70, 1, "out", "FR", false))
	links := g.Plan("game", 48)
	if len(links) != 2 {
		t.Fatalf("plan after restart = %+v, want 2 links", links)
	}
	for _, l := range links {
		if l.OutNode != 70 {
			t.Errorf("link from stale node %d", l.OutNode)
		}
	}
}

// No sink, no plan — and no panic. The helper asks for a plan on every registry
// change, including the ones that arrive before the sink's own global does.
func TestPlanWithoutASinkIsEmpty(t *testing.T) {
	g := New()
	addEmitter(g, 32, 33, "game", "Game", 43, 44)
	if links := g.Plan("game", 999); len(links) != 0 {
		t.Errorf("Plan with an unknown sink = %+v, want none", links)
	}
	if links := g.Plan("", 999); len(links) != 0 {
		t.Errorf("Plan with no target = %+v, want none", links)
	}
}
