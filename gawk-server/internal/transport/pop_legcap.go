package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The fleet-wide downlink leg cap (docs/76 item 24). A PoP's legs land on
// whichever home pod the UDP load balancer picks, so a per-pod count lets a
// PoP hold maxDownlinkStreams on every pod. The count lives on the PoP's
// link-home pod instead: a pod serving /pop/subscribe/{id} takes a slot
// there before it upgrades, over CONNECT
// /internal/pop/{name}?leg={id}&gen=…&max=…&proto=…&psk=… (the want route's
// PSK and generation fence), and the slot is that session: held while it is
// open, given back when it closes. A pod that dies without closing loses
// its slots at the QUIC idle timeout (edgeIdleTimeout, 4 s); the edge
// dialer's keepalive holds a live pod's session open however idle.
//
// The count is of distinct broadcasts, as the per-pod count was: the PoP's
// downlink pulls each broadcast once (one EdgeManager pull), so for an
// honest PoP distinct broadcasts are its legs; and a re-attach that the load
// balancer puts on another pod while home has not yet seen its
// predecessor's session end is the same broadcast, which must fit or a link
// flap at a full PoP would refuse its own legs.
//
// Fail closed: no link, an unreachable link home or a stale view past
// popLegSlotTimeout refuse the leg with the cap's 429. A slot lost while the
// leg lives (the link moved, its pod drained or died) is taken again at the
// new link home within popLegSlotRegrace, else the leg ends with 4003 and
// the PoP's re-dial meets the cap. A single-pod home counts locally
// (acquirePopLeg).

const (
	// popLegSlotTimeout bounds taking a slot before the upgrade: a stale
	// status.lease (404 / 409) or a link home that is momentarily away is
	// retried for this long.
	popLegSlotTimeout = 2 * time.Second
	// popLegSlotRetry paces those retries.
	popLegSlotRetry = 100 * time.Millisecond
	// popLegSlotRegrace is how long a live leg whose slot was lost keeps
	// trying to take it again at the new link home. A relink is the PoP's
	// link retry plus the new holder's status.lease write: seconds.
	popLegSlotRegrace = 15 * time.Second
	// popLegSlotCheck is how often the link home checks that it still
	// holds the link a slot was counted under.
	popLegSlotCheck = 250 * time.Millisecond
)

// popLinkLocator is the slice of PopHome the fleet-wide count needs
// (*popsrv.Manager): where a PoP's link is, and whether this pod holds it.
type popLinkLocator interface {
	// LinkHolder is the pod address and link generation of pop's link
	// (status.lease), ok false when it is not linked.
	LinkHolder(pop string) (addr string, gen int64, ok bool)
	// HoldsLink is this pod's own link for pop and its generation.
	HoldsLink(pop string) (gen int64, ok bool)
}

func (s *Server) popLinkLocator() popLinkLocator {
	l, _ := s.popHomeOrNil().(popLinkLocator)
	return l
}

// popLegCount is the link-home pod's count for one PoP: distinct
// broadcasts, each with the number of slots (sessions) holding it, under
// the link generation it was taken in.
type popLegCount struct {
	gen int64
	ids map[string]int
}

// popLegSlot is one slot a leg holds. lost closes when the slot stops
// counting (the link moved, or the link home's session to it ended);
// release gives it back. lost is nil for a slot that cannot be lost.
type popLegSlot struct {
	lost    <-chan struct{}
	release func()
}

// errPopLegCap is the cap refusing: the leg is refused, never retried.
var errPopLegCap = errors.New("transport: the PoP's downlink leg cap is full")

// acquireFleetPopLeg takes a slot for PoP pop's leg of broadcast id, on
// this pod when it holds the link, else at the link-home pod. Any error
// refuses the leg; errPopLegCap is the cap itself.
func (s *Server) acquireFleetPopLeg(ctx context.Context, pop, id string, limit int) (*popLegSlot, error) {
	loc := s.popLinkLocator()
	em := s.edgeManager()
	if loc == nil || em == nil {
		return nil, errors.New("transport: no PoP link locator")
	}
	ctx, cancel := context.WithTimeout(ctx, popLegSlotTimeout)
	defer cancel()
	var last error
	for {
		if gen, ok := loc.HoldsLink(pop); ok {
			return s.acquireLocalPopLegSlot(loc, pop, gen, id, limit)
		}
		// A link moving between pods is briefly invisible here (its holder
		// drained, the new holder's status.lease not yet in this pod's
		// cache): that is retried like a stale holder, inside the same
		// window, and only refuses the leg once the window ends.
		if addr, gen, ok := loc.LinkHolder(pop); !ok || addr == "" {
			last = errPopUnlinked
		} else {
			// Dialled under the server's lifetime, not the request's: the
			// slot session outlives the CONNECT that asked for it.
			dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), popLegSlotTimeout)
			up, err := em.dial(dctx, addr, internalPopLegPath(pop, id, gen, limit))
			dcancel()
			if err == nil {
				return remotePopLegSlot(em.baseCtx, up), nil
			}
			var se *internalStatusError
			if errors.As(err, &se) && se.status == http.StatusTooManyRequests {
				return nil, errPopLegCap
			}
			last = fmt.Errorf("transport: no leg slot from %s: %w", addr, err)
		}
		t := time.NewTimer(popLegSlotRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, last
		case <-t.C:
		}
	}
}

