package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// popReasonLegLimit is the ReasonHeader value of a leg refused by the
// PoP's spec.limits.maxDownlinkStreams.
const popReasonLegLimit = "downlink-limit"

// handlePopSubscribe is CONNECT /pop/subscribe/{id}: a PoP's downlink leg
// for a home broadcast (docs/76 §6.6, item 23). Any pod serves it, origin or
// edge, exactly as it serves a viewer: a miss demand-creates the edge pull.
// It is not /internal/subscribe, so docs/22's depth-2 fence is untouched and
// a PoP is a third tier below it.
//
// Refusals are statuses the PoP's Go dialer reads: 401 / 403 / 429 from
// Authenticate, 426 for a proto major it does not speak, 404 for a broadcast
// that is not here (or is this PoP's own reservation: a PoP never pulls its
// own broadcast back), 429 with ReasonHeader "downlink-limit" over the
// PoP's leg cap. No rateLimited or IP-ban check, as on every /pop/* route
// (pop_routes.go): the credential is the gate.
//
// The leg is home's leg writer (hub.SubscribeLeg): an internal subscriber,
// exempt from the viewer caps and out of the local count, whose reported
// ViewerCount is accepted (docs/76 item 25, the credential as docs/23 D6's
// peer proof). ?delivery=carrier asks for carrier delivery (OD16): deltas
// and parity on per-GOP carriers, every other datagram type as a datagram.
// Without it the leg is an edge's datagrams, as before R73.
func (s *Server) handlePopSubscribe(w http.ResponseWriter, r *http.Request) {
	const route = "pop_subscribe"
	if s.rejectedDraining(w, route) {
		return
	}
	home := s.popHomeOrNil()
	if home == nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if _, err := poplink.CheckProto(r.URL.Query()); err != nil {
		s.metrics.Connection(route, metrics.OutcomeError)
		popRefuse(w, http.StatusUpgradeRequired, poplink.ReasonProto)
		return
	}
	pop, status, err := home.Authenticate(r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUnauthorized)
		reason := poplink.RefusalReason(err)
		if status == http.StatusTooManyRequests {
			reason = poplink.ReasonLimited
		}
		s.log.Info("pop subscribe refused", "remote", r.RemoteAddr, "status", status, "reason", reason)
		popRefuse(w, status, reason)
		return
	}
	id, err := broadcastid.Normalize(r.PathValue("id"))
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	log := s.log.With("route", route, "pop", pop.Name, "broadcast_id", id, "remote", r.RemoteAddr)
	if coord := s.clusterCoord(); coord != nil {
		if o, _, _, ok := coord.Lookup(id); ok && o.Pop == pop.Name {
			s.metrics.Connection(route, metrics.OutcomeNotFound)
			log.Info("pop subscribe refused: the PoP's own broadcast")
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
	// Only a missing broadcast refuses: a leg is exempt from the viewer
	// caps CheckSubscribe also reports.
	err = s.registry.CheckSubscribe(id)
	if edges := s.edgeManager(); errors.Is(err, hub.ErrNotFound) && edges != nil {
		if edgeErr := edges.EnsureEdge(r.Context(), id); edgeErr == nil {
			err = s.registry.CheckSubscribe(id)
		}
	}
	if errors.Is(err, hub.ErrNotFound) {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// The cap is fleet-wide in cluster mode, counted on the PoP's link-home
	// pod (pop_legcap.go); this pod's own count (PopLegs) is then only
	// counted, never enforced. A single-pod home enforces it here.
	limit := pop.EffectiveLimits().MaxDownlinkStreams
	coord := s.clusterCoord()
	localLimit := limit
	if coord != nil {
		localLimit = 0
	}
	release, ok := s.acquirePopLeg(pop.Name, id, localLimit)
	if !ok {
		s.metrics.Connection(route, metrics.OutcomeLimitRejected)
		log.Info("pop subscribe refused: the PoP's downlink leg cap", "max", limit)
		popRefuse(w, http.StatusTooManyRequests, popReasonLegLimit)
		return
	}
	defer release()
	var slot *popLegSlot
	if coord != nil {
		slot, err = s.acquireFleetPopLeg(r.Context(), pop.Name, id, limit)
		if err != nil {
			// Fail closed: a leg the count cannot see is refused as over it.
			s.metrics.Connection(route, metrics.OutcomeLimitRejected)
			log.Info("pop subscribe refused: the PoP's fleet-wide downlink leg cap", "max", limit, "err", err)
			popRefuse(w, http.StatusTooManyRequests, popReasonLegLimit)
			return
		}
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		if slot != nil {
			slot.release()
		}
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		log.Warn("pop subscribe upgrade failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer s.trackSession(sess)()
	if slot != nil {
		kctx, kcancel := context.WithCancel(r.Context())
		kept := make(chan struct{})
		slotLog := log // log is re-bound below; the goroutine keeps its own
		go func() {
			defer close(kept)
			s.keepPopLegSlot(kctx, slot, pop.Name, id, limit, func() {
				_ = sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeOriginMoved), "downlink leg slot lost")
			}, slotLog)
		}()
		defer func() { kcancel(); <-kept }()
	}

	// No close notice and no session notices: the PoP's pump reads every uni
	// stream as media (by prologue), and it reads close codes itself.
	carrier := r.URL.Query().Get("delivery") == "carrier"
	sub, err := s.registry.SubscribeLeg(id, &webtransportSessionAdapter{Session: sess}, hub.LegOptions{Carrier: carrier})
	if err != nil {
		// The hub vanished between the check and now: 4000 tells the PoP the
		// broadcast is over.
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcastEnded), "broadcast ended")
		return
	}
	defer sub.Close()

	s.metrics.Connection(route, metrics.OutcomeAccepted)
	delivery := "datagrams"
	if carrier {
		delivery = "carrier"
	}
	log = log.With("delivery", delivery)
	log.Info("pop downlink leg attached")
	tsLimiter := newTimeSyncLimiter()
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("pop downlink leg ended", "reason", sessionEndReason(r.Context(), err), "dropped", sub.Dropped())
			return
		}
		// The PoP's TimeSync pings (its per-hop ClockMapping rewrite).
		if maybeAnswerTimeSync(sess, dgram, tsLimiter) {
			continue
		}
		// The PoP's party viewer count: home's G includes it (docs/76
		// item 25), as an edge's on /internal/subscribe.
		if len(dgram) == wire.ViewerCountSize && dgram[1] == wire.TypeViewerCount {
			if count, err := wire.ParseViewerCount(dgram); err == nil {
				sub.RecordDownstreamViewers(count)
			}
		}
	}
}

