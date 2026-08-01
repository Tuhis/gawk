// Command gawk-pw-helper captures one application's audio into a virtual
// PipeWire sink, so the broadcaster can publish that application's sound and
// nothing else (R35, docs/39 D3/D4).
//
// It is a helper, not a product: gawk-broadcast spawns it, speaks
// newline-delimited JSON over its stdin/stdout (internal/pwproto), and closing
// the pipe is how it ends. Running it by hand is a debugging tool —
//
//	echo '{"op":"watch"}' | gawk-pw-helper
//
// prints the list of applications currently playing audio and keeps printing
// it as that list changes.
//
// # Why it is a separate process
//
// Two reasons, both settled elsewhere and inherited here. Crash isolation is
// this module's posture (docs/19 Decision 3, the reason GStreamer is a child):
// a libpipewire loop that wedges must not take the broadcast with it. And
// cleanup: every object created here is a proxy on *this process's* connection
// with no `object.linger`, so the daemon destroys the sink and every link when
// the connection closes — clean exit, SIGKILL, or OOM alike. There is no exit
// path that leaves a stranger's sound server dirty, because the cleanup is the
// daemon's reaction to a closed socket rather than anything this code runs.
//
// # What it never does
//
// It never moves anyone's audio. Links are a tee: PipeWire output ports fan
// out, so the captured application keeps playing to the speakers and this sink
// receives a copy. No media flows through this process — it is a control plane,
// and if it dies, audio degrades while video does not notice.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwgraph"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/version"
)

// roundTripTimeout bounds a wait on the daemon. Generously long — a busy
// sound server under a game launch is not a hung one — but bounded, because a
// helper that hangs is worse than a helper that reports failure: the engine's
// degrade path (docs/39 D6) is immediate and silent, and a hang defers it.
const roundTripTimeout = 5 * time.Second

func main() {
	fs := flag.NewFlagSet("gawk-pw-helper", flag.ExitOnError)
	showVer := fs.Bool("version", false, "print the build version and exit")
	_ = fs.Parse(os.Args[1:])
	if *showVer {
		fmt.Printf("gawk-pw-helper v%s (libpipewire %s)\n", version.String(), libraryVersion())
		return
	}

	out := pwproto.NewWriter(os.Stdout)
	if err := run(os.Stdin, out); err != nil {
		_ = out.Write(pwproto.Event{Event: pwproto.EventFatal, Message: err.Error()})
		// Exit 0 even on a fatal: the engine reads the event, not the status,
		// and a non-zero exit would make the supervising log noisier about a
		// condition that is by design not fatal to the broadcast.
		os.Exit(0)
	}
}

func run(stdin io.Reader, out *pwproto.Writer) error {
	c, err := connect()
	if err != nil {
		return err
	}
	defer c.close()

	h := &helper{
		conn:  c,
		out:   out,
		graph: pwgraph.New(),
		links: map[pwgraph.Link]linkHandle{},
		// One sink name per process. The pid makes it unique across
		// simultaneous helpers (two broadcasters on one machine is unusual but
		// not forbidden), and identifies the owner in `pw-dump` when someone
		// goes looking.
		sinkName: fmt.Sprintf("gawk-app-capture-%d", os.Getpid()),
	}
	defer h.teardown()

	// Two round-trips, not one, and the second is not belt-and-braces.
	//
	// The first walks the registry, and every audio-relevant global it delivers
	// triggers a *bind* — which is the only way to reach
	// `application.process.binary` (see pwgraph.Global). Those bound objects
	// answer with their own info events, which land after that first sync. So a
	// helper that declared itself ready here would publish an application list
	// with every identity still unresolved, i.e. empty — making a running game
	// look like a machine with nothing playing. The second round-trip is what
	// makes "no applications are playing audio" an answer rather than a guess.
	if err := c.roundtrip(roundTripTimeout); err != nil {
		return err
	}
	h.apply()
	if err := c.roundtrip(roundTripTimeout); err != nil {
		return err
	}
	h.apply()
	if err := out.Write(pwproto.Event{
		Event:   pwproto.EventReady,
		Version: libraryVersion(),
	}); err != nil {
		return err
	}
	if err := h.emitApps(); err != nil {
		return err
	}

	requests := make(chan pwproto.Request, 8)
	readErr := make(chan error, 1)
	go func() {
		r := pwproto.NewReader[pwproto.Request](stdin, pwproto.MaxLine)
		for {
			req, err := r.Next()
			if err != nil {
				readErr <- err
				close(requests)
				return
			}
			requests <- req
		}
	}()

	// SIGTERM and SIGINT take the same path as EOF. SIGKILL takes no path at
	// all, which is exactly why the cleanup does not depend on one.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	for {
		select {
		case <-c.Wake():
			h.apply()
			if err := h.emitApps(); err != nil {
				return err
			}
			if err := h.reconcile(); err != nil {
				return err
			}
		case req, ok := <-requests:
			if !ok {
				// The channel closes with the reader; wait for its verdict.
				err := <-readErr
				if errors.Is(err, io.EOF) {
					return nil // stdin closed: the teardown call
				}
				return err
			}
			if err := h.handle(req); err != nil {
				return err
			}
		case err := <-c.Fatal():
			return err
		case <-signals:
			return nil
		}
	}
}

