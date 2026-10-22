package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The home side of R74's uplink on demand (docs/76 §6.5, PP8). An outside
// viewer of a PoP's broadcast lands on pod B; B's subscribe miss finds the
// broadcast's Lease reserved for the PoP with no holder, EnsureEdge makes the
// waiting edge hub and calls onReserved — here — which sends want{X} to the
// link-home pod A (/internal/pop/{name}, or A's own manager when B is A). A
// forwards it on the link; the PoP admits it and dials /pop/publish/{id}
// (pop_publish.go), or refuses it. A refusal for the budget ends B's waiting
// viewers with 4012 (item 28); with no answer they wait as for any absent
// broadcaster.

// Defaults for the -pop-uplink-linger and -pop-demand-timeout knobs when a
// Config leaves them zero (the in-process harnesses).
const (
	defaultPopUplinkLinger  = 30 * time.Second
	defaultPopDemandTimeout = 5 * time.Second
)

// popUplinkFullReason is the close reason that goes with 4012.
const popUplinkFullReason = "outside viewing is full right now"

// ErrPopNotHolder: a want for a PoP whose link this pod does not hold.
var ErrPopNotHolder = errors.New("transport: this pod does not hold the PoP's link")

// PopDemandHome is the home's PoP uplink subsystem as transport calls it
// (popsrv's Uplink, installed by serve's wirePopUplink). An interface so
// transport never imports popsrv.
type PopDemandHome interface {
	// LinkHolder is where pop's link is held: the pod address and the
	// link's generation (status.lease), ok false when it is not linked.
	LinkHolder(pop string) (addr string, gen int64, ok bool)
	// HoldsLink reports this pod's own link for pop and its generation.
	HoldsLink(pop string) (gen int64, ok bool)
	// Want forwards want{id} on pop's link, which this pod holds (else
	// ErrPopNotHolder), one want per broadcast however many pods ask, and
	// waits for the PoP's answer up to the demand timeout or ctx: the
	// refusal reason (poplink.Refuse*), or "" when the PoP admitted it or
	// did not answer.
	Want(ctx context.Context, pop, id string) (refusal string, err error)
	// ReservedFor names the PoP id is reserved for: the single-pod
	// /pop/publish ownership check (cluster mode reads the Lease).
	ReservedFor(id string) (pop string, ok bool)
}

// demandDeferrer is the R74 lr=1 hold (docs/76 item 21, W3-offers): how
// long to hold a demand from a session at remote that said it follows an
// offer, so a guest who switches to the PoP costs the venue nothing.
type demandDeferrer interface {
	Defer(remote netip.Addr, lr bool) time.Duration
}

type popDemandBox struct{ h PopDemandHome }
type demandDeferrerBox struct{ d demandDeferrer }

// popUplinkState is the Server's uplink state (one field on Server).
type popUplinkState struct {
	demand   atomic.Pointer[popDemandBox]
	deferrer atomic.Pointer[demandDeferrerBox]

	mu sync.Mutex
	// loops: one demand loop per broadcast on this pod.
	loops map[string]*demandLoop
	// legs: the live /pop/publish leg per broadcast on this pod (its
	// linger watcher's token).
	legs map[string]*popLegClaim
}

// SetPopDemand installs the home's uplink subsystem (nil removes it). It
// becomes the edge manager's onReserved in cluster mode; a single-pod home
// waits its viewers in a publisher-less hub instead (popWaitSinglePod).
// Safe while serving.
func (s *Server) SetPopDemand(h PopDemandHome) {
	if h == nil {
		s.uplink.demand.Store(nil)
	} else {
		s.uplink.demand.Store(&popDemandBox{h: h})
	}
	if em := s.edgeManager(); em != nil && em.onReserved == nil {
		em.onReserved = s.onReservedMiss
	}
}

// SetDemandDeferrer installs the lr=1 hold (W3-offers); nil removes it.
func (s *Server) SetDemandDeferrer(d demandDeferrer) {
	if d == nil {
		s.uplink.deferrer.Store(nil)
		return
	}
	s.uplink.deferrer.Store(&demandDeferrerBox{d: d})
}

func (s *Server) popDemand() PopDemandHome {
	if b := s.uplink.demand.Load(); b != nil {
		return b.h
	}
	return nil
}

func (s *Server) popUplinkLinger() time.Duration {
	if s.cfg.PopUplinkLinger > 0 {
		return s.cfg.PopUplinkLinger
	}
	return defaultPopUplinkLinger
}

func (s *Server) popDemandTimeout() time.Duration {
	if s.cfg.PopDemandTimeout > 0 {
		return s.cfg.PopDemandTimeout
	}
	return defaultPopDemandTimeout
}

// demandRequest is what a subscribe miss knows about its viewer, for the
// lr=1 hold: where it came from and whether it follows an offer.
type demandRequest struct {
	remote netip.Addr
	lr     bool
}

