package transport

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// popLingerTick paces a leg's linger check.
const popLingerTick = time.Second

// popLegClaim is one /pop/publish leg's claim of a broadcast on this pod: the
// linger watcher's token. A newer leg's claim replaces it, which retires the
// older watcher.
type popLegClaim struct {
	id   string
	pop  string
	gen  int64 // the Lease generation the leg claimed (cluster mode)
	sess *webtransport.Session
	// live is set while the leg's session runs.
	live atomic.Bool
	// handedBack is set once the linger handed the Lease back (4013).
	handedBack atomic.Bool
	stop       chan struct{}
}

// handlePopPublish is CONNECT /pop/publish/{id}: an R74 PoP's uplink leg for
// a broadcast reserved for it (docs/76 item 18). It lands on any pod C,
// which checks the credential and that the ID is reserved for this very PoP
// — that check stands in for the resume token — then claims the broadcast's
// Lease as origin (cluster.ClaimForPop, force: a newer leg wins) and runs an
// ordinary publish session on it: keyframe streams and, when C advertises
// CapUplinkCarriers, R73 carriers through the R73 reader, every other type
// as a datagram, TimeSync answered, and the viewer count G pushed down.
//
// It sends no announce, resume token, telemetry or session notices: the PoP
// already knows the ID, and a leg is no browser. It always sends
// RelayCapabilities, flags zero included, so the PoP's writer learns the
// delivery without a timeout. No rate limit or IP ban applies (docs/76 item
// 30): the PoP's address carries every guest at the party; an ID ban does.
//
// Statuses before the upgrade: 404 without -pops or for an ID reserved for
// nobody, 426 proto, 401/403/429 from the credential, 409 for an ID that is
// another PoP's (or a home broadcast), 451 for a banned ID.
func (s *Server) handlePopPublish(w http.ResponseWriter, r *http.Request) {
	const route = "pop_publish"
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
		popRefuse(w, status, reason)
		return
	}
	id, err := broadcastid.Normalize(r.PathValue("id"))
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if _, banned := s.banSet().BannedID(id, s.clock()); banned {
		s.metrics.Connection(route, metrics.OutcomeBanned)
		s.log.Warn("pop uplink leg refused: banned broadcast", "pop", pop.Name, "broadcast_key", s.broadcastKey(id))
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
		return
	}
	if status := s.popOwnership(id, pop); status != 0 {
		s.metrics.Connection(route, metrics.OutcomeConflict)
		s.log.Info("pop uplink leg refused: not this PoP's reservation", "pop", pop.Name,
			"broadcast_key", s.broadcastKey(id), "status", status)
		w.WriteHeader(status)
		return
	}

	// Come-home, as a broadcaster's claim does: a pull on this pod (an edge
	// or a reservation's waiting hub) holds the hub's publisher slot. Its
	// viewers stay on the hub.
	if edges := s.edgeManager(); edges != nil {
		edges.StopEdge(id)
	}
	_, pub, err := s.registry.ResumePublish(id)
	if err != nil && !errors.Is(err, hub.ErrPublisherActive) {
		s.metrics.Connection(route, metrics.OutcomeError)
		s.log.Warn("pop uplink leg refused", "pop", pop.Name, "broadcast_key", s.broadcastKey(id), "err", err)
		if errors.Is(err, hub.ErrMaxBroadcasts) {
			w.WriteHeader(http.StatusTooManyRequests)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	var gen int64
	if pub != nil {
		if gen, status = s.claimForPop(r.Context(), id, pop.Name); status != 0 {
			pub.Close()
			s.metrics.Connection(route, metrics.OutcomeConflict)
			if status == http.StatusServiceUnavailable {
				// Undecided (an API blip), not a refusal: the broadcast is
				// still the PoP's, so the hub and its viewers stay, the pull
				// stopped above resumes, and the PoP's redial decides.
				s.resumePull(id)
			} else {
				s.registry.EndBroadcast(id)
			}
			w.WriteHeader(status)
			return
		}
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		if pub != nil {
			pub.Close()
		}
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if pub == nil {
		// An older leg of the same PoP holds the slot (a leg that died
		// silently, or a redial racing its predecessor): newest leg wins.
		if _, pub, err = s.registry.TakeOverPublish(id); err != nil {
			s.metrics.Connection(route, metrics.OutcomeNotFound)
			closeWithNotice(sess, wire.CloseCodeBroadcastEnded, "broadcast ended")
			return
		}
		if gen, status = s.claimForPop(r.Context(), id, pop.Name); status != 0 {
			pub.Close()
			s.metrics.Connection(route, metrics.OutcomeConflict)
			sess.CloseWithError(webtransport.SessionErrorCode(status), "broadcast is not reserved for this PoP")
			return
		}
	}
	defer pub.Close()
	defer s.trackSession(sess)()
	// The leg is findable by its broadcast ID (an ID ban's kill), never by
	// its address: an IP ban on the venue must not cut the leg (item 30).
	defer s.trackPublisher(id, sess, netip.Addr{})()
	if _, banned := s.banSet().BannedID(id, s.clock()); banned {
		s.metrics.Connection(route, metrics.OutcomeBanned)
		closeWithNotice(sess, wire.CloseCodeTerminatedByOperator, terminationReason)
		return
	}
	s.publisherLive(r, id, pub)
	if !pub.BindConn(&webtransportSessionAdapter{Session: sess}) {
		s.metrics.Connection(route, metrics.OutcomeConflict)
		sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodePublisherSuperseded), "superseded by a newer leg")
		return
	}
	pub.BindSend(func(b []byte) { _ = sess.SendDatagram(b) })
	s.metrics.Connection(route, metrics.OutcomeAccepted)

	log := s.log.With("route", route, "pop", pop.Name, "broadcast_key", s.broadcastKey(id))
	s.sendLegCapabilities(sess, log)
	log.Info("pop uplink leg started", "generation", gen)

	claim := s.startLegClaim(id, pop.Name, gen, sess)
	defer func() {
		// The publisher first: the linger's hand-back waits for live to
		// drop before it removes the hub, which a live publisher would keep.
		pub.Close()
		claim.live.Store(false)
	}()

	go s.acceptKeyframeStreams(r.Context(), sess, pub, log)
	tsLimiter := newTimeSyncLimiter()
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			if claim.handedBack.Load() {
				log.Info("pop uplink leg closed: no demand")
			} else {
				log.Info("pop uplink leg ended", "reason", sessionEndReason(r.Context(), err))
			}
			return
		}
		pub.NoteSeen()
		if maybeAnswerTimeSync(sess, dgram, tsLimiter) {
			continue
		}
		pub.HandleDatagram(dgram)
	}
}