// linkHandle is one link we created, kept only so we can destroy it.
type linkHandle struct{ proxy proxyPtr }

type helper struct {
	conn  *conn
	out   *pwproto.Writer
	graph *pwgraph.Graph

	sinkName   string
	sinkProxy  proxyPtr
	sinkNodeID uint32
	sinkSerial uint32
	sinkChans  []string

	target string
	links  map[pwgraph.Link]linkHandle

	lastApps  []pwproto.App
	lastLinks int
	appsSent  bool
	linksSent bool
}

// apply folds everything the loop thread queued into the graph, in arrival
// order. Order matters because PipeWire reuses ids: "add 33, remove 33, add 33"
// applied out of order is a graph that believes a dead node is alive.
func (h *helper) apply() {
	for _, q := range h.conn.drain() {
		switch q.op {
		case queuedAdd:
			h.graph.Add(q.global)
		case queuedMerge:
			h.graph.Merge(q.id, q.props)
		case queuedRemove:
			if q.id == h.sinkNodeID && h.sinkNodeID != 0 {
				// Our own sink went away underneath us (a daemon restart).
				// Forget it rather than linking into a ghost; the next capture
				// request makes a new one.
				h.sinkNodeID, h.sinkSerial = 0, 0
			}
			h.graph.Remove(q.id)
		}
	}
}

func (h *helper) emitApps() error {
	apps := h.graph.Apps()
	if h.appsSent && slices.Equal(apps, h.lastApps) {
		return nil
	}
	h.lastApps = apps
	h.appsSent = true
	return h.out.Write(pwproto.Event{Event: pwproto.EventApps, Apps: apps})
}

func (h *helper) handle(req pwproto.Request) error {
	switch req.Op {
	case pwproto.OpWatch:
		// The registry is already being watched; this only asks for the
		// current list, which a fresh reader has not seen.
		h.appsSent = false
		return h.emitApps()
	case pwproto.OpCapture:
		return h.capture(req.Binary)
	case pwproto.OpCaptureSystem:
		return h.capture(systemTarget)
	case pwproto.OpRelease:
		h.releaseLinks()
		h.target = ""
		return h.emitLinks(true)
	case pwproto.OpPing:
		return h.out.Write(pwproto.Event{Event: pwproto.EventPong})
	default:
		// An unknown op is a version skew between two binaries from the same
		// build, which should be impossible — say so rather than ignoring it.
		return h.out.Write(pwproto.Event{
			Event:   pwproto.EventFatal,
			Message: fmt.Sprintf("unknown operation %q", req.Op),
		})
	}
}

// systemTarget is the pseudo-binary that means "the whole machine's output".
// A sentinel rather than a second field on the helper: everything downstream —
// re-targeting, the link diff, the events — then works identically for both,
// and only the plan differs.
const systemTarget = "\x00system"

// capture points the sink at one application, creating the sink on first use.
func (h *helper) capture(binary string) error {
	if binary == "" {
		return h.out.Write(pwproto.Event{
			Event:   pwproto.EventFatal,
			Message: "capture requested with no application binary",
		})
	}
	if h.sinkNodeID == 0 {
		if err := h.createSink(binary); err != nil {
			return err
		}
	}
	if h.target != binary {
		// Re-targeting drops the old links and makes new ones. The sink — and
		// therefore the gst pipeline reading its monitor — is untouched, which
		// is what makes a mid-session switch a re-link rather than a
		// renegotiation (docs/39 D5).
		h.releaseLinks()
		h.target = binary
	}
	if err := h.reconcile(); err != nil {
		return err
	}
	// Always answer a capture request with a link count, even — especially —
	// when it is zero. Zero is the *answer* for an application that has gone
	// quiet since the user picked it, and a silent reply would leave the engine
	// waiting for news that is already true.
	return h.emitLinks(true)
}

