// Package transport owns the HTTP/3 + WebTransport endpoint: routes,
// session acceptance and server lifecycle. It contains no media logic —
// datagrams are handed to the hub as opaque bytes.
package transport

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/clientinfo"
	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/ops"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// drainWindow bounds the SIGTERM drain (docs/22): every open session gets
// wire.CloseCodeServerDraining, staggered across this window so the reconnect
// herd doesn't spike. It runs while the pod is still Ready, so conntrack still
// routes the close frames to the peers. Must stay well inside
// terminationGracePeriodSeconds.
const drainWindow = time.Second

// drainFlushDelay lets the last 4002 close frames reach their peers before Run
// tears the QUIC connections down; a CONNECTION_CLOSE racing the close capsule
// would turn the clean drain signal into an anonymous drop.
const drainFlushDelay = 250 * time.Millisecond

// How many times, and how far apart, a subscriber is told what delivery it
// was served: the announcement is one unreliable datagram, so a small repeat
// removes a single point of failure. The burst is tight on purpose — it only
// covers the startup race before the viewer's reader drains, and a datagram is
// ack-eliciting, so repeats spread over seconds act as a keepalive and hold
// the connection past -max-idle-timeout
// (TestIdleSubscriberTimesOutWithoutKeepalive).
const (
	deliveryAckAnnouncements   = 4
	deliveryAckReannounceEvery = 150 * time.Millisecond
)

// drainSession is the slice of a webtransport.Session the drain needs;
// narrowed to an interface so the drain logic is unit-testable with fakes.
type drainSession interface {
	CloseWithError(code webtransport.SessionErrorCode, reason string) error
}

// ClusterCoordinator is the transport's slice of *cluster.Coordinator
// (docs/22). Nil means single-pod mode: no claims, no releases, no edge pulls.
type ClusterCoordinator interface {
	// Claim acquires the broadcast's origin Lease for this pod. force is set
	// when the claimant presented a valid resume token (every /publish/{id});
	// mint-path claims are create-only (never steal a live holder).
	Claim(ctx context.Context, broadcastID string, force bool) (int64, error)
	// ReleaseAll clears this pod's lease holderships (the SIGTERM drain) so
	// the broadcaster's instant reconnect can claim on another pod.
	ReleaseAll(ctx context.Context)
	// Resolve returns the broadcast's current origin (edge pull).
	Resolve(ctx context.Context, broadcastID string) (cluster.Origin, error)
	// OriginGeneration reports whether this pod holds the broadcast's lease
	// and at which generation — the internal route's 404/409 fence.
	OriginGeneration(broadcastID string) (int64, bool)
	// Lookup is Resolve's cached, non-blocking twin: the lease as the
	// informer last saw it (ok false before sync or with no lease; inGrace
	// once the origin is away; stalled while its publisher is silent).
	Lookup(broadcastID string) (origin cluster.Origin, inGrace, stalled, ok bool)
}

// Server wraps a webtransport.Server with the gawk routes.
type Server struct {
	cfg      config.Config
	registry *hub.Registry
	log      *slog.Logger
	wt       *webtransport.Server
	limiter  *ipRateLimiter
	// resume mints/verifies the /publish/{id} resume tokens.
	resume *resumeTokens
	// wiring holds everything SetCluster installs; nil when -cluster-mode is
	// off. Read it through clusterCoord/edgeManager, never directly.
	wiring atomic.Pointer[clusterWiring]
	// metrics carries the connection-outcome counters; nil is safe (all
	// methods are nil-receiver no-ops) so tests can run the server unwired.
	metrics *metrics.ServerMetrics

	// bans is the moderation ban set (nil = -moderation-source=off; every
	// check is then a cheap miss). Settable once, atomic like `wiring`; read
	// it through banSet.
	bans atomic.Pointer[moderation.Set]
	// rooms is the room registry (docs/44); nil with -rooms off. Settable
	// once, atomic like bans; read it through roomRegistry.
	rooms atomic.Pointer[roomsrv.Registry]
	// roomCluster is the cluster wiring for rooms (roomcluster.go);
	// nil without -cluster-mode. Same discipline as `wiring`.
	roomCluster atomic.Pointer[roomClusterWiring]
	// now is the clock ban expiry is evaluated against; injectable so tests
	// can step past an expiry without sleeping.
	now func() time.Time

	// Open-session tracking for the SIGTERM drain: every upgraded
	// session registers here and unregisters when its handler returns.
	sessMu   sync.Mutex
	sessions map[drainSession]struct{}
	// roomSessions indexes local room control sessions by normalized code
	// (lease loss closes one room's sessions); proxiedRooms counts the
	// sessions this pod forwards to other homes (/statusz "proxy" rows).
	// Both under sessMu.
	roomSessions map[string]map[drainSession]struct{}
	proxiedRooms map[string]*proxiedRoom
	// Live publisher sessions by broadcast ID: the demote path closes the
	// stale one when the broadcast's Lease is force-taken.
	publishers map[string]*publisherSession
	draining   atomic.Bool
	// onDrain runs after the 4002s have been sent, before Run returns — the
	// seam that releases this pod's broadcast Leases. Nil-safe.
	onDrain func()
	// drainSleep staggers the per-session closes; injectable so the drain
	// unit test asserts exact timing without wall-clock sleeps.
	drainSleep func(time.Duration)

	// testHookPostUpgradeSubscribe runs between the session upgrade and the
	// authoritative Subscribe, widening the CheckSubscribe→Subscribe race
	// window for tests. Atomic because tests install it while handlers are
	// serving, and in-process UDP loopback gives no happens-before edge.
	testHookPostUpgradeSubscribe atomic.Pointer[func(id string)]

	// testHookPostUpgradePublish runs between the publish upgrade and
	// trackPublisher (see postUpgradePublishHook). Atomic, as above.
	testHookPostUpgradePublish atomic.Pointer[func(id string)]

	// testHookRateLimitLoopback disables the limiter's loopback bypass so
	// tests (dialing from 127.0.0.1) can hit the 429 path. Atomic, as above.
	testHookRateLimitLoopback atomic.Bool

	// testHookOriginCheckLoopback disables the origin allowlist's loopback
	// bypass so in-process cluster tests run the production config shape;
	// with the bypass, an internal-route origin rejection is invisible to
	// every test. Atomic, as above.
	testHookOriginCheckLoopback atomic.Bool
}

// rejectedDraining rejects a new CONNECT with 503 once the drain has begun,
// rather than accept a session only to 4002 it and burn a client reconnect.
func (s *Server) rejectedDraining(w http.ResponseWriter, route string) bool {
	if !s.draining.Load() {
		return false
	}
	s.metrics.Connection(route, metrics.OutcomeDraining)
	w.WriteHeader(http.StatusServiceUnavailable)
	return true
}