type demandRequestKey struct{}

// withDemandRequest is the subscribe miss's context for EnsureEdge: the
// request's own, carrying the viewer's address and lr flag to onReserved.
func withDemandRequest(r *http.Request) context.Context {
	return context.WithValue(r.Context(), demandRequestKey{}, demandRequest{
		remote: remoteIP(r.RemoteAddr),
		lr:     r.URL.Query().Get("lr") == "1",
	})
}

func demandRequestFrom(ctx context.Context) (demandRequest, bool) {
	d, ok := ctx.Value(demandRequestKey{}).(demandRequest)
	return d, ok
}

// onReservedMiss is EdgeManager.onReserved: a subscribe miss on pop's
// dormant reservation. The waiting hub already exists; this starts (or
// nudges) the broadcast's demand loop and returns at once — the viewer
// upgrades and waits whatever the PoP answers.
func (s *Server) onReservedMiss(ctx context.Context, id string, o cluster.Origin) error {
	if s.popDemand() == nil {
		return nil
	}
	s.startDemand(ctx, id, o.Pop)
	return nil
}

// popWaitSinglePod is the single-pod home's miss on a PoP reservation (no
// edge pulls, no Lease): it makes the broadcast's hub in the state a
// waiting edge hub is in — known, nothing primed, no publisher — so
// the viewer upgrades and waits, and starts the demand. The PoP's leg then
// claims that very hub (ResumePublish). False leaves the miss a 404.
func (s *Server) popWaitSinglePod(ctx context.Context, id string) bool {
	if s.clusterCoord() != nil {
		return false
	}
	h := s.popDemand()
	if h == nil {
		return false
	}
	pop, ok := h.ReservedFor(id)
	if !ok {
		return false
	}
	// Made as an edge hub, as cluster mode's waiting hub is: home never had
	// this broadcast, so making and removing the hub must not look like one
	// starting and ending (no broadcast.started/ended, no publisher-away
	// hook, no usage sample). The leg's ResumePublish makes it an origin.
	switch _, pub, err := s.registry.EdgePublish(id); {
	case err == nil:
		pub.Close()
	case !errors.Is(err, hub.ErrPublisherActive):
		return false
	}
	s.startDemand(ctx, id, pop)
	return true
}

// popLegEnded is a /pop/publish leg's end without 4013 (the leg died, or
// the PoP host restarted and with it forgot the want, docs/76 §6.13). Its
// viewers stay on this pod's graced hub, and edge pulls on other pods stay
// on it too, so no subscribe miss will fire a want again: while anyone is
// downstream, the demand loop asks the PoP again until a leg is back.
func (s *Server) popLegEnded(id, pop string) {
	if s.popDemand() == nil || s.registry.DownstreamSessions(id) == 0 {
		return
	}
	s.startDemand(context.Background(), id, pop)
}

// startDemand starts (or nudges) id's demand loop on this pod.
func (s *Server) startDemand(ctx context.Context, id, pop string) {
	req, hasReq := demandRequestFrom(ctx)
	var hold time.Duration
	if b := s.uplink.deferrer.Load(); b != nil && hasReq {
		hold = b.d.Defer(req.remote, req.lr)
	}
	s.uplink.mu.Lock()
	if s.uplink.loops == nil {
		s.uplink.loops = map[string]*demandLoop{}
	}
	l := s.uplink.loops[id]
	if l == nil {
		l = &demandLoop{s: s, id: id, pop: pop, nudge: make(chan struct{}, 1), hold: hold}
		s.uplink.loops[id] = l
		s.uplink.mu.Unlock()
		go l.run()
		return
	}
	s.uplink.mu.Unlock()
	l.shorten(hold)
}

// demandLoop is one broadcast's want on this pod: it asks, then asks again
// every demand timeout while viewers here wait on a reservation no leg has
// claimed — the first want can be lost (the link was down, the PoP's
// broadcaster away), and a lease handed back by a leg's linger has no miss
// to fire a new one.
type demandLoop struct {
	s     *Server
	id    string
	pop   string
	nudge chan struct{}

	mu   sync.Mutex
	hold time.Duration
}

// shorten lowers the loop's initial hold: a later viewer without lr=1 (or
// outside every PoP's address) must not wait on an earlier guest's hold.
func (l *demandLoop) shorten(hold time.Duration) {
	l.mu.Lock()
	if hold < l.hold {
		l.hold = hold
		select {
		case l.nudge <- struct{}{}:
		default:
		}
	}
	l.mu.Unlock()
}