// remotePopLegSlot is a slot held as a session to the link-home pod. The
// link home never opens a stream on it, so AcceptUniStream returns exactly
// when the session ends — by either side.
func remotePopLegSlot(base context.Context, up edgeUpstream) *popLegSlot {
	lost := make(chan struct{})
	go func() {
		_, _ = up.AcceptUniStream(base)
		close(lost)
	}()
	var once sync.Once
	return &popLegSlot{lost: lost, release: func() { once.Do(func() { _ = up.Close() }) }}
}

// acquireLocalPopLegSlot counts a slot on this pod, the link home, under
// link generation gen. It is lost when this pod stops holding the link at
// gen; a newer generation's first slot starts the count afresh, since
// every older slot is then lost and releasing.
func (s *Server) acquireLocalPopLegSlot(loc popLinkLocator, pop string, gen int64, id string, limit int) (*popLegSlot, error) {
	s.popSlotsMu.Lock()
	if s.popSlots == nil {
		s.popSlots = map[string]*popLegCount{}
	}
	c := s.popSlots[pop]
	if c == nil || c.gen != gen {
		c = &popLegCount{gen: gen, ids: map[string]int{}}
		s.popSlots[pop] = c
	}
	if c.ids[id] == 0 && limit > 0 && len(c.ids) >= limit {
		s.popSlotsMu.Unlock()
		return nil, errPopLegCap
	}
	c.ids[id]++
	s.popSlotsMu.Unlock()

	lost, stop := make(chan struct{}), make(chan struct{})
	go func() {
		t := time.NewTicker(popLegSlotCheck)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if held, ok := loc.HoldsLink(pop); !ok || held != gen {
					close(lost)
					return
				}
			}
		}
	}()
	var once sync.Once
	return &popLegSlot{lost: lost, release: func() {
		once.Do(func() {
			close(stop)
			s.popSlotsMu.Lock()
			defer s.popSlotsMu.Unlock()
			if c.ids[id]--; c.ids[id] <= 0 {
				delete(c.ids, id)
			}
			if len(c.ids) == 0 && s.popSlots[pop] == c {
				delete(s.popSlots, pop)
			}
		})
	}}, nil
}

// keepPopLegSlot holds a live leg's slot until ctx ends (the leg ended),
// then releases it. A lost slot is taken again at the link home as it is
// now; refused by the cap, or not regained within popLegSlotRegrace, the
// leg ends with 4003 through endLeg (the PoP re-dials and meets the cap).
func (s *Server) keepPopLegSlot(ctx context.Context, slot *popLegSlot, pop, id string, limit int, endLeg func(), log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			slot.release()
			return
		case <-slot.lost:
		}
		slot.release()
		deadline := time.Now().Add(popLegSlotRegrace)
		for {
			next, err := s.acquireFleetPopLeg(ctx, pop, id, limit)
			if err == nil {
				slot = next
				break
			}
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errPopLegCap) || time.Now().After(deadline) {
				log.Info("pop downlink leg ended: its fleet-wide slot is gone", "err", err)
				endLeg()
				return
			}
			t := time.NewTimer(popLegSlotRetry)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

// PopLegSlots is the number of distinct broadcasts PoP pop holds downlink
// leg slots for, fleet-wide, as counted on this pod: the link-home pod's
// count, 0 elsewhere.
func (s *Server) PopLegSlots(pop string) int {
	loc := s.popLinkLocator()
	if loc == nil {
		return 0
	}
	gen, ok := loc.HoldsLink(pop)
	if !ok {
		return 0
	}
	s.popSlotsMu.Lock()
	defer s.popSlotsMu.Unlock()
	if c := s.popSlots[pop]; c != nil && c.gen == gen {
		return len(c.ids)
	}
	return 0
}

// handleInternalPopLeg is /internal/pop/{name}?leg={id}, the link home's
// half: the PSK and proto are checked (handleInternalPop), then the
// generation fence as for a want — 404 when this pod does not hold the
// link, 409 for a stale generation — and the cap (429 "downlink-limit").
// The slot is the upgraded session: it ends when the asking pod closes it
// or goes silent past the idle timeout, or when this pod stops holding the
// link (closed with 4003, so the asker takes its slot at the new holder).
func (s *Server) handleInternalPopLeg(w http.ResponseWriter, r *http.Request, pop string, q url.Values) {
	const route = "internal_pop"
	loc := s.popLinkLocator()
	id, err := broadcastid.Normalize(q.Get("leg"))
	if loc == nil || err != nil || pop == "" {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	held, ok := loc.HoldsLink(pop)
	if !ok {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if gen, err := strconv.ParseInt(q.Get("gen"), 10, 64); err != nil || gen != held {
		s.metrics.Connection(route, metrics.OutcomeConflict)
		w.WriteHeader(http.StatusConflict)
		return
	}
	// The asker's limit is the Pop's spec as its informer reads it; both
	// pods read the same CR.
	limit, _ := strconv.Atoi(q.Get("max"))
	slot, err := s.acquireLocalPopLegSlot(loc, pop, held, id, limit)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeLimitRejected)
		popRefuse(w, http.StatusTooManyRequests, popReasonLegLimit)
		return
	}
	defer slot.release()
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer s.trackSession(sess)()
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	select {
	case <-sess.Context().Done():
	case <-slot.lost:
		_ = sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeOriginMoved), "pop link moved")
	}
}

// internalPopLegPath is the slot route on the link-home pod; the edge
// dialer appends the PSK.
func internalPopLegPath(pop, id string, gen int64, limit int) string {
	return fmt.Sprintf("/internal/pop/%s?leg=%s&gen=%d&max=%d&proto=%d",
		url.PathEscape(pop), url.QueryEscape(id), gen, limit, wire.Version)
}