// rateLimited reports whether a connection attempt should be rejected with
// 429. Loopback bypasses it (k8s exec probes hit /echo every cycle) unless
// the test hook forces it; trusted CIDRs bypass it too (behind MetalLB L2 +
// etp=Cluster the relay sees SNAT'd node IPs, and a rollout's reconnect herd
// must not burn viewers' fatal-on-first-connect budget).
func (s *Server) rateLimited(r *http.Request) bool {
	if s.limiter == nil {
		return false
	}
	if isLoopbackAddr(r.RemoteAddr) && !s.testHookRateLimitLoopback.Load() {
		return false
	}
	if isTrustedAddr(r.RemoteAddr, s.cfg.TrustedCIDRs) {
		return false
	}
	if s.limiter.Allow(r.RemoteAddr) {
		return false
	}
	s.metrics.RateLimited()
	s.log.Warn("connection rate limited",
		"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "path", r.URL.Path)
	return true
}

// SetModeration installs the ban set (docs/42 §4.3). Call once from main,
// before Run; without one nothing is enforced (-moderation-source=off). Stored
// atomically because a violated "before Run" would otherwise be a silent data
// race on the publish hot path.
func (s *Server) SetModeration(bans *moderation.Set) { s.bans.Store(bans) }

// banSet returns the ban set, or nil with -moderation-source=off. Every
// moderation.Set method is nil-receiver-safe, so callers need no guard.
func (s *Server) banSet() *moderation.Set { return s.bans.Load() }

// remoteIP extracts the peer address from an http.Request's RemoteAddr,
// canonicalized (zone stripped, v4-mapped-v6 collapsed) so a ban written as
// 203.0.113.7/32 catches a peer the stack reports as ::ffff:203.0.113.7.
// Returns an invalid Addr when RemoteAddr is not an address, which every
// caller must treat as "no IP to match" rather than "matches nothing".
func remoteIP(remoteAddr string) netip.Addr {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return moderation.CanonicalAddr(addr)
}

// rejectBanned answers a banned publish attempt with HTTP 451 pre-upgrade
// (403 means "token / secret rejected", so native broadcasters can say
// "banned"; a browser just sees a dial failure). Warn withholds the ban
// reason (operator-private) and the raw ID (a join capability); it names the
// broadcast by its HMAC'd key, and Debug carries both.
func (s *Server) rejectBanned(w http.ResponseWriter, r *http.Request, rec moderation.Record, broadcastID string) {
	s.metrics.Connection("publish", metrics.OutcomeBanned)
	attrs := []any{
		"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"),
		"target_type", string(rec.Target.Type),
	}
	if broadcastID != "" {
		// Only the claim path has one; the mint path is rejected before an ID
		// exists at all.
		attrs = append(attrs, "broadcast_key", s.broadcastKey(broadcastID))
	}
	s.log.Warn("publish rejected: banned", attrs...)
	s.log.Debug("publish ban detail",
		"id", broadcastID, "remote", r.RemoteAddr, "target_type", string(rec.Target.Type),
		"target", rec.Target.Value, "ban_reason", rec.Reason, "created_by", rec.CreatedBy)
	w.WriteHeader(http.StatusUnavailableForLegalReasons)
}

// bannedPublisher reports whether either handle of a live publisher — its
// broadcast ID or its source address — is banned right now.
func (s *Server) bannedPublisher(broadcastID string, peer netip.Addr) (moderation.Record, bool) {
	now := s.clock()
	if rec, banned := s.banSet().BannedIP(peer, now); banned {
		return rec, true
	}
	return s.banSet().BannedID(broadcastID, now)
}

// broadcastKey is the HMAC'd handle a broadcast may be named by at Info+ (raw
// IDs are joinable and only ~31^6 strong) — the same key /statusz and the
// metrics labels use. Empty without a registry (unit fixtures).
func (s *Server) broadcastKey(broadcastID string) string {
	if s.registry == nil || broadcastID == "" {
		return ""
	}
	return s.registry.ObfuscateID(broadcastID)
}

// postUpgradePublishHook runs as the ban TOCTOU window opens: the pre-upgrade
// ban gate is behind us and trackPublisher has not yet made the session
// findable. Tests land a ban here; production never sets one.
func (s *Server) postUpgradePublishHook(id string) {
	if hook := s.testHookPostUpgradePublish.Load(); hook != nil {
		(*hook)(id)
	}
}

// clock returns the time ban expiry is evaluated against.
func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// New builds the server. getCert supplies the TLS certificate per handshake
// (a tlsutil.Reloader in production, a fixed dev cert locally). m carries the
// connection-outcome counters and may be nil (tests).
func New(cfg config.Config, r *hub.Registry, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), log *slog.Logger, m *metrics.ServerMetrics) *Server {
	var limiter *ipRateLimiter
	if cfg.ConnRateLimit > 0 {
		limiter = newIPRateLimiter(cfg.ConnRateLimit, cfg.ConnBurstLimit)
	}
	s := &Server{
		cfg:        cfg,
		registry:   r,
		log:        log,
		limiter:    limiter,
		metrics:    m,
		resume:     newResumeTokens(cfg),
		sessions:   make(map[drainSession]struct{}),
		publishers: make(map[string]*publisherSession),
		drainSleep: time.Sleep,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	// Same handler as the TCP ops endpoint (single definition; see ops).
	mux.HandleFunc("GET /statusz", ops.StatuszHandler(r, roomStatsSource{s}, log))
	mux.HandleFunc("CONNECT /echo", s.handleEcho)
	mux.HandleFunc("CONNECT /publish", s.handlePublish)
	mux.HandleFunc("CONNECT /publish/{id}", s.handlePublish)
	mux.HandleFunc("CONNECT /subscribe/{id}", s.handleSubscribe)
	// Pod-to-pod edge pull; 404s outright unless -cluster-mode is on.
	mux.HandleFunc("CONNECT /internal/subscribe/{id}", s.handleInternalSubscribe)
	// Rooms: the routes exist only with -rooms on — otherwise not even a 404
	// handler of ours answers here. "new" is the literal mint route; the mux
	// prefers it over the {code} pattern.
	if cfg.Rooms {
		mux.HandleFunc("CONNECT /room/new", s.handleRoomNew)
		mux.HandleFunc("CONNECT /room/{code}", s.handleRoom)
		// Pod-to-pod room proxying; 404s outright unless -cluster-mode
		// installed the room store.
		mux.HandleFunc("CONNECT /internal/room/{code}", s.handleInternalRoom)
	}

	s.wt = &webtransport.Server{
		// WebTransport flow-control SETTINGS: the three WT_INITIAL_MAX_* are
		// mandatory whenever WT_MAX_SESSIONS > 1 (always, in webtransport-go),
		// and Safari refuses the session without them, leaving no relay-side
		// trace (docs/gotchas.md). The values are maximal and never bind; the
		// QUIC limits stay the effective caps. Server.Config is the only door
		// (H3.AdditionalSettings is wiped by Server.init). With a peer that
		// also advertises them, OpenUniStream returns StreamLimitReachedError
		// instead of blocking, which the keyframe path treats as a drop.
		Config: &webtransport.Config{
			MaxIncomingStreams:    1 << 60,
			MaxIncomingUniStreams: 1 << 60,
			MaxIncomingData:       1 << 60,
		},
		H3: &http3.Server{
			Addr: cfg.Addr,
			// ConfigureTLSConfig adds the h3 ALPN; webtransport-go passes
			// this config to quic.ListenEarly as-is, so it must be set here.
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{GetCertificate: getCert}),
			Handler:         mux,
			EnableDatagrams: true,
			// Keep the QUIC connection's context reachable from every
			// handler, so a session that ends can say why (endreason.go).
			ConnContext: withConnContext,
			QUICConfig: &quic.Config{
				EnableDatagrams: true,
				MaxIdleTimeout:  cfg.MaxIdleTimeout,
				// The keepalive keeps idle subscribers alive while the broadcaster is
				// away: the effective idle timeout is the min of both endpoints'
				// (browsers advertise ~30s), and PINGs reset both timers.
				KeepAlivePeriod: cfg.KeepAlivePeriod,
				// Required by webtransport-go v0.11+.
				EnableStreamResetPartialDelivery: true,
			},
		},
	}
	// Adds the WebTransport SETTINGS to the h3 server; without this the
	// browser (and Dialer) reject with "server didn't enable WebTransport".
	webtransport.ConfigureHTTP3Server(s.wt.H3)

	if len(cfg.AllowedOrigins) > 0 {
		allowed := cfg.AllowedOrigins
		s.wt.CheckOrigin = func(r *http.Request) bool {
			// k8s probes exec gawk-echo against 127.0.0.1 with no Origin header;
			// without this bypass every deployment with AllowedOrigins fails its
			// own probes. Loopback can't be spoofed over QUIC (full handshake).
			if isLoopbackAddr(r.RemoteAddr) && !s.testHookOriginCheckLoopback.Load() {
				return true
			}
			origin := r.Header.Get("Origin")
			// Edge pulls announce a fixed origin, honored only on the PSK-gated
			// internal route; the allowlist still governs client-facing paths.
			if origin == internalEdgeOrigin && strings.HasPrefix(r.URL.Path, "/internal/") {
				return true
			}
			if slices.Contains(allowed, origin) {
				return true
			}
			// webtransport-go otherwise rejects silently; log the origin and
			// remote so blocked dials are visible to operators.
			s.metrics.OriginRejected()
			s.log.Warn("origin rejected", "origin", origin, "remote", r.RemoteAddr, "path", r.URL.Path)
			return false
		}
	} else {
		// Dev default: accept any origin (webtransport-go's default requires
		// Origin == Host, breaking e.g. a Vite app on :5173 → :4433).
		s.wt.CheckOrigin = func(*http.Request) bool { return true }
	}

	return s
}