// resumePull puts back the pull handlePopPublish stopped for a leg whose
// claim could not be decided, when anyone is still downstream of the hub
// (HandleLeaseLost's self-demote, for a hub this pod never held).
func (s *Server) resumePull(id string) {
	edges := s.edgeManager()
	if edges == nil || s.registry.DownstreamSessions(id) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), edgeAttachTimeout)
		defer cancel()
		if err := edges.EnsureEdge(ctx, id); err != nil {
			s.log.Warn("pop uplink leg: pull not resumed", "broadcast_key", s.broadcastKey(id), "err", err)
		}
	}()
}

// popOwnership checks id is reserved for pop before anything is touched:
// 0 when it is (or the cache has not seen it yet — the claim decides then),
// else the status to refuse with.
func (s *Server) popOwnership(id string, pop *pops.Pop) int {
	if coord := s.clusterCoord(); coord != nil {
		origin, _, _, ok := coord.Lookup(id)
		if ok && origin.Pop != pop.Name {
			return http.StatusConflict
		}
		if s.popCoord() == nil {
			return http.StatusNotFound
		}
		return 0
	}
	h := s.popDemand()
	if h == nil {
		return http.StatusNotFound
	}
	owner, ok := h.ReservedFor(id)
	switch {
	case !ok:
		return http.StatusNotFound
	case owner != pop.Name:
		return http.StatusConflict
	}
	return 0
}

// claimForPop takes id's Lease as origin for pop's leg (cluster mode; a
// no-op single-pod): the generation, or the status to refuse with.
func (s *Server) claimForPop(ctx context.Context, id, pop string) (int64, int) {
	pc := s.popCoord()
	if s.clusterCoord() == nil {
		return 0, 0
	}
	if pc == nil {
		return 0, http.StatusNotFound
	}
	gen, err := pc.ClaimForPop(ctx, id, pop, true)
	switch {
	case err == nil:
		return gen, 0
	case errors.Is(err, cluster.ErrNotFound):
		return 0, http.StatusNotFound
	case errors.Is(err, cluster.ErrReserved):
		return 0, http.StatusConflict
	}
	s.log.Warn("pop uplink leg claim failed", "broadcast_key", s.broadcastKey(id), "err", err)
	return 0, http.StatusServiceUnavailable
}

// sendLegCapabilities is sendRelayCapabilities for a leg: sent even with no
// flag set, so the PoP decides its delivery on an answer, not a timeout.
func (s *Server) sendLegCapabilities(sess *webtransport.Session, log interface{ Warn(string, ...any) }) {
	caps := wire.RelayCapabilities{}
	if s.cfg.ParityDefault > 0 {
		caps.Flags |= wire.CapParityChunks
		caps.ParityLevel = uint8(s.cfg.ParityDefault)
	}
	if s.cfg.UplinkCarriers {
		caps.Flags |= wire.CapUplinkCarriers
	}
	msg, err := wire.AppendRelayCapabilities(nil, caps)
	if err != nil {
		log.Warn("leg capabilities encode failed", "err", err)
		return
	}
	if err := sendUniMessage(sess, msg); err != nil {
		log.Warn("leg capabilities send failed", "err", err)
	}
}