func (l *demandLoop) run() {
	s := l.s
	defer func() {
		s.uplink.mu.Lock()
		if s.uplink.loops[l.id] == l {
			delete(s.uplink.loops, l.id)
		}
		s.uplink.mu.Unlock()
	}()
	// Cluster mode: the loop ends with the edge manager (server shutdown).
	// Single-pod it ends when nobody waits.
	ctx := context.Background()
	if em := s.edgeManager(); em != nil {
		ctx = em.baseCtx
	}
	start := time.Now()
	for {
		l.mu.Lock()
		wait := l.hold - time.Since(start)
		l.mu.Unlock()
		if wait <= 0 {
			break
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-l.nudge:
		case <-t.C:
		}
		t.Stop()
	}

	timeout := s.popDemandTimeout()
	for first := true; ; first = false {
		if ctx.Err() != nil || !l.stillWaiting(first) {
			return
		}
		began := time.Now()
		refusal := s.sendWant(ctx, l.pop, l.id, timeout)
		if refusal == poplink.RefuseBudget {
			s.popUplinkFull(l.id)
			return
		}
		if rest := timeout - time.Since(began); rest > 0 {
			t := time.NewTimer(rest)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// stillWaiting: the reservation still has no leg, and (after the first
// want, which goes before the viewer has upgraded) somebody here waits: a
// viewer, or another PoP's downlink leg.
func (l *demandLoop) stillWaiting(first bool) bool {
	if coord := l.s.clusterCoord(); coord != nil {
		origin, _, _, ok := coord.Lookup(l.id)
		if !ok || origin.Pop == "" {
			return false
		}
		switch em := l.s.edgeManager(); {
		case origin.Holder == "":
			// A dormant reservation.
		case em != nil && origin.Holder == em.podName:
			// This pod's own claim, graced by a leg that ended without
			// 4013 (popLegEnded): waiting until a leg publishes again.
			if live, _, known := l.s.registry.BroadcastState(l.id); !known || live {
				return false
			}
		default:
			return false // a leg holds it on another pod
		}
	} else {
		// Single-pod: the waiting hub is there and no leg publishes yet.
		h := l.s.popDemand()
		if h == nil {
			return false
		}
		if _, ok := h.ReservedFor(l.id); !ok {
			return false
		}
		if live, _, known := l.s.registry.BroadcastState(l.id); !known || live {
			return false
		}
	}
	// Every session on the hub, PoP downlink legs included: a leg is
	// internal, and another PoP's party viewers wait behind it.
	return first || l.s.registry.DownstreamSessions(l.id) > 0
}

// popUplinkFull ends this pod's waiting viewers of id with 4012 (the PoP
// refused the want for its budget, docs/76 item 28): in-band SessionClosing
// first, through the viewers' close-notice adapters. The waiting pull goes
// first, so its claim cannot remake the hub.
func (s *Server) popUplinkFull(id string) {
	if em := s.edgeManager(); em != nil {
		em.StopEdge(id)
	}
	if s.registry.TerminateBroadcast(id, uint32(wire.CloseCodePopUplinkFull), popUplinkFullReason) {
		s.log.Info("pop uplink full: outside viewers refused", "broadcast_key", s.broadcastKey(id))
	}
}

// sendWant sends one want for id to pop's link-home pod and returns the
// PoP's refusal ("" for none). This pod's own link is asked directly.
func (s *Server) sendWant(ctx context.Context, pop, id string, timeout time.Duration) string {
	h := s.popDemand()
	if h == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, ok := h.HoldsLink(pop); ok {
		refusal, err := h.Want(ctx, pop, id)
		if err != nil {
			s.log.Debug("pop want not forwarded", "pop", pop, "broadcast_key", s.broadcastKey(id), "err", err)
		}
		return refusal
	}
	addr, gen, ok := h.LinkHolder(pop)
	em := s.edgeManager()
	if !ok || addr == "" || em == nil {
		s.log.Debug("pop want held: no link", "pop", pop, "broadcast_key", s.broadcastKey(id))
		return ""
	}
	// The edge dialer carries the cluster PSK and the internal origin, as
	// for /internal/subscribe; the answer, if any, is one uni stream.
	// Leave the dial a little longer than A's own wait, so A's "no answer"
	// arrives as a clean close rather than our timeout.
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), timeout+2*time.Second)
	defer dcancel()
	up, err := em.dial(dctx, addr, internalPopPath(pop, id, gen))
	if err != nil {
		s.log.Debug("pop want dial failed", "pop", pop, "addr", addr, "err", err)
		return ""
	}
	defer up.Close()
	st, err := up.AcceptUniStream(dctx)
	if err != nil {
		return ""
	}
	rec, err := poplink.Read(st)
	if r, ok := rec.(*poplink.Refuse); err == nil && ok && r.ID == id {
		return r.Reason
	}
	return ""
}

// internalPopPath is the want route on the link-home pod; the edge dialer
// appends the PSK.
func internalPopPath(pop, id string, gen int64) string {
	return fmt.Sprintf("/internal/pop/%s?want=%s&gen=%d&proto=%d",
		url.PathEscape(pop), url.QueryEscape(id), gen, wire.Version)
}