// Run serves until ctx is cancelled, then drains and closes the server. It
// returns a non-nil listen error, or nil after a graceful shutdown.
//
// The QUIC transport is built explicitly (not via wt.ListenAndServe) so the
// fleet-shared StatelessResetKey can be set: any pod then answers packets for
// an unknown connection ID with a stateless reset, detecting abrupt pod death
// in ~1 RTT instead of the ~30 s idle timeout.
func (s *Server) Run(ctx context.Context) error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.Addr)
	if err != nil {
		return err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	tr := &quic.Transport{Conn: udpConn}
	if len(s.cfg.StatelessResetKey) == 32 {
		key := quic.StatelessResetKey(s.cfg.StatelessResetKey)
		tr.StatelessResetKey = &key
	}
	// Mirror webtransport-go's serve(): clone the H3 QUIC config and force
	// the two capabilities it would force itself.
	quicConf := s.wt.H3.QUICConfig.Clone()
	quicConf.EnableDatagrams = true
	quicConf.EnableStreamResetPartialDelivery = true
	ln, err := tr.ListenEarly(s.wt.H3.TLSConfig, quicConf)
	if err != nil {
		tr.Close()
		udpConn.Close()
		return err
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.acceptLoop(ln) }()

	s.log.Info("listening", "addr", s.cfg.Addr,
		"stateless_reset_key_set", tr.StatelessResetKey != nil)
	closeAll := func() {
		s.wt.Close()
		ln.Close()
		tr.Close()
		udpConn.Close()
	}
	defer func() {
		if s.limiter != nil {
			s.limiter.Close()
		}
	}()
	select {
	case <-ctx.Done():
		s.drain()
		closeAll()
		<-errCh // wait for the accept loop to return
		return nil
	case err := <-errCh:
		closeAll()
		return err
	}
}

// acceptLoop hands every accepted QUIC connection to the WebTransport server.
// It returns when the listener is closed.
func (s *Server) acceptLoop(ln *quic.EarlyListener) error {
	for {
		qconn, err := ln.Accept(context.Background())
		if err != nil {
			return err
		}
		go func() {
			if err := s.wt.ServeQUICConn(qconn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Debug("QUIC connection ended", "err", err)
			}
		}()
	}
}

// trackSession registers an upgraded session for the SIGTERM drain; the
// returned func unregisters it (deferred by every session handler).
func (s *Server) trackSession(sess drainSession) func() {
	s.sessMu.Lock()
	s.sessions[sess] = struct{}{}
	s.sessMu.Unlock()
	return func() {
		s.sessMu.Lock()
		delete(s.sessions, sess)
		s.sessMu.Unlock()
	}
}

// publisherSession is what the pod remembers about a live publisher.
type publisherSession struct {
	sess drainSession
	// remote is the publisher's source address, recorded at track time so an
	// IP ban can find the live session long after the CONNECT request is
	// gone. Invalid when unparseable.
	remote netip.Addr
}

// trackPublisher additionally indexes a publisher session by broadcast ID
// so the demote path can close the stale one on lease loss, and so an IP ban
// can find it.
func (s *Server) trackPublisher(id string, sess drainSession, remote netip.Addr) func() {
	entry := &publisherSession{sess: sess, remote: remote}
	s.sessMu.Lock()
	s.publishers[id] = entry
	s.sessMu.Unlock()
	return func() {
		s.sessMu.Lock()
		if s.publishers[id] == entry {
			delete(s.publishers, id)
		}
		s.sessMu.Unlock()
	}
}

// PublisherRemote returns the recorded source address of a broadcast's live
// publisher: what the IP-ban kill walks, and publisherRemoteIp on GET
// /internal/admin/broadcasts. Exported so the ops package need not re-derive
// session bookkeeping.
func (s *Server) PublisherRemote(id string) (netip.Addr, bool) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	entry, ok := s.publishers[id]
	if !ok {
		return netip.Addr{}, false
	}
	return entry.remote, entry.remote.IsValid()
}

// HandleLeaseLost is the demote path (docs/22): this pod's Lease was
// force-taken — the broadcaster re-homed (NAT rebind / rollout reconnect)
// while our publisher session still looks half-alive. Close that stale
// session, close downstream edge sessions with 4003 so they re-resolve, and
// become an edge ourselves for any local viewers (nobody chases viewers
// across pods; depth stays ≤ 2 because the new origin serves us directly).
func (s *Server) HandleLeaseLost(broadcastID string, _ cluster.Origin) {
	s.sessMu.Lock()
	stale := s.publishers[broadcastID]
	delete(s.publishers, broadcastID)
	s.sessMu.Unlock()
	if stale != nil {
		s.log.Info("origin lease lost: closing stale publisher session", "broadcast_id", broadcastID)
		_ = stale.sess.CloseWithError(0, "origin moved")
	}

	s.registry.CloseInternalSubscribers(broadcastID, uint32(wire.CloseCodeOriginMoved), "origin moved")

	if edges := s.edgeManager(); edges != nil && s.registry.ExternalSubscribers(broadcastID) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), edgeAttachTimeout)
			defer cancel()
			if err := edges.EnsureEdge(ctx, broadcastID); err != nil {
				s.log.Warn("self-demote to edge failed", "broadcast_id", broadcastID, "err", err)
			} else {
				s.log.Info("demoted to edge after lease loss", "broadcast_id", broadcastID)
			}
		}()
	}
}

// drain implements the close-first-while-Ready shutdown (docs/22): flip
// readiness, send 4002 to every open session staggered over drainWindow, then
// run onDrain. New CONNECTs get 503 once draining.
func (s *Server) drain() {
	s.draining.Store(true)
	s.sessMu.Lock()
	sessions := make([]drainSession, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessMu.Unlock()

	if len(sessions) > 0 {
		s.log.Info("draining: closing open sessions", "sessions", len(sessions), "window", drainWindow)
		interval := drainWindow / time.Duration(len(sessions))
		for i, sess := range sessions {
			if i > 0 {
				s.drainSleep(interval)
			}
			_ = sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeServerDraining), "server draining")
		}
		s.drainSleep(drainFlushDelay)
	}
	if s.onDrain != nil {
		s.onDrain()
	}
}