// startLegClaim records this leg as id's claim on this pod and starts its
// linger watcher, retiring an older leg's watcher.
func (s *Server) startLegClaim(id, pop string, gen int64, sess *webtransport.Session) *popLegClaim {
	c := &popLegClaim{id: id, pop: pop, gen: gen, sess: sess, stop: make(chan struct{})}
	c.live.Store(true)
	s.uplink.mu.Lock()
	if s.uplink.legs == nil {
		s.uplink.legs = map[string]*popLegClaim{}
	}
	if old := s.uplink.legs[id]; old != nil {
		close(old.stop)
	}
	s.uplink.legs[id] = c
	s.uplink.mu.Unlock()
	go s.watchLegLinger(c)
	return c
}

func (s *Server) endLegClaim(c *popLegClaim) {
	s.uplink.mu.Lock()
	if s.uplink.legs[c.id] == c {
		delete(s.uplink.legs, c.id)
	}
	s.uplink.mu.Unlock()
}

// watchLegLinger is docs/76 item 20: once id's hub on this pod has had no
// downstream — no local viewer, edge pull or leg — for -pop-uplink-linger,
// the leg is closed with 4013 PopNoDemand and the Lease goes back to a
// dormant reservation (holder cleared, generation kept), and the hub goes
// too, so the next outside viewer anywhere misses and wants again.
//
// The watcher outlives its session: a leg that ends without 4013 (the
// PoP's broadcaster went away, or the leg died) leaves the hub in its
// ordinary grace with the Lease still held here, so a returning leg's
// force-take re-homes every viewer the docs/22 way. If nobody is left
// watching, the same linger hands the Lease back then — never left to the
// grace deadline, where the cluster janitor would delete the reservation.
// It stops when a newer leg claims the broadcast, or the Lease moved.
func (s *Server) watchLegLinger(c *popLegClaim) {
	defer s.endLegClaim(c)
	linger := s.popUplinkLinger()
	t := time.NewTicker(popLingerTick)
	defer t.Stop()
	var idleSince time.Time
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		if !s.legClaimStands(c) {
			return
		}
		_, g, known := s.registry.BroadcastState(c.id)
		if !known {
			return
		}
		if g > 0 || s.registry.DownstreamSessions(c.id) > 0 {
			idleSince = time.Time{}
			continue
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
			continue
		}
		if time.Since(idleSince) < linger {
			continue
		}
		s.handBack(c)
		return
	}
}

// legClaimStands reports whether c is still this pod's claim of its Lease
// (always, single-pod). The coordinator's held set covers a renewed claim;
// a leg that ended graced the Lease, which stops renewing and drops it from
// that set while the Lease still names this pod at c.gen, so the cached
// Lease decides then. A newer claim here, another pod's force-take or a
// deleted Lease ends the watch.
func (s *Server) legClaimStands(c *popLegClaim) bool {
	coord := s.clusterCoord()
	if coord == nil {
		return true
	}
	if gen, held := coord.OriginGeneration(c.id); held {
		return gen == c.gen
	}
	o, _, _, ok := coord.Lookup(c.id)
	edges := s.edgeManager()
	return ok && edges != nil && o.Holder == edges.podName && o.Generation == c.gen
}

// legReleaser is the coordinator's fenced Release (cluster.Coordinator).
type legReleaser interface {
	ReleaseGeneration(ctx context.Context, broadcastID string, generation int64) (bool, error)
}

// handBack is the linger's end: 4013 to the leg first, then the Lease back
// to a reservation, then the hub. The close goes first because the Lease's
// release is a lease loss on this pod (HandleLeaseLost), which closes the
// publisher session with code 0, and a leg that reads that instead of 4013
// redials: the PoP would put the broadcast straight back on its uplink.
func (s *Server) handBack(c *popLegClaim) {
	c.handedBack.Store(true)
	if c.live.Load() {
		_ = c.sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodePopNoDemand), "no demand")
		// Let the handler's deferred pub.Close run before the hub goes.
		deadline := time.Now().Add(2 * time.Second)
		for c.live.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if coord := s.clusterCoord(); coord != nil {
		if rel, ok := coord.(legReleaser); ok {
			// Fenced on this leg's generation: a newer leg that claimed the
			// Lease here while the close above ran keeps it, its renew
			// loop and its hub.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			released, err := rel.ReleaseGeneration(ctx, c.id, c.gen)
			cancel()
			if err != nil {
				s.log.Warn("pop uplink linger: lease hand-back failed", "broadcast_key", s.broadcastKey(c.id), "err", err)
			} else if !released {
				s.log.Info("pop uplink linger: a newer claim holds the lease; nothing handed back",
					"pop", c.pop, "broadcast_key", s.broadcastKey(c.id))
				return
			}
		}
	}
	s.registry.EndBroadcast(c.id)
	s.log.Info("pop uplink linger: no demand; reservation handed back", "pop", c.pop, "broadcast_key", s.broadcastKey(c.id))
}