// acquirePopLeg counts a leg of PoP pop for broadcast id against its
// spec.limits.maxDownlinkStreams (docs/76 item 24: a compromised PoP cannot
// drain the fleet's egress). The cap counts distinct broadcasts, so a
// second leg for one broadcast (a re-attach racing its predecessor's end)
// always fits. This is the pod's own count (PopLegs); limit 0 counts
// without enforcing, as cluster mode does: there the cap is the link-home
// pod's fleet-wide count (acquireFleetPopLeg).
func (s *Server) acquirePopLeg(pop, id string, limit int) (release func(), ok bool) {
	s.popLegsMu.Lock()
	defer s.popLegsMu.Unlock()
	if s.popLegs == nil {
		s.popLegs = map[string]map[string]int{}
	}
	ids := s.popLegs[pop]
	if ids[id] == 0 && limit > 0 && len(ids) >= limit {
		return nil, false
	}
	if ids == nil {
		ids = map[string]int{}
		s.popLegs[pop] = ids
	}
	ids[id]++
	return func() {
		s.popLegsMu.Lock()
		defer s.popLegsMu.Unlock()
		if ids[id]--; ids[id] <= 0 {
			delete(ids, id)
		}
		if len(ids) == 0 && len(s.popLegs[pop]) == 0 {
			delete(s.popLegs, pop)
		}
	}, true
}

// PopLegs is the number of distinct broadcasts PoP pop pulls from this pod
// over /pop/subscribe now.
func (s *Server) PopLegs(pop string) int {
	s.popLegsMu.Lock()
	defer s.popLegsMu.Unlock()
	return len(s.popLegs[pop])
}