// Ready reports whether the server is accepting new work (/readyz). It is
// scale-down hygiene; rollout correctness comes from the active drain.
func (s *Server) Ready() bool {
	return !s.draining.Load()
}

// SetCluster wires the origin-Lease coordinator: /publish claims acquire the
// Lease, the drain releases this pod's holderships right after the 4002s (so
// the broadcaster's reconnect claims on a ready pod with no TTL wait), and
// viewers for broadcasts we don't host trigger an edge pull. podName is this
// pod's identity (the self-dial guard). Call once, before Run.
//
// Published as one atomic pointer: the ban informer reaches the edge manager
// from its own goroutine (HandleBanAdded -> terminate()), and a plain field
// would turn any startup overlap into a silent data race.
func (s *Server) SetCluster(c ClusterCoordinator, podName string) {
	em := newEdgeManager(s.registry, c,
		newEdgeDialer(s.cfg.InternalServerName, s.cfg.InternalPSK, nil, s.log),
		podName, s.log)
	// Every pod that held a killed broadcast counts its own kill, and an
	// edge's usually arrives as its origin's 4006 rather than a Ban event.
	em.terminated = s.metrics.Termination
	s.wiring.Store(&clusterWiring{coord: c, edges: em})
	s.onDrain = func() {
		em.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.ReleaseAll(ctx)
	}
}

// clusterWiring is everything SetCluster installs that another goroutine can
// read. Grouped into one value so the coordinator and the edge manager are
// published together — a reader that sees one has seen the other.
type clusterWiring struct {
	coord ClusterCoordinator
	edges *EdgeManager
}

// clusterCoord returns the origin-Lease coordinator, or nil with
// -cluster-mode off.
func (s *Server) clusterCoord() ClusterCoordinator {
	if w := s.wiring.Load(); w != nil {
		return w.coord
	}
	return nil
}

// edgeManager returns this pod's upstream-pull manager, or nil with
// -cluster-mode off.
func (s *Server) edgeManager() *EdgeManager {
	if w := s.wiring.Load(); w != nil {
		return w.edges
	}
	return nil
}

// HandleLeaseDeleted is the cluster informer's lease-deletion dispatch
// (cluster-wide "broadcast ended"): stop any edge pull for the broadcast,
// then expire the local hub so viewers get the terminal 4000.
func (s *Server) HandleLeaseDeleted(broadcastID string) {
	// On an edge pod the origin's kill (which deletes the Lease) can arrive
	// before this pod's own Ban event; ending the hub normally would tell
	// viewers 4000 instead of 4006. This ban-set check is only the fallback
	// (it misses IP bans and unseen ID bans) — the primary path is the edge
	// pull passing the origin's code on. It runs before the edge teardown
	// because terminate() stops the pull itself.
	if rec, banned := s.banSet().BannedID(broadcastID, s.clock()); banned {
		s.log.Debug("lease deletion for a banned broadcast: terminating instead of ending",
			"id", broadcastID, "target_type", string(rec.Target.Type))
		s.terminate(broadcastID, "lease-deleted:banned")
		return
	}
	if edges := s.edgeManager(); edges != nil {
		edges.OnLeaseDeleted(broadcastID)
	}
	s.registry.EndBroadcast(broadcastID)
}

