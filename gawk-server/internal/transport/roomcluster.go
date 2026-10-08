package transport

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Cluster-mode rooms (docs/44 §4.5). A public CONNECT /room/{code} on a pod
// not holding the room is either proxied — an internal session to the
// holder's /internal/room/{code} (cluster PSK + lease generation), the
// client's one bidirectional stream piped both ways — or, when the lease is
// absent, released or stale, adopted: force-take with generation CAS,
// rebuild from the CR, serve locally. Either way the holder sees one control
// stream per participant, so the registry has one code path. Reachable only
// with -rooms and -cluster-mode.

// RoomCluster is the transport's slice of *roomcluster.Store.
type RoomCluster interface {
	// Holding reports whether this pod holds the room's lease and at which
	// generation — the internal route's 404/409 fence.
	Holding(code string) (int64, bool)
	// Resolve returns the room's current home from the informer cache.
	Resolve(code string) (roomcluster.Home, bool)
	// Adopt takes the room over when its lease is not live; a live foreign
	// home is roomcluster.ErrHeldElsewhere.
	Adopt(ctx context.Context, code string) error
	// ReleaseAll releases this pod's room leases (the drain).
	ReleaseAll(ctx context.Context)
}

// roomClusterWiring is everything SetRoomCluster installs, published as one
// atomic pointer for the reason clusterWiring is.
type roomClusterWiring struct {
	store   RoomCluster
	podName string
	dial    roomProxyDialer
}

// roomProxyUpstream is one internal session to a room's home.
type roomProxyUpstream struct {
	sess *webtransport.Session
	d    *webtransport.Transport
}

func (u *roomProxyUpstream) close(code uint32, reason string) {
	_ = u.sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
	_ = u.d.Close()
}

// roomProxyDialer opens the internal room session. A pre-upgrade rejection
// by the home comes back as a non-zero status with the error, so the
// receiving pod can answer the participant with the same status.
type roomProxyDialer func(ctx context.Context, addr, path string) (status int, up *roomProxyUpstream, err error)

// newRoomProxyDialer is newEdgeDialer's twin for room control (same TLS,
// timers, PSK and internal Origin).
func newRoomProxyDialer(serverName, psk string, rootCAs *x509.CertPool) roomProxyDialer {
	return func(ctx context.Context, addr, path string) (int, *roomProxyUpstream, error) {
		d := &webtransport.Transport{
			TLSClientConfig: &tls.Config{ServerName: serverName, RootCAs: rootCAs},
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
				MaxIdleTimeout:                   edgeIdleTimeout,
				KeepAlivePeriod:                  edgeKeepAlive,
			},
		}
		target := "https://" + addr + path + "&psk=" + url.QueryEscape(psk)
		rsp, sess, err := d.Dial(ctx, target, http.Header{"Origin": []string{internalEdgeOrigin}})
		if err != nil {
			_ = d.Close()
			if rsp != nil {
				return rsp.StatusCode, nil, fmt.Errorf("internal room dial to %s: status %d: %w", addr, rsp.StatusCode, err)
			}
			return 0, nil, err
		}
		return 0, &roomProxyUpstream{sess: sess, d: d}, nil
	}
}