// createSink builds the virtual sink and waits for its global to arrive.
//
// The layout is chosen, not assumed (docs/39 D3): raw port-to-port links
// perform no channel mixing, so a sink narrower than the application's streams
// would silently drop channels. The stereo downmix the viewer receives stays
// where it already is, in the gst branch's audioconvert, which is the only
// place it can be done properly.
func (h *helper) createSink(binary string) error {
	var chans []string
	if binary != systemTarget {
		chans = h.graph.StreamChannels(binary)
	}
	if len(chans) == 0 {
		chans = h.graph.WidestSinkChannels()
	}
	if len(chans) == 0 {
		chans = []string{"FL", "FR"}
	}
	proxy, err := h.conn.createSink(
		h.sinkName,
		"gawk application audio capture",
		strings.Join(chans, ","),
		len(chans),
	)
	if err != nil {
		return err
	}
	h.sinkProxy = proxy
	h.sinkChans = chans

	// The proxy exists immediately; the *node* is announced through the
	// registry a round-trip later, and its id and serial are what everything
	// downstream addresses.
	if err := h.conn.roundtrip(roundTripTimeout); err != nil {
		return err
	}
	h.apply()
	node, ok := h.graph.FindSinkByName(h.sinkName)
	if !ok {
		// One more round-trip: on a loaded daemon the node's global can land
		// just behind the sync's done.
		if err := h.conn.roundtrip(roundTripTimeout); err != nil {
			return err
		}
		h.apply()
		node, ok = h.graph.FindSinkByName(h.sinkName)
	}
	if !ok {
		return errors.New("the capture sink was created but never appeared in the registry")
	}
	h.sinkNodeID, h.sinkSerial = node.ID, node.Serial
	return h.out.Write(pwproto.Event{
		Event:    pwproto.EventSink,
		Serial:   node.Serial,
		NodeID:   node.ID,
		Channels: len(chans),
	})
}

// reconcile makes the live links equal the plan.
//
// The plan is a pure function of the graph, so this is a diff rather than
// bookkeeping: a stream dying, a second stream opening, or a game restarting
// all reduce to "recompute and apply the difference". Nothing here can drift
// out of step with reality, because nothing here remembers anything reality
// does not.
func (h *helper) reconcile() error {
	if h.target == "" || h.sinkNodeID == 0 {
		return nil
	}
	var want []pwgraph.Link
	if h.target == systemTarget {
		want = h.graph.PlanSystemAudio(h.sinkNodeID)
	} else {
		want = h.graph.Plan(h.target, h.sinkNodeID)
	}
	wanted := make(map[pwgraph.Link]bool, len(want))
	for _, l := range want {
		wanted[l] = true
	}
	for l, handle := range h.links {
		if !wanted[l] {
			h.conn.destroy(handle.proxy)
			delete(h.links, l)
		}
	}
	for _, l := range want {
		if _, ok := h.links[l]; ok {
			continue
		}
		proxy, err := h.conn.createLink(l)
		if err != nil {
			// One refused link is not a failure of the feature: the port may
			// have vanished between the plan and the call, and the next
			// registry event recomputes. Reported as a message, never as a
			// fatal.
			if werr := h.out.Write(pwproto.Event{
				Event:   pwproto.EventLinks,
				Binary:  h.target,
				Message: err.Error(),
			}.WithLinks(len(h.links))); werr != nil {
				return werr
			}
			continue
		}
		h.links[l] = linkHandle{proxy: proxy}
	}
	return h.emitLinks(false)
}

func (h *helper) emitLinks(force bool) error {
	if !force && h.linksSent && len(h.links) == h.lastLinks {
		return nil
	}
	h.lastLinks = len(h.links)
	h.linksSent = true
	binary := h.target
	if binary == systemTarget {
		// The engine and the GUI speak in binaries; the sentinel is this
		// process's business and must not leak into either.
		binary = ""
	}
	return h.out.Write(pwproto.Event{
		Event:  pwproto.EventLinks,
		Binary: binary,
	}.WithLinks(len(h.links)))
}

func (h *helper) releaseLinks() {
	for l, handle := range h.links {
		h.conn.destroy(handle.proxy)
		delete(h.links, l)
	}
}

// teardown is the polite version of what the daemon would do anyway. It exists
// so a clean exit is *observably* clean at the moment it happens, rather than
// whenever the socket closes — and so the AG5 kill matrix is testing the
// no-linger guarantee rather than this function.
func (h *helper) teardown() {
	h.releaseLinks()
	if h.sinkProxy != nil {
		h.conn.destroy(h.sinkProxy)
		h.sinkProxy = nil
	}
}