// handlePublish claims a publisher slot, upgrades the session and feeds its
// datagrams to the publisher. With a path "id" it reclaims that slot; without
// one it upgrades first, mints an ID and announces it on a uni stream.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	if s.rejectedDraining(w, "publish") {
		return
	}
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	if s.cfg.PublishSecret != "" &&
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("secret")), []byte(s.cfg.PublishSecret)) != 1 {
		s.metrics.Connection("publish", metrics.OutcomeUnauthorized)
		s.log.Warn("publish unauthorized: invalid or missing secret",
			"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// The IP ban gates both mint and claim — an IP is the only handle that
	// spans a re-mint loop. Pre-upgrade, before any hub state is touched.
	peer := remoteIP(r.RemoteAddr)
	if rec, banned := s.banSet().BannedIP(peer, s.clock()); banned {
		s.rejectBanned(w, r, rec, "")
		return
	}

	id := r.PathValue("id")
	// The mint path starts a broadcast; every claim of an existing ID
	// continues one.
	startKind := metrics.BroadcastResumed
	if id == "" {
		startKind = metrics.BroadcastNew
	}
	var pub *hub.Publisher
	var err error
	var sess *webtransport.Session

	if id != "" {
		// Claim path: every /publish/{id} — a graced hub or an ID this pod has
		// never seen — requires the resume token minted at first publish. The
		// token prevents graced-ID hijack; the unknown-ID create below makes
		// broadcasts survive relay restarts.
		normID, err := broadcastid.Normalize(id)
		if err != nil {
			s.metrics.Connection("publish", metrics.OutcomeNotFound)
			s.log.Warn("publish claim rejected: invalid broadcast ID",
				"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"))
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The ID ban check must precede resume.verify: the token is
		// HMAC(key, broadcastID), so a killed broadcaster still holds a valid
		// one and could otherwise resurrect the broadcast on any pod.
		if rec, banned := s.banSet().BannedID(normID, s.clock()); banned {
			s.rejectBanned(w, r, rec, normID)
			return
		}
		if !s.resume.verify(normID, r.URL.Query().Get("resume")) {
			// The token value itself is never logged.
			s.metrics.Connection("publish", metrics.OutcomeUnauthorized)
			s.log.Warn("publish claim rejected: invalid or missing resume token",
				"id", normID, "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// Come-home: if this pod is edge for the ID, its upstream pull holds
		// the hub's publisher slot — stop it (and wait) so the claim below
		// succeeds. The hub and its viewers survive.
		if edges := s.edgeManager(); edges != nil {
			edges.StopEdge(normID)
		}

		// ErrPublisherActive is not a rejection: the holder may be this
		// broadcaster's silently-dead previous session, indistinguishable from
		// a live one until the idle timeout, and rejecting would push the
		// client into a mint fallback that orphans every viewer. The verified
		// token proves ownership, so newest publisher wins — but only after a
		// successful upgrade, so a malformed request can't depose a healthy one.
		id, pub, err = s.registry.ResumePublish(normID)
		if err != nil && !errors.Is(err, hub.ErrPublisherActive) {
			s.log.Warn("publish claim rejected",
				"id", normID, "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "err", err)
			if errors.Is(err, hub.ErrMaxBroadcasts) {
				s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			s.metrics.Connection("publish", metrics.OutcomeError)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Cluster mode: the verified token force-takes the origin Lease, even
		// from a live holder (re-home; the old origin demotes). The hub claim
		// is released on failure so no zombie slot survives a lost race. On
		// the takeover path (pub == nil) this is deferred to after the upgrade.
		if coord := s.clusterCoord(); pub != nil && coord != nil {
			if _, err := coord.Claim(r.Context(), id, true); err != nil {
				pub.Close()
				s.metrics.Connection("publish", metrics.OutcomeError)
				s.log.Warn("origin lease claim failed", "id", id, "err", err)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}

		sess, err = s.wt.Upgrade(w, r)
		if err != nil {
			if pub != nil {
				pub.Close() // Release on upgrade failure
			}
			s.metrics.Connection("publish", metrics.OutcomeUpgradeFailed)
			s.log.Warn("publish upgrade failed", "err", err)
			// Upgrade writes no status on failure — without an explicit one
			// the client would see an implicit 200 and believe it connected.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		s.postUpgradePublishHook(normID)

		if pub == nil {
			// Another session holds the slot: depose it now that this
			// session is real.
			id, pub, err = s.registry.TakeOverPublish(normID)
			if err != nil {
				// "Not found" is usually a GC since the claim attempt, but a ban in
				// this window also removes the hub, and must not be reported as an
				// ended broadcast. This only picks the truthful report; the re-check
				// below is what closes the window.
				if rec, banned := s.bannedPublisher(normID, peer); banned {
					s.metrics.Connection("publish", metrics.OutcomeBanned)
					s.log.Warn("publish closed: banned during the upgrade",
						"broadcast_key", s.broadcastKey(normID), "remote", r.RemoteAddr,
						"target_type", string(rec.Target.Type))
					closeWithNotice(sess, wire.CloseCodeTerminatedByOperator, terminationReason)
					return
				}
				// The broadcast was GC'd between the claim attempt and the
				// takeover.
				s.metrics.Connection("publish", metrics.OutcomeNotFound)
				s.log.Warn("publish takeover failed", "id", normID, "err", err)
				closeWithNotice(sess, wire.CloseCodeBroadcastEnded, "broadcast ended")
				return
			}
			if coord := s.clusterCoord(); coord != nil {
				if _, cerr := coord.Claim(r.Context(), id, true); cerr != nil {
					pub.Close()
					s.metrics.Connection("publish", metrics.OutcomeError)
					s.log.Warn("origin lease claim failed", "id", id, "err", cerr)
					sess.CloseWithError(webtransport.SessionErrorCode(http.StatusServiceUnavailable), "failed to claim origin lease")
					return
				}
			}
		}
	} else {
		// Mint path: reject at-capacity pre-upgrade so the browser sees a
		// clean 429; StartPublish re-checks authoritatively after the upgrade.
		if err := s.registry.CheckPublishNew(); err != nil {
			s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
			s.log.Warn("publish mint rejected",
				"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "err", err)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		// Mint path: upgrade first
		sess, err = s.wt.Upgrade(w, r)
		if err != nil {
			s.metrics.Connection("publish", metrics.OutcomeUpgradeFailed)
			s.log.Warn("publish upgrade failed", "err", err)
			w.WriteHeader(http.StatusForbidden) // never an implicit 200
			return
		}
		// No ID exists yet on this path — which is precisely the shape the
		// hook is here to reproduce.
		s.postUpgradePublishHook("")

		id, pub, err = s.registry.StartPublish("")
		if err != nil {
			s.log.Warn("failed to start publish session after upgrade", "err", err)
			if errors.Is(err, hub.ErrMaxBroadcasts) {
				s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
				sess.CloseWithError(429, "max concurrent broadcasts reached")
			} else {
				s.metrics.Connection("publish", metrics.OutcomeError)
				sess.CloseWithError(500, "failed to start publish session")
			}
			return
		}

		// Cluster mode: create the fresh ID's origin Lease — where the
		// cluster-wide MaxBroadcasts binds (the registry limit is per-pod). On
		// rejection the hub goes into grace and is GC'd; nobody knows the ID.
		if coord := s.clusterCoord(); coord != nil {
			if _, cerr := coord.Claim(r.Context(), id, false); cerr != nil {
				pub.Close()
				s.log.Warn("origin lease create failed for minted broadcast", "id", id, "err", cerr)
				if errors.Is(cerr, cluster.ErrMaxBroadcasts) {
					s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
					sess.CloseWithError(429, "max concurrent broadcasts reached")
				} else {
					s.metrics.Connection("publish", metrics.OutcomeError)
					sess.CloseWithError(500, "failed to claim origin lease")
				}
				return
			}
		}
	}
	defer pub.Close()
	defer s.trackSession(sess)()
	defer s.trackPublisher(id, sess, peer)()

	// Close the ban TOCTOU window: until trackPublisher this session was
	// invisible to HandleBanAdded, so a ban landing since the pre-upgrade
	// gate killed nothing (and the file source never re-fires). A source
	// closes the gate before actuating (moderationsrc.Sink.apply) and this
	// read follows trackPublisher, so either that kill saw this session or
	// this check sees that ban.
	if rec, banned := s.bannedPublisher(id, peer); banned {
		s.metrics.Connection("publish", metrics.OutcomeBanned)
		s.log.Warn("publish closed: banned during the upgrade",
			"broadcast_key", s.broadcastKey(id), "remote", r.RemoteAddr,
			"target_type", string(rec.Target.Type))
		s.log.Debug("publish ban detail", "id", id, "remote", r.RemoteAddr,
			"target_type", string(rec.Target.Type), "target", rec.Target.Value,
			"ban_reason", rec.Reason, "created_by", rec.CreatedBy)
		closeWithNotice(sess, wire.CloseCodeTerminatedByOperator, terminationReason)
		return
	}

	// Bind the session so a later token-bearing claim can depose it. False
	// means a takeover already won since the pre-upgrade claim.
	if !pub.BindConn(&webtransportSessionAdapter{Session: sess, closeNotice: true}) {
		s.metrics.Connection("publish", metrics.OutcomeConflict)
		s.log.Info("publisher superseded during setup", "broadcast_id", id)
		closeWithNotice(sess, wire.CloseCodePublisherSuperseded, "superseded by a new publisher session")
		return
	}

	// Bind the relay→publisher push channel for the live viewer count.
	// SendDatagram is goroutine-safe; a failed push is repaired by the
	// keepalive.
	pub.BindSend(func(b []byte) { _ = sess.SendDatagram(b) })

	// BroadcastAnnounce and ResumeToken ride separate uni streams so a
	// client that parses one stream strictly as the announce still works.
	// Arrival order is not guaranteed (docs/gotchas.md), so clients
	// dispatch server uni streams by wire type.
	announceBytes, err := wire.AppendBroadcastAnnounce(nil, id)
	if err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to build broadcast announce bytes", "err", err)
		sess.CloseWithError(500, "failed to build announce")
		return
	}
	if err := sendUniMessage(sess, announceBytes); err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to send broadcast announce", "err", err)
		sess.CloseWithError(500, "failed to send announce")
		return
	}
	// The token is what lets this publisher claim the ID again on any pod
	// (auto-resume, relay restarts). Deterministic per ID, so re-minting on
	// reclaim hands back the same token. Never logged.
	tokenBytes, err := wire.AppendResumeToken(nil, s.resume.mint(id))
	if err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to build resume token message", "err", err)
		sess.CloseWithError(500, "failed to build resume token")
		return
	}
	if err := sendUniMessage(sess, tokenBytes); err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to send resume token", "err", err)
		sess.CloseWithError(500, "failed to send resume token")
		return
	}

	s.metrics.Connection("publish", metrics.OutcomeAccepted)
	client := clientinfo.FromQuery(r.URL.Query())
	s.metrics.BroadcastStarted(startKind, client)
	pub.SetClient(client)
	log := s.log.With("remote", sess.RemoteAddr(), "route", "publish", "broadcast_id", id)

	// Tell the producer whether to emit parity and at what level.
	// Best-effort: without it the producer emits none.
	s.sendRelayCapabilities(sess, log)

	// Telemetry identity, also recorded on the hub so /statusz's
	// publisherSessionId joins the broadcaster's reports. Sent last: a
	// telemetry failure must never abort setup.
	pub.SetTelemetrySession(s.sendTelemetryHello(sess, id, wire.TelemetryRoleBroadcaster, log))
	// Where this session's telemetry should go — composes with the hello,
	// never sent on /internal/subscribe.
	s.sendTelemetryEndpoint(sess, log)

	log.Info("publisher session started")

	// Keyframes arrive on publisher-initiated uni streams, concurrently with
	// the datagram loop; the goroutine shares the request context and exits
	// with the session.
	go s.acceptKeyframeStreams(r.Context(), sess, pub, log)

	tsLimiter := newTimeSyncLimiter()
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("publisher session ended", "reason", sessionEndReason(r.Context(), err))
			return
		}
		// Any datagram at all is the page's loop running — the stall clock
		// is stamped here, before the TimeSync answer, because those pings
		// never reach the hub.
		pub.NoteSeen()
		// TimeSync is a transport-level concern (the reply needs this session
		// and the relay clock); everything else is the hub's.
		if maybeAnswerTimeSync(sess, dgram, tsLimiter) {
			continue
		}
		pub.HandleDatagram(dgram)
	}
}

// sendRelayCapabilities tells a client which optional features this fleet
// supports (docs/34 §4.4). Best-effort: without it a client emits and
// requests no parity. Nothing is sent when there is nothing to advertise.
// Growth is new bits in the flags word, never new bytes: both producer
// mirrors parse it strictly by size.
func (s *Server) sendRelayCapabilities(sess *webtransport.Session, log *slog.Logger) {
	caps := wire.RelayCapabilities{}
	if s.cfg.ParityDefault > 0 {
		caps.Flags |= wire.CapParityChunks
		caps.ParityLevel = uint8(s.cfg.ParityDefault)
	}
	if s.cfg.StripedDelivery {
		caps.Flags |= wire.CapStripedDelivery
	}
	if caps.Flags == 0 {
		return
	}
	msg, err := wire.AppendRelayCapabilities(nil, caps)
	if err != nil {
		log.Warn("relay capabilities encode failed", "err", err)
		return
	}
	if err := sendUniMessage(sess, msg); err != nil {
		log.Warn("relay capabilities send failed", "err", err)
	}
}

// sendUniMessage writes one complete wire message on a fresh server-initiated
// unidirectional stream and closes it.
func sendUniMessage(sess *webtransport.Session, msg []byte) error {
	stream, err := sess.OpenUniStream()
	if err != nil {
		return err
	}
	if _, err := stream.Write(msg); err != nil {
		stream.CancelWrite(0)
		return err
	}
	return stream.Close()
}

// TimeSync (docs/15): the relay answers client pings inline with its
// monotonic clock, giving each client an offset + RTT sample. The reply never
// rides the video queue — a delayed reply is a corrupted measurement.

// processStart anchors the relay's monotonic reference clock. Monotonic on
// purpose: an NTP step on the server must not jump every client's offset.
var processStart = time.Now()

func relayNowUs() uint64 {
	return uint64(time.Since(processStart).Microseconds())
}

// Replies are answered at most at this rate per session (clients ping every
// ~2s; more is a bug or abuse). Excess pings are dropped.
const (
	timeSyncReplyRate  = 5.0 // replies per second
	timeSyncReplyBurst = 5.0
)

// timeSyncLimiter is a tiny per-session token bucket (single-goroutine use:
// each session's read loop owns one).
type timeSyncLimiter struct {
	tokens float64
	last   time.Time
}

func newTimeSyncLimiter() *timeSyncLimiter {
	return &timeSyncLimiter{tokens: timeSyncReplyBurst, last: time.Now()}
}

func (l *timeSyncLimiter) allow() bool {
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * timeSyncReplyRate
	l.last = now
	if l.tokens > timeSyncReplyBurst {
		l.tokens = timeSyncReplyBurst
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// maybeAnswerTimeSync answers a TimeSync ping inline and reports whether the
// datagram was one (handled or dropped — either way the caller is done with
// it). Malformed pings and reply errors are ignored: the next ping retries.
func maybeAnswerTimeSync(sess *webtransport.Session, dgram []byte, lim *timeSyncLimiter) bool {
	if len(dgram) != wire.TimeSyncSize || dgram[1] != wire.TypeTimeSync {
		return false
	}
	clientUs, _, err := wire.ParseTimeSync(dgram)
	if err != nil || !lim.allow() {
		return true
	}
	_ = sess.SendDatagram(wire.AppendTimeSync(nil, clientUs, relayNowUs()))
	return true
}

// maxConcurrentKeyframeStreams bounds concurrent keyframe ingests. Normal
// operation needs ~1; the cap stops a hostile publisher opening unbounded
// streams.
const maxConcurrentKeyframeStreams = 4

// acceptKeyframeStreams reads keyframe streams from the publisher and hands
// each to the hub for ingestion + fan-out. It returns when the session's
// context is cancelled (AcceptUniStream errors).
func (s *Server) acceptKeyframeStreams(ctx context.Context, sess *webtransport.Session, pub *hub.Publisher, log *slog.Logger) {
	sem := make(chan struct{}, maxConcurrentKeyframeStreams)
	for {
		stream, err := sess.AcceptUniStream(ctx)
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			// Too many concurrent keyframe streams: reset this one rather than
			// blocking the accept loop or growing goroutines without bound.
			stream.CancelRead(0)
			log.Warn("keyframe stream rejected: too many concurrent")
			continue
		}
		go func(st *webtransport.ReceiveStream) {
			defer func() { <-sem }()
			if err := pub.IngestKeyframeStream(st); err != nil {
				st.CancelRead(0)
				log.Debug("keyframe stream ingest failed", "err", err)
			}
		}(stream)
	}
}

// handleSubscribe upgrades the session and registers it with the hub.
// ID-less requests or non-existent broadcast IDs return 404 pre-upgrade.
// Full broadcasts return 429.
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.rejectedDraining(w, "subscribe") {
		return
	}
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		s.metrics.Connection("subscribe", metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// ?stripe=N&leg=j (docs/35 §5.3) marks a stripe leg. Validated strictly
	// pre-upgrade, unlike every other parameter: a mis-striped leg can't
	// degrade to anything useful (a wrong share manufactures holes), so
	// rejection is graceful — the viewer stays unstriped. Reliable/DVR legs
	// are rejected too (striping is live-edge only). ?owner= ties a viewer's
	// sessions together: required on legs (an unowned leg is an unreapable
	// orphan); invalid elsewhere it just means an unowned session.
	ownerParam := r.URL.Query().Get("owner")
	stripeLeg, isStripeLeg, stripeErr := hub.NegotiateStripe(
		r.URL.Query().Get("stripe"), r.URL.Query().Get("leg"), ownerParam,
		s.cfg.StripedDelivery, r.URL.Query().Get("delivery") == "reliable" || r.URL.Query().Get("buffer") != "")
	if stripeErr != nil {
		s.log.Warn("stripe leg rejected pre-upgrade",
			"id", id, "remote", r.RemoteAddr, "err", stripeErr)
		s.metrics.Connection("subscribe", metrics.OutcomeError)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	err := s.registry.CheckSubscribe(id)
	if edges := s.edgeManager(); errors.Is(err, hub.ErrNotFound) && edges != nil {
		// Cluster mode: we don't host this broadcast, but its
		// origin may be another pod — demand-create the edge pull, then
		// re-check (the pull creates the local hub on attach).
		if edgeErr := edges.EnsureEdge(r.Context(), id); edgeErr == nil {
			err = s.registry.CheckSubscribe(id)
		}
	}
	if err != nil {
		s.log.Warn("subscribe rejected pre-upgrade",
			"id", id, "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "err", err)
		if errors.Is(err, hub.ErrNotFound) {
			s.metrics.Connection("subscribe", metrics.OutcomeNotFound)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if errors.Is(err, hub.ErrFull) || errors.Is(err, hub.ErrTotalSubscribers) {
			s.metrics.Connection("subscribe", metrics.OutcomeLimitRejected)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		s.metrics.Connection("subscribe", metrics.OutcomeError)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection("subscribe", metrics.OutcomeUpgradeFailed)
		s.log.Warn("subscribe upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden) // never an implicit 200
		return
	}
	defer s.trackSession(sess)()

	if hook := s.testHookPostUpgradeSubscribe.Load(); hook != nil {
		(*hook)(id)
	}

	// ?delivery=reliable opts into carrier delivery (a query param: the
	// WebTransport JS API can't set headers); ?buffer=<ms> additionally opts
	// into DVR delivery (docs/26) from the ring at this subscriber's own
	// cursor — the viewer's guaranteed minimum playout offset. A mode change
	// is a reconnect, and no value rejects: unusable ones degrade to a
	// working mode.
	reliable := r.URL.Query().Get("delivery") == "reliable"
	mode, bufferMs := hub.NegotiateDelivery(reliable, r.URL.Query().Get("buffer"), s.cfg.DVRWindow)
	// ?parity=0|1|2 opts a live-edge viewer down from the
	// fleet default. Carrier modes are served 0 regardless — their deltas ride
	// QUIC retransmission, so parity would be pure egress waste.
	parityRequested, parityServed := hub.NegotiateParity(
		r.URL.Query().Get("parity"), s.cfg.ParityDefault, mode != wire.DeliveryDatagrams)
	// A stripe leg gets no close notice: the viewer reads no streams on a
	// leg (the relay sends legs none), and a leg's death is handled by the
	// primary's fallback whatever its code.
	adapter := &webtransportSessionAdapter{Session: sess, closeNotice: !isStripeLeg}
	var sub *hub.Subscriber
	switch {
	case isStripeLeg:
		// A leg is a plain datagram subscriber with a per-leg share filter;
		// its parity prefix matches the primary's negotiation so the parity
		// share composes unchanged.
		sub, err = s.registry.SubscribeStripeLeg(id, adapter, stripeLeg, parityServed)
	case mode == wire.DeliveryDVR:
		sub, err = s.registry.SubscribeDVR(id, adapter, bufferMs)
	case mode == wire.DeliveryReliable:
		sub, err = s.registry.SubscribeReliable(id, adapter)
	default:
		owner := ""
		if hub.ValidOwnerToken(ownerParam) {
			owner = ownerParam
		}
		sub, err = s.registry.SubscribeParity(id, adapter, parityServed, owner)
	}
	if err != nil {
		s.log.Warn("subscribe rejected after upgrade", "id", id, "remote", sess.RemoteAddr(), "err", err)
		if errors.Is(err, hub.ErrNotFound) {
			// The broadcast was GC'd between the pre-upgrade check and now:
			// send the terminal code so the viewer shows "broadcast ended"
			// instead of burning its reconnect budget against a 404.
			s.metrics.Connection("subscribe", metrics.OutcomeNotFound)
			closeWithNotice(sess, wire.CloseCodeBroadcastEnded, "broadcast ended")
			return
		}
		s.metrics.Connection("subscribe", metrics.OutcomeLimitRejected)
		if errors.Is(err, hub.ErrTotalSubscribers) {
			sess.CloseWithError(webtransport.SessionErrorCode(http.StatusTooManyRequests), "total subscriber limit reached")
			return
		}
		sess.CloseWithError(webtransport.SessionErrorCode(http.StatusTooManyRequests), "subscriber limit reached")
		return
	}
	defer sub.Close()

	s.metrics.Connection("subscribe", metrics.OutcomeAccepted)
	log := s.log.With("remote", sess.RemoteAddr(), "route", "subscribe", "broadcast_id", id)
	if reliable {
		log = log.With("delivery", "reliable")
	}
	if parityRequested != parityServed || parityServed > 0 {
		log = log.With("parity_requested", parityRequested, "parity_served", parityServed)
	}
	if mode == wire.DeliveryDVR {
		log = log.With("delivery", "dvr", "buffer_ms", bufferMs)
	}
	if isStripeLeg {
		// A leg is not a viewer: no delivery ack, capabilities or telemetry
		// hello (the primary carries them). Upgrade success is its acceptance.
		log = log.With("stripe_leg", stripeLeg.Member, "stripe_n", stripeLeg.N)
		log.Info("stripe leg session started")
		legLimiter := newTimeSyncLimiter()
		for {
			dgram, err := sess.ReceiveDatagram(r.Context())
			if err != nil {
				log.Info("stripe leg session ended", "reason", sessionEndReason(r.Context(), err), "dropped", sub.Dropped())
				return
			}
			// Any inbound datagram renews the leg's liveness lease (the viewer's
			// 1 Hz StripeState refresh); only TimeSync is answered.
			sub.NoteLegAlive()
			maybeAnswerTimeSync(sess, dgram, legLimiter)
		}
	}
	// A viewer's primary session is a join, and the web app marks its
	// automatic reconnects (docs/61). Legs returned above; edge pulls use
	// their own route.
	joinKind := metrics.JoinFirst
	if r.URL.Query().Get("rejoin") == "1" {
		joinKind = metrics.JoinRejoin
	}
	delivery := deliveryLabel(mode)
	s.metrics.ViewerJoined(joinKind, delivery, clientinfo.FromQuery(r.URL.Query()))
	joinedAt := time.Now()
	defer func() { s.metrics.ViewerLeft(delivery, time.Since(joinedAt)) }()

	// Tell the viewer what it was actually served: a DVR-replayed GOP is
	// byte-identical to a live one, so otherwise it can't tell an honoured
	// request from a downgrade. Best-effort.
	ack := wire.AppendDeliveryAck(nil, mode, uint16(bufferMs))
	if err := sess.SendDatagram(ack); err != nil {
		log.Warn("delivery ack not sent; the viewer cannot report its served mode", "err", err)
	}
	// ...and re-announce it (see deliveryAckAnnouncements): there is no
	// back-channel to ask again (docs/15).
	go func() {
		t := time.NewTicker(deliveryAckReannounceEvery)
		defer t.Stop()
		for i := 1; i < deliveryAckAnnouncements; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				if err := sess.SendDatagram(ack); err != nil {
					return
				}
			}
		}
	}()
	// The viewer is told the fleet level too, so its overlay can show
	// "requested 2 / active 1" rather than leaving a refusal invisible.
	s.sendRelayCapabilities(sess, log)

	// The viewer half of the correlation ID; a failed hello means a viewer
	// that never reports, shown as unknown rather than ok.
	sub.SetTelemetrySession(s.sendTelemetryHello(sess, id, wire.TelemetryRoleViewer, log))
	// Same endpoint advertisement as the publish route.
	s.sendTelemetryEndpoint(sess, log)

	log.Info("subscriber session started")
	tsLimiter := newTimeSyncLimiter()
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("subscriber session ended", "reason", sessionEndReason(r.Context(), err), "dropped", sub.Dropped())
			return
		}
		// Subscribers send TimeSync pings and StripeState; everything else
		// is discarded.
		if s.cfg.StripedDelivery && len(dgram) == wire.StripeStateSize && dgram[1] == wire.TypeStripeState {
			if st, err := wire.ParseStripeState(dgram); err == nil {
				// ApplyStripeState is inert on reliable/DVR/leg sessions; on
				// this route sub is always external. Level state: the 1 Hz
				// refresh re-arms the TTL, a flip is counted once.
				sub.ApplyStripeState(st)
			}
			continue
		}
		maybeAnswerTimeSync(sess, dgram, tsLimiter)
	}
}

// deliveryLabel names a negotiated delivery mode for the usage labels.
func deliveryLabel(mode wire.DeliveryMode) string {
	switch mode {
	case wire.DeliveryReliable:
		return "reliable"
	case wire.DeliveryDVR:
		return "dvr"
	}
	return "datagrams"
}

// handleInternalSubscribe serves a downstream edge pod (docs/22). Rejections
// are plain HTTP statuses the Go dialer reads: 401 bad PSK, 404 not-origin
// (or cluster mode off), 409 stale generation, 426 version skew (mid-rollout;
// the edge retries). Generation fencing bounds depth at 2: a pod serves only
// while origin for exactly that originGeneration, so an edge never feeds an
// edge. No per-IP rate limit: the PSK is the gate, and under etp=Cluster
// the limiter would only see node IPs.
func (s *Server) handleInternalSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.rejectedDraining(w, "internal") {
		return
	}
	coord := s.clusterCoord()
	if coord == nil {
		s.metrics.Connection("internal", metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("psk")), []byte(s.cfg.InternalPSK)) != 1 {
		s.metrics.Connection("internal", metrics.OutcomeUnauthorized)
		s.log.Warn("internal subscribe unauthorized: bad PSK", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Query().Get("proto") != strconv.Itoa(wire.Version) {
		s.metrics.Connection("internal", metrics.OutcomeError)
		s.log.Warn("internal subscribe protocol skew",
			"remote", r.RemoteAddr, "proto", r.URL.Query().Get("proto"))
		w.WriteHeader(http.StatusUpgradeRequired)
		return
	}
	normID, err := broadcastid.Normalize(r.PathValue("id"))
	if err != nil {
		s.metrics.Connection("internal", metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	heldGen, held := coord.OriginGeneration(normID)
	if !held {
		s.metrics.Connection("internal", metrics.OutcomeNotFound)
		s.log.Warn("internal subscribe rejected: not origin", "id", normID, "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	gen, err := strconv.ParseInt(r.URL.Query().Get("gen"), 10, 64)
	if err != nil || gen != heldGen {
		s.metrics.Connection("internal", metrics.OutcomeConflict)
		s.log.Warn("internal subscribe rejected: stale generation",
			"id", normID, "remote", r.RemoteAddr, "got", r.URL.Query().Get("gen"), "held", heldGen)
		w.WriteHeader(http.StatusConflict)
		return
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection("internal", metrics.OutcomeUpgradeFailed)
		s.log.Warn("internal subscribe upgrade failed", "err", err)
		// A legible status instead of an implicit 200: the edge dialer
		// surfaces upstream statuses in its logs.
		w.WriteHeader(http.StatusForbidden)
		return
	}
	defer s.trackSession(sess)()

	// No close notice: the edge client reads every uni stream here as a
	// keyframe, and it reads close codes itself (it is Go, not Chrome).
	sub, err := s.registry.SubscribeInternal(normID, &webtransportSessionAdapter{Session: sess})
	if err != nil {
		// The hub vanished between the fence and now (GC race): 4000 tells
		// the edge the broadcast is over (its own lease watch will agree).
		s.metrics.Connection("internal", metrics.OutcomeNotFound)
		sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcastEnded), "broadcast ended")
		return
	}
	defer sub.Close()

	s.metrics.Connection("internal", metrics.OutcomeAccepted)
	log := s.log.With("remote", sess.RemoteAddr(), "route", "internal", "broadcast_id", normID)
	log.Info("edge session attached")
	tsLimiter := newTimeSyncLimiter()
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("edge session ended", "reason", sessionEndReason(r.Context(), err), "dropped", sub.Dropped())
			return
		}
		// The edge's TimeSync pings (per-hop ClockMapping rewrite) are
		// answered against THIS pod's clock, exactly like a viewer's.
		if maybeAnswerTimeSync(sess, dgram, tsLimiter) {
			continue
		}
		// The only route that accepts a client-sent ViewerCount: the peer is
		// a PSK-authenticated, generation-fenced edge. The count pump sums it
		// into the origin's global total.
		if len(dgram) == wire.ViewerCountSize && dgram[1] == wire.TypeViewerCount {
			if count, err := wire.ParseViewerCount(dgram); err == nil {
				sub.RecordDownstreamViewers(count)
			}
		}
	}
}

// isLoopbackAddr reports whether addr (an http.Request.RemoteAddr-style
// "host:port" string) resolves to a loopback IP.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isTrustedAddr reports whether addr falls in any trusted CIDR.
func isTrustedAddr(addr string, cidrs []*net.IPNet) bool {
	if len(cidrs) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, c := range cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}

// handleEcho upgrades the CONNECT request and echoes every datagram back.
// Kept permanently as a connectivity diagnostic; it also doubles as the k8s
// exec probe target, so its routine session logs are quietable — see
// QuietProbeLogs.
func (s *Server) handleEcho(w http.ResponseWriter, r *http.Request) {
	if s.rejectedDraining(w, "echo") {
		return
	}
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection("echo", metrics.OutcomeUpgradeFailed)
		s.log.Warn("echo upgrade failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer s.trackSession(sess)()
	s.metrics.Connection("echo", metrics.OutcomeAccepted)
	// k8s probes exec gawk-echo against 127.0.0.1 forever; quiet that
	// traffic while still logging real off-pod echo use.
	quiet := s.cfg.QuietProbeLogs && isLoopbackAddr(r.RemoteAddr)
	log := s.log.With("remote", sess.RemoteAddr(), "route", "echo")
	// Identity for probes (docs/40 §4.4), off the echo loop's critical
	// path — a client that grants no uni credit or never reads must not
	// be able to wedge this handler.
	go s.sendRelayIdentity(sess, log)
	if !quiet {
		log.Info("session started")
	}
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			if !quiet {
				log.Info("session ended", "reason", sessionEndReason(r.Context(), err))
			}
			return
		}
		if err := sess.SendDatagram(dgram); err != nil {
			log.Warn("echo send failed", "err", err)
			return
		}
	}
}

type webtransportSessionAdapter struct {
	*webtransport.Session
	// closeNotice: browser-facing sessions state a terminal code in-band
	// first (closenotice.go). False on /internal/subscribe, whose edge
	// client reads every uni stream as a keyframe.
	closeNotice bool
}

func (w *webtransportSessionAdapter) CloseWithError(code uint32, reason string) error {
	if w.closeNotice {
		// Async: the hub closes every viewer of a broadcast in one pass,
		// and each session's own handler keeps it alive for the settle.
		closeWithNoticeAsync(w.Session, code, reason)
		return nil
	}
	return w.Session.CloseWithError(webtransport.SessionErrorCode(code), reason)
}

// OpenKeyframeStream opens a server-initiated unidirectional stream carrying
// one keyframe to this subscriber. Non-blocking: if the peer's stream
// limit is momentarily reached it returns an error and the hub counts a drop.
func (w *webtransportSessionAdapter) OpenKeyframeStream() (hub.KeyframeStream, error) {
	s, err := w.Session.OpenUniStream()
	if err != nil {
		return nil, err
	}
	return keyframeSendStream{s}, nil
}

// OpenCarrierStream opens a server-initiated unidirectional stream used as a
// reliable delta carrier. Transport-identical to a keyframe stream —
// the viewer tells the two apart by the stream's first two bytes.
func (w *webtransportSessionAdapter) OpenCarrierStream() (hub.KeyframeStream, error) {
	return w.OpenKeyframeStream()
}

// keyframeSendStream adapts a webtransport SendStream to hub.KeyframeStream.
// Write/Close/SetWriteDeadline are promoted from the embedded stream; only
// CancelWrite needs a fixed application error code.
type keyframeSendStream struct {
	*webtransport.SendStream
}

func (k keyframeSendStream) CancelWrite() {
	k.SendStream.CancelWrite(0)
}