// SetRoomCluster installs the room store. Call after SetCluster
// (the drain hook chains onto the lease release) and before Run.
func (s *Server) SetRoomCluster(rc RoomCluster, podName string) {
	s.roomCluster.Store(&roomClusterWiring{
		store:   rc,
		podName: podName,
		dial:    newRoomProxyDialer(s.cfg.InternalServerName, s.cfg.InternalPSK, nil),
	})
	prev := s.onDrain
	s.onDrain = func() {
		if prev != nil {
			prev()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rc.ReleaseAll(ctx)
	}
}

func (s *Server) roomClusterWiring() *roomClusterWiring { return s.roomCluster.Load() }

// roomHomedElsewhere is handleRoom's cluster gate. True means it answered
// the request itself (proxied or rejected); false means serve locally — this
// pod holds or just adopted the room, or no CR exists and the local registry
// decides.
func (s *Server) roomHomedElsewhere(w http.ResponseWriter, r *http.Request, code string) bool {
	rc := s.roomClusterWiring()
	if rc == nil {
		return false
	}
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return false
	}
	if _, held := rc.store.Holding(norm); held {
		return false
	}
	home, found := rc.store.Resolve(norm)
	if !found {
		return false
	}
	if home.Live && home.Holder != rc.podName {
		s.proxyRoom(w, r, rc, norm, home)
		return true
	}
	// Absent, released or stale lease: adopt.
	switch err := rc.store.Adopt(r.Context(), norm); {
	case err == nil:
		s.metrics.Connection("room-adopt", metrics.OutcomeAccepted)
		return false
	case errors.Is(err, roomcluster.ErrHeldElsewhere):
		// Lost the race to another pod: it is the home now.
		if home, ok := rc.store.Resolve(norm); ok && home.Live && home.Holder != rc.podName {
			s.proxyRoom(w, r, rc, norm, home)
			return true
		}
		s.metrics.Connection("room-adopt", metrics.OutcomeConflict)
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	case errors.Is(err, roomcluster.ErrNotFound):
		s.metrics.Connection("room-adopt", metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return true
	default:
		s.metrics.Connection("room-adopt", metrics.OutcomeError)
		s.log.Warn("room adoption failed", "room_key", s.broadcastKey(norm), "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
}

// roomProxyPath builds the internal route: the participant's own query
// (creator token, attach secret, nickname fallback) forwarded untouched,
// plus the lease generation. The PSK is the dialer's to add.
func roomProxyPath(code string, gen int64, q url.Values) string {
	fwd := url.Values{}
	for k, v := range q {
		if k == "psk" || k == "gen" {
			continue
		}
		fwd[k] = v
	}
	fwd.Set("gen", strconv.FormatInt(gen, 10))
	return "/internal/room/" + code + "?" + fwd.Encode()
}

// proxyRoom forwards one participant's control session to the home pod.
func (s *Server) proxyRoom(w http.ResponseWriter, r *http.Request, rc *roomClusterWiring, code string, home roomcluster.Home) {
	const route = "room-proxy"
	log := s.log.With("route", route, "room_key", s.broadcastKey(code), "home", home.Holder)
	dialCtx, cancel := context.WithTimeout(r.Context(), edgeAttachTimeout)
	status, up, err := rc.dial(dialCtx, home.Addr, roomProxyPath(code, home.Generation, r.URL.Query()))
	cancel()
	if err != nil {
		if status != 0 {
			// The home answered pre-upgrade: pass its status through.
			s.metrics.Connection(route, metrics.OutcomeUnauthorized)
			log.Info("room proxy: home rejected pre-upgrade", "status", status)
			w.WriteHeader(status)
			return
		}
		// Dead upstream, but the lease is still live by the cache's clock,
		// so not adoptable yet; the client's reconnect adopts after staleness.
		s.metrics.Connection(route, metrics.OutcomeError)
		log.Warn("room proxy: home unreachable", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		up.close(0, "participant upgrade failed")
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		log.Warn("room proxy upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	defer s.trackSession(sess)()
	defer s.trackProxied(code, home.Kind)()
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	log = log.With("remote", sess.RemoteAddr())

	ctx := r.Context()
	helloCtx, cancel := context.WithTimeout(ctx, roomHelloTimeout)
	client, err := sess.AcceptStream(helloCtx)
	cancel()
	if err != nil {
		log.Warn("room proxy: participant opened no control stream", "err", err)
		sess.CloseWithError(roomCloseBadRequest, "no control stream")
		up.close(0, "participant left")
		return
	}
	upstream, err := up.sess.OpenStreamSync(ctx)
	if err != nil {
		log.Warn("room proxy: upstream stream open failed", "err", err)
		sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeServerDraining), "room home lost")
		up.close(0, "stream open failed")
		return
	}
	log.Info("room proxy session started", "generation", home.Generation)

	// Pipe both ways; whichever side ends first decides the other's fate.
	// The participant leaving closes the upstream cleanly. The upstream
	// ending closes the participant with the home's own code when it sent
	// one (RoomEnded 4007 must reach the client to be terminal), else with
	// the non-terminal draining code so it reconnects wherever the room
	// lands. The copies are raced: a quiet room sends nothing that would
	// fail against a dead participant, so waiting on home→participant
	// alone would leave a departed participant on the roster.
	fromClient := make(chan error, 1)
	go func() {
		_, err := io.Copy(upstream, client)
		fromClient <- err
	}()
	fromHome := make(chan error, 1)
	go func() {
		_, err := io.Copy(client, upstream)
		fromHome <- err
	}()
	participantLeft := func(err error) {
		log.Info("room proxy session ended", "reason", sessionEndReason(ctx, err))
		sess.CloseWithError(0, "")
		up.close(0, "participant left")
	}
	select {
	case err := <-fromClient:
		participantLeft(err)
	case upErr := <-fromHome:
		select {
		case err := <-fromClient:
			// Both ended at once: a dead participant also fails the
			// home→participant write.
			participantLeft(err)
			return
		default:
		}
		code, reason := uint32(wire.CloseCodeServerDraining), "room home lost"
		var se *webtransport.SessionError
		if errors.As(upErr, &se) {
			code, reason = uint32(se.ErrorCode), "room home closed"
		}
		log.Info("room proxy upstream ended", "close_code", code, "err", upErr)
		sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
		up.close(0, "")
	}
}

// handleInternalRoom is the home's side of proxying. Gates, in order: 404
// without cluster wiring, 401 bad PSK, 404 not the home, 409 stale
// generation, then the ordinary join gate (CheckJoin with the forwarded
// params) and serveRoomSession. No rate limit or ban check: the proxying pod
// already applied both.
func (s *Server) handleInternalRoom(w http.ResponseWriter, r *http.Request) {
	const route = "internal-room"
	if s.rejectedDraining(w, route) {
		return
	}
	rc := s.roomClusterWiring()
	reg := s.roomRegistry()
	if rc == nil || reg == nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("psk")), []byte(s.cfg.InternalPSK)) != 1 {
		s.metrics.Connection(route, metrics.OutcomeUnauthorized)
		s.log.Warn("internal room unauthorized: bad PSK", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	norm, err := rooms.NormalizeCode(r.PathValue("code"))
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	heldGen, held := rc.store.Holding(norm)
	if !held {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		s.log.Warn("internal room rejected: not home", "room_key", s.broadcastKey(norm), "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	gen, err := strconv.ParseInt(q.Get("gen"), 10, 64)
	if err != nil || gen != heldGen {
		s.metrics.Connection(route, metrics.OutcomeConflict)
		s.log.Warn("internal room rejected: stale generation", "room_key", s.broadcastKey(norm),
			"remote", r.RemoteAddr, "got", q.Get("gen"), "held", heldGen)
		w.WriteHeader(http.StatusConflict)
		return
	}
	grants, err := reg.CheckJoin(norm, q.Get("creator"), q.Get("attach"), q.Has("attach"))
	if err != nil {
		status, outcome := roomJoinStatus(err)
		s.metrics.Connection(route, outcome)
		s.log.Warn("internal room join rejected pre-upgrade", "room_key", s.broadcastKey(norm), "status", status, "err", err)
		w.WriteHeader(status)
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		s.log.Warn("internal room upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	s.serveRoomSession(r, sess, reg, norm, grants, nil)
}

// HandleRoomLeaseLost is the store's OnLeaseLost dispatch: this pod's copy of
// the room is stale. Its sessions are closed with the non-terminal draining
// code so they reconnect wherever the room now lives, and the local copy is
// dropped so a later adoption rebuilds from the CR. EndRoom's 4007 reaches
// nobody: the first close code on a QUIC session is the one the peer sees.
func (s *Server) HandleRoomLeaseLost(code string) {
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return
	}
	s.sessMu.Lock()
	sessions := make([]drainSession, 0, len(s.roomSessions[norm]))
	for sess := range s.roomSessions[norm] {
		sessions = append(sessions, sess)
	}
	s.sessMu.Unlock()
	s.log.Info("room lease lost: closing local control sessions", "room_key", s.broadcastKey(norm), "sessions", len(sessions))
	for _, sess := range sessions {
		_ = sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeServerDraining), "room re-homed")
	}
	if reg := s.roomRegistry(); reg != nil {
		// Announce the move first — participant_left{reason: home_moved}
		// each, and no room.closed from EndRoom — or bus consumers would see a
		// live room end and its people vanish.
		reg.ReleaseHome(norm)
		reg.EndRoom(norm, wire.RoomEndReasonOperator)
	}
}

// trackRoomSession registers a local control session under its room so a
// lease loss can close exactly that room's sessions.
func (s *Server) trackRoomSession(code string, sess drainSession) func() {
	s.sessMu.Lock()
	if s.roomSessions == nil {
		s.roomSessions = make(map[string]map[drainSession]struct{})
	}
	set := s.roomSessions[code]
	if set == nil {
		set = make(map[drainSession]struct{})
		s.roomSessions[code] = set
	}
	set[sess] = struct{}{}
	s.sessMu.Unlock()
	return func() {
		s.sessMu.Lock()
		if set := s.roomSessions[code]; set != nil {
			delete(set, sess)
			if len(set) == 0 {
				delete(s.roomSessions, code)
			}
		}
		s.sessMu.Unlock()
	}
}

// proxiedRoom is the /statusz view of one proxied room on this pod.
type proxiedRoom struct {
	kind     string
	sessions int
}

// trackProxied counts one forwarded control session for /statusz and the
// gawk_room_proxied_sessions gauge.
func (s *Server) trackProxied(code, kind string) func() {
	s.sessMu.Lock()
	if s.proxiedRooms == nil {
		s.proxiedRooms = make(map[string]*proxiedRoom)
	}
	p := s.proxiedRooms[code]
	if p == nil {
		p = &proxiedRoom{kind: kind}
		s.proxiedRooms[code] = p
	}
	p.sessions++
	s.sessMu.Unlock()
	return func() {
		s.sessMu.Lock()
		if p := s.proxiedRooms[code]; p != nil {
			p.sessions--
			if p.sessions <= 0 {
				delete(s.proxiedRooms, code)
			}
		}
		s.sessMu.Unlock()
	}
}

// RoomStats is the /statusz + metrics rooms source: the registry's "home"
// rows plus one "proxy" row per room this pod forwards, keyed by the same
// HMAC. Nil with -rooms off. Implements ops.RoomStatsSource and
// metrics.RoomStatsSource.
func (s *Server) RoomStats() map[string]roomsrv.RoomStats {
	reg := s.roomRegistry()
	if reg == nil {
		return nil
	}
	out := reg.Stats()
	s.sessMu.Lock()
	for code, p := range s.proxiedRooms {
		key := s.broadcastKey(code)
		if _, home := out[key]; home {
			continue
		}
		out[key] = roomsrv.RoomStats{Kind: p.kind, Participants: p.sessions, Role: "proxy"}
	}
	s.sessMu.Unlock()
	return out
}

// TotalStats forwards the registry's totals (metrics.RoomStatsSource).
func (r roomStatsSource) TotalStats() roomsrv.Totals {
	if reg := r.s.roomRegistry(); reg != nil {
		return reg.TotalStats()
	}
	return roomsrv.Totals{}
}

// RoomStatsSource returns the rooms stats source for the ops endpoint and
// the metrics collector, or a nil interface when rooms are off (a typed nil
// would render an empty section).
func (s *Server) RoomStatsSource() metrics.RoomStatsSource {
	if s.roomRegistry() == nil {
		return nil
	}
	return roomStatsSource{s}
}
