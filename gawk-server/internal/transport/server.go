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
	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/ops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// drainWindow bounds the SIGTERM drain (R17 W1, docs/22 Decision 2): every
// open session is closed with wire.CloseCodeServerDraining, staggered evenly
// across this window so the reconnect herd doesn't land as one spike. The
// drain runs while the pod is still Ready — its conntrack entries still point
// here, so the close frames actually reach the peers; that is why it must be
// active and immediate rather than "unready, then linger" (kube-proxy flushes
// UDP conntrack on endpoint removal at an unspecified time). A constant, not
// a knob: it must stay comfortably inside terminationGracePeriodSeconds.
const drainWindow = time.Second

// drainFlushDelay gives the last 4002 close frames time to reach their peers
// before Run tears the QUIC connections down: a CONNECTION_CLOSE racing the
// session-close capsule onto the wire would turn the clean drain signal into
// an anonymous drop (still recovered, but on the slower abrupt-error path).
const drainFlushDelay = 250 * time.Millisecond

// How many times a subscriber is told what delivery it was served, and how far
// apart (R21, docs/26 Decision 7a). The announcement rides an unreliable
// datagram and never changes, so a small bounded repeat removes a single point
// of failure for 20 bytes.
//
// The burst is deliberately tight rather than spread over seconds. What it
// defends against is a *startup* race — datagrams arriving before the viewer's
// reader is draining — which resolves in well under a second; and a datagram is
// ack-eliciting, so a repeat spread across seconds is indistinguishable from a
// keepalive and holds the QUIC connection open past -max-idle-timeout. A
// 1 s idle timeout with keepalives off is exactly what
// TestIdleSubscriberTimesOutWithoutKeepalive pins, and a 4 s version of this
// broke it. Finish long before any sane idle timeout.
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
// (R17 W3/W4, docs/22 Decision 8). Nil = single-pod mode: no claims, no
// releases, no edge pulls — behavior byte-identical to pre-R17.
type ClusterCoordinator interface {
	// Claim acquires the broadcast's origin Lease for this pod. force is set
	// when the claimant presented a valid resume token (every /publish/{id});
	// mint-path claims are create-only (never steal a live holder).
	Claim(ctx context.Context, broadcastID string, force bool) (int64, error)
	// ReleaseAll clears this pod's lease holderships (the SIGTERM drain) so
	// the broadcaster's instant reconnect can claim on another pod.
	ReleaseAll(ctx context.Context)
	// Resolve returns the broadcast's current origin (edge pull, W4).
	Resolve(ctx context.Context, broadcastID string) (cluster.Origin, error)
	// OriginGeneration reports whether this pod holds the broadcast's lease
	// and at which generation — the internal route's 404/409 fence (W4).
	OriginGeneration(broadcastID string) (int64, bool)
}

// Server wraps a webtransport.Server with the gawk routes.
type Server struct {
	cfg      config.Config
	registry *hub.Registry
	log      *slog.Logger
	wt       *webtransport.Server
	limiter  *ipRateLimiter
	// resume mints/verifies the /publish/{id} resume tokens (R17 W2).
	resume *resumeTokens
	// cluster is the origin-Lease coordinator; nil when -cluster-mode is off.
	cluster ClusterCoordinator
	// edges owns this pod's upstream pulls (R17 W4); nil when cluster is.
	edges *EdgeManager
	// metrics carries the R9 connection-outcome counters; nil is safe (all
	// methods are nil-receiver no-ops) so tests can run the server unwired.
	metrics *metrics.ServerMetrics

	// Open-session tracking for the SIGTERM drain (R17 W1): every upgraded
	// session registers here and unregisters when its handler returns.
	sessMu   sync.Mutex
	sessions map[drainSession]struct{}
	// Live publisher sessions by broadcast ID (R17 W5): the demote path
	// closes the stale one when the broadcast's Lease is force-taken.
	publishers map[string]drainSession
	draining   atomic.Bool
	// onDrain runs after the 4002s have been sent, before Run returns — the
	// seam where W3 releases this pod's broadcast Leases. Nil-safe.
	onDrain func()
	// drainSleep staggers the per-session closes; injectable so the drain
	// unit test asserts exact timing without wall-clock sleeps.
	drainSleep func(time.Duration)

	// testHookPostUpgradeSubscribe runs between the session upgrade and the
	// authoritative Subscribe, letting tests deterministically widen the
	// CheckSubscribe→Subscribe race window. Never stored in production.
	// Atomic because tests install it while handler goroutines are already
	// serving, and the in-process UDP loopback between test client and
	// server provides no happens-before edge for a plain field write.
	testHookPostUpgradeSubscribe atomic.Pointer[func(id string)]

	// testHookRateLimitLoopback disables the loopback bypass of the
	// connection rate limiter, so tests (which dial from 127.0.0.1) can
	// exercise the 429 path. Always false in production. Atomic because
	// tests set it while the server is already accepting — in-process
	// localhost UDP gives the race detector no happens-before edge between
	// that write and a handler's read (caught by -race in the PR #47 pass).
	testHookRateLimitLoopback atomic.Bool

	// testHookOriginCheckLoopback disables the loopback bypass of the
	// origin allowlist, so in-process cluster tests can run the production
	// config shape (AllowedOrigins set): the pre-0.16.2 internal-edge
	// rejection was invisible to every test precisely because of this
	// bypass (docs/22 finding 12). Always false in production; atomic for
	// the same reason as testHookRateLimitLoopback.
	testHookOriginCheckLoopback atomic.Bool
}

// rejectedDraining rejects a new CONNECT with 503 once the drain has begun:
// this pod is about to exit, so accepting a session only to 4002 it
// milliseconds later would burn a client reconnect attempt for nothing.
func (s *Server) rejectedDraining(w http.ResponseWriter, route string) bool {
	if !s.draining.Load() {
		return false
	}
	s.metrics.Connection(route, metrics.OutcomeDraining)
	w.WriteHeader(http.StatusServiceUnavailable)
	return true
}

// rateLimited reports whether a connection attempt should be rejected with
// 429. Loopback bypasses the limiter (k8s exec probes hit /echo on every
// probe cycle) unless the test hook forces it; trusted CIDRs bypass it too
// (R17 W5, docs/22 Decision 13: under MetalLB L2 + etp=Cluster the relay
// sees SNAT'd node IPs, and a rollout's reconnect herd through those must
// not burn viewers' fatal-on-first-connect budget).
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
		publishers: make(map[string]drainSession),
		drainSleep: time.Sleep,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	// Same handler as the TCP ops endpoint (single definition; see ops).
	mux.HandleFunc("GET /statusz", ops.StatuszHandler(r, log))
	mux.HandleFunc("CONNECT /echo", s.handleEcho)
	mux.HandleFunc("CONNECT /publish", s.handlePublish)
	mux.HandleFunc("CONNECT /publish/{id}", s.handlePublish)
	mux.HandleFunc("CONNECT /subscribe/{id}", s.handleSubscribe)
	// Pod-to-pod edge pull (R17 W4); 404s outright unless -cluster-mode is on.
	mux.HandleFunc("CONNECT /internal/subscribe/{id}", s.handleInternalSubscribe)

	s.wt = &webtransport.Server{
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
				// The keepalive is what keeps idle subscribers alive while the
				// broadcaster is away: PINGs reset both endpoints' idle timers,
				// and the effective idle timeout is the min of both endpoints'
				// advertised values (browsers advertise ~30s).
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
			// The startup/liveness/readiness probes exec gawk-echo against
			// 127.0.0.1 from inside this same pod; a plain WebTransport
			// dial sends no Origin header, so without this bypass every
			// real deployment (AllowedOrigins non-empty) fails its own
			// probes and crash-loops. Loopback can't be spoofed over QUIC
			// (it requires a full handshake), so this doesn't weaken the
			// check against real, off-pod clients.
			if isLoopbackAddr(r.RemoteAddr) && !s.testHookOriginCheckLoopback.Load() {
				return true
			}
			origin := r.Header.Get("Origin")
			// Pod-to-pod edge pulls announce a fixed origin no deployment
			// should have to whitelist (docs/22 finding 12: with
			// -allowed-origins set — every real deployment — W4's edge
			// dials were silently rejected). Honored only on the PSK-gated
			// internal route, so the allowlist still governs every
			// client-facing path and a wrong origin on /internal/* is
			// still rejected and logged below.
			if origin == internalEdgeOrigin && strings.HasPrefix(r.URL.Path, "/internal/") {
				return true
			}
			if slices.Contains(allowed, origin) {
				return true
			}
			// A disallowed origin is otherwise rejected silently inside
			// webtransport-go; log it so blocked cross-origin dials are
			// visible (with the offending origin + remote) rather than a
			// mystery to operators.
			s.metrics.OriginRejected()
			s.log.Warn("origin rejected", "origin", origin, "remote", r.RemoteAddr, "path", r.URL.Path)
			return false
		}
	} else {
		// Dev default: accept any origin (webtransport-go's built-in default
		// would require Origin to match Host, which breaks cross-origin dev
		// setups like a Vite app on :5173 talking to :4433).
		s.wt.CheckOrigin = func(*http.Request) bool { return true }
	}

	return s
}

// Run serves until ctx is cancelled, then drains (4002 to every open
// session, staggered ≤ drainWindow — R17 W1) and closes the server. It
// always returns a non-nil listen error, or nil after a graceful shutdown.
//
// The QUIC transport is constructed explicitly (not via wt.ListenAndServe,
// which builds its own) so the shared StatelessResetKey can be set: with the
// key, any pod receiving packets for an unknown connection ID answers with a
// stateless reset the client accepts — abrupt pod death is detected in ~1 RTT
// instead of the ~30 s idle timeout (docs/22 Decision 3).
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

// trackPublisher additionally indexes a publisher session by broadcast ID
// so the W5 demote path can close the stale one on lease loss.
func (s *Server) trackPublisher(id string, sess drainSession) func() {
	s.sessMu.Lock()
	s.publishers[id] = sess
	s.sessMu.Unlock()
	return func() {
		s.sessMu.Lock()
		if s.publishers[id] == sess {
			delete(s.publishers, id)
		}
		s.sessMu.Unlock()
	}
}

// HandleLeaseLost is the demote path (R17 W5, docs/22 Decision 11): this
// pod's Lease for the broadcast was force-taken — the broadcaster re-homed
// (NAT rebind / rollout reconnect) while our publisher session still looks
// half-alive. (a) Close that stale session (its client already abandoned it;
// only the new session drives resume, so no ping-pong). (b) Close downstream
// edge sessions with 4003 — the Go edge clients re-resolve to the new
// origin. (c) Become an edge ourselves for any still-connected local
// viewers: nobody chases viewers across pods, and depth stays ≤ 2 because
// the new origin serves us directly.
func (s *Server) HandleLeaseLost(broadcastID string, _ cluster.Origin) {
	s.sessMu.Lock()
	stale := s.publishers[broadcastID]
	delete(s.publishers, broadcastID)
	s.sessMu.Unlock()
	if stale != nil {
		s.log.Info("origin lease lost: closing stale publisher session", "broadcast_id", broadcastID)
		_ = stale.CloseWithError(0, "origin moved")
	}

	s.registry.CloseInternalSubscribers(broadcastID, uint32(wire.CloseCodeOriginMoved), "origin moved")

	if s.edges != nil && s.registry.ExternalSubscribers(broadcastID) > 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), edgeAttachTimeout)
			defer cancel()
			if err := s.edges.EnsureEdge(ctx, broadcastID); err != nil {
				s.log.Warn("self-demote to edge failed", "broadcast_id", broadcastID, "err", err)
			} else {
				s.log.Info("demoted to edge after lease loss", "broadcast_id", broadcastID)
			}
		}()
	}
}

// drain implements the close-first-while-Ready shutdown (docs/22 Decision 2):
// flip readiness, send wire.CloseCodeServerDraining to every open session
// staggered evenly over drainWindow, then run the onDrain hook (W3: release
// this pod's broadcast Leases). New CONNECTs are rejected 503 once draining.
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

// Ready reports whether the server is accepting new work — false once the
// drain has begun. Served as /readyz on the ops endpoint; per docs/22
// Decision 2 this is scale-down/HPA hygiene, not the rollout-correctness
// mechanism (that is the active drain above).
func (s *Server) Ready() bool {
	return !s.draining.Load()
}

// SetCluster wires the origin-Lease coordinator (R17 W3/W4): /publish claims
// acquire the broadcast's Lease, the SIGTERM drain releases this pod's
// holderships right after the 4002s go out — the broadcaster's instant
// reconnect then claims an empty-holder lease on a ready pod, no force or
// TTL wait needed — and viewers landing here for broadcasts we don't host
// trigger an edge pull from the lease's origin pod. podName is this pod's
// identity (the self-dial guard). Call before Run.
func (s *Server) SetCluster(c ClusterCoordinator, podName string) {
	s.cluster = c
	s.edges = newEdgeManager(s.registry, c,
		newEdgeDialer(s.cfg.InternalServerName, s.cfg.InternalPSK, nil, s.log),
		podName, s.log)
	s.onDrain = func() {
		s.edges.Stop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.ReleaseAll(ctx)
	}
}

// HandleLeaseDeleted is the cluster informer's lease-deletion dispatch
// (cluster-wide "broadcast ended"): stop any edge pull for the broadcast,
// then expire the local hub so viewers get the terminal 4000.
func (s *Server) HandleLeaseDeleted(broadcastID string) {
	if s.edges != nil {
		s.edges.OnLeaseDeleted(broadcastID)
	}
	s.registry.EndBroadcast(broadcastID)
}

// handlePublish claims a publisher slot, upgrades the session and feeds received
// datagrams to the publisher. If the path value "id" is present, it attempts to
// reclaim the slot. If "id" is empty, it upgrades first, then mints a new ID,
// starts publishing, and announces the ID to the publisher over a unidirectional stream.
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

	id := r.PathValue("id")
	var pub *hub.Publisher
	var err error
	var sess *webtransport.Session

	if id != "" {
		// Claim path (R17 W2): every /publish/{id} — reclaim of a graced hub
		// AND claim of an ID this pod has never seen — requires the resume
		// token minted at first publish. The token gate is what closes the
		// graced-ID hijack, and the unknown-ID create below is what makes
		// broadcasts survive relay restarts. Pre-upgrade checks.
		normID, err := broadcastid.Normalize(id)
		if err != nil {
			s.metrics.Connection("publish", metrics.OutcomeNotFound)
			s.log.Warn("publish claim rejected: invalid broadcast ID",
				"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"))
			w.WriteHeader(http.StatusNotFound)
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
		// W5 come-home: if this pod is currently EDGE for the ID, its
		// upstream pull holds the hub's publisher slot — stop it (and wait)
		// so the broadcaster's claim below succeeds instead of 409ing
		// against our own plumbing. The hub and its viewers survive; the
		// role flips back to origin on the claim.
		if s.edges != nil {
			s.edges.StopEdge(normID)
		}

		// ErrPublisherActive is NOT a rejection (docs/06 revision
		// 2026-07-18): the slot holder may be this same broadcaster's
		// silently-dead previous session, which the relay cannot tell from a
		// live one until the QUIC idle timeout fires — and a 409 here forced
		// clients into a mint fallback that orphaned every viewer. The
		// verified resume token is proof of ownership, so newest publisher
		// wins — the same-pod counterpart of the lease force-take below —
		// but the depose happens only after a successful upgrade, so a
		// malformed request can never take down a healthy publisher.
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

		// Cluster mode (R17 W3): the verified resume token is the proof of
		// ownership that force-takes the origin Lease — even from a live
		// holder (NAT rebind / re-home; the old origin's watch fires and it
		// demotes, W5). The hub claim above is released on failure so no
		// zombie slot survives a lost lease race. On the takeover path
		// (pub == nil) this is deferred to after the upgrade, beside the
		// local depose.
		if pub != nil && s.cluster != nil {
			if _, err := s.cluster.Claim(r.Context(), id, true); err != nil {
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
			// Upgrade never writes on failure (checked v0.11.1) — without
			// an explicit status the client would see an implicit 200 and
			// believe it connected (docs/22 finding 12).
			w.WriteHeader(http.StatusForbidden)
			return
		}

		if pub == nil {
			// Another session holds the slot: depose it now that this
			// session is real (docs/06 revision 2026-07-18).
			id, pub, err = s.registry.TakeOverPublish(normID)
			if err != nil {
				// The broadcast was GC'd between the claim attempt and the
				// takeover.
				s.metrics.Connection("publish", metrics.OutcomeNotFound)
				s.log.Warn("publish takeover failed", "id", normID, "err", err)
				sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcastEnded), "broadcast ended")
				return
			}
			if s.cluster != nil {
				if _, cerr := s.cluster.Claim(r.Context(), id, true); cerr != nil {
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
		// clean HTTP 429 (a failed dial), mirroring the subscribe path;
		// StartPublish below re-checks authoritatively after the upgrade.
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
			w.WriteHeader(http.StatusForbidden) // no implicit 200 (finding 12)
			return
		}

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

		// Cluster mode (R17 W3): a freshly minted ID gets its origin Lease
		// created here — also where the CLUSTER-wide MaxBroadcasts binds
		// (docs/22 Decision 13; the local registry limit above is per-pod).
		// On rejection the fresh hub is released into grace and GC'd; nobody
		// ever learned this ID, so nothing can reach it meanwhile.
		if s.cluster != nil {
			if _, cerr := s.cluster.Claim(r.Context(), id, false); cerr != nil {
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
	defer s.trackPublisher(id, sess)()

	// Bind the session so a later token-bearing claim can depose this
	// publisher. A false return means a takeover already won while this
	// session was between its pre-upgrade claim and here — end it rather
	// than publish into a deposed broadcast.
	if !pub.BindConn(&webtransportSessionAdapter{sess}) {
		s.metrics.Connection("publish", metrics.OutcomeConflict)
		s.log.Info("publisher superseded during setup", "broadcast_id", id)
		sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodePublisherSuperseded), "superseded by a new publisher session")
		return
	}

	// R18 (docs/23 Decision 4): bind the relay→publisher push channel so the
	// count pump can deliver the live viewer count. SendDatagram is
	// goroutine-safe, so the pump writing concurrently with this read loop's
	// TimeSync replies is fine; a failed push is repaired by the keepalive.
	pub.BindSend(func(b []byte) { _ = sess.SendDatagram(b) })

	// Send BroadcastAnnounce, then the ResumeToken (R17 W2), each on its own
	// server-initiated uni stream. Separate streams keep old *browser*
	// clients working across a version skew: they read one stream, parse it
	// strictly as the announce, and leave the token stream unread. That
	// story leans on the client seeing the announce stream first, which is
	// NOT a transport guarantee — webtransport-go accepts incoming streams
	// in nondeterministic order (docs/22 finding 9: the native engine saw
	// the token first in ~half of dials), so every current client dispatches
	// server uni streams by wire type instead of trusting arrival order.
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
	log := s.log.With("remote", sess.RemoteAddr(), "route", "publish", "broadcast_id", id)

	// R29 (docs/34 §4.4): tell the producer what this fleet supports, so it
	// knows whether to emit parity and at what level. Best-effort like the
	// telemetry hello below — a producer that never receives it emits no
	// parity, which is exactly the pre-R29 behaviour, so a failure here
	// degrades the stream's loss resilience but never costs the broadcast.
	s.sendRelayCapabilities(sess, log)

	// R28 TM1: hand this session its telemetry identity and record the same
	// handle on the hub, so /statusz's publisherSessionId joins the
	// broadcaster's own reports. Sent last of the three uni messages — a
	// telemetry failure must never cost a broadcast, so it is the one that
	// cannot abort setup.
	pub.SetTelemetrySession(s.sendTelemetryHello(sess, id, wire.TelemetryRoleBroadcaster, log))

	log.Info("publisher session started")

	// Keyframes arrive on publisher-initiated unidirectional streams (R8),
	// concurrently with the delta datagram loop. This goroutine shares the
	// request context, so it exits when the session ends alongside the loop
	// below — no separate shutdown signal.
	go s.acceptKeyframeStreams(r.Context(), sess, pub, log)

	tsLimiter := newTimeSyncLimiter()
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("publisher session ended", "reason", sessionEndReason(r.Context(), err))
			return
		}
		// TimeSync is a transport-level concern (the reply needs this session
		// and the relay clock); everything else is the hub's.
		if maybeAnswerTimeSync(sess, dgram, tsLimiter) {
			continue
		}
		pub.HandleDatagram(dgram)
	}
}

// sendRelayCapabilities tells a client which optional features this fleet
// supports (R29, docs/34 §4.4). Best-effort by design: a client that never
// receives it behaves exactly as it did before R29 — the producer emits no
// parity and the viewer requests none — so a failure degrades resilience
// without breaking anything.
//
// Nothing is sent at all when there is no capability to advertise — parity
// level 0 AND striping disabled — which is what keeps that configuration
// byte-identical to a relay predating both features (docs/34 §4.4, docs/35
// §5.3). Capability GROWTH is new bits in the flags word, never new bytes:
// the message is parsed strictly by size on both producer mirrors.
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

// TimeSync (R5 Q2, docs/15): clients ping over their existing session and the
// relay answers inline with its monotonic clock, giving each client an
// NTP-style offset + RTT sample against the relay as the common reference.
// The reply must never ride the per-subscriber video queue — a delayed reply
// is a corrupted measurement — so it is sent directly from the read loop.

// processStart anchors the relay's monotonic reference clock. Monotonic on
// purpose: an NTP step on the server must not jump every client's offset.
var processStart = time.Now()

func relayNowUs() uint64 {
	return uint64(time.Since(processStart).Microseconds())
}

// Replies are cheap (18 bytes) but answered at most at this rate per session —
// a constant, not a knob: clients ping every ~2s, so anything past this is a
// bug or abuse. Excess pings are silently dropped.
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

// maxConcurrentKeyframeStreams bounds how many publisher-initiated keyframe
// streams the server ingests at once. Normal operation needs ~1 (keyframes are
// ~2/s and each ingest is fast); the cap stops a hostile publisher from opening
// unbounded streams.
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

	// R30 (docs/35 §5.3): ?stripe=N&leg=j marks this dial as one stripe leg
	// of a striping viewer. Validated pre-upgrade and STRICTLY — unlike every
	// other subscribe parameter, a mis-striped leg cannot degrade to anything
	// useful (serving it a wrong share would manufacture holes), so rejection
	// is the graceful outcome: the viewer stays unstriped. Reliable/DVR
	// combinations are rejected here too (striping is live-edge only, §3).
	//
	// §14 (owner decision 2026-08-13): ?owner= is the viewer-minted token
	// tying one viewer's sessions together, REQUIRED on legs (an unowned leg
	// is an unreapable orphan waiting to happen) and optional elsewhere — an
	// invalid token on a non-leg dial degrades to an unowned session that
	// simply cannot stripe, never to a rejection.
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
	if errors.Is(err, hub.ErrNotFound) && s.edges != nil {
		// Cluster mode (R17 W4): we don't host this broadcast, but its
		// origin may be another pod — demand-create the edge pull, then
		// re-check (the pull creates the local hub on attach).
		if edgeErr := s.edges.EnsureEdge(r.Context(), id); edgeErr == nil {
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
		w.WriteHeader(http.StatusForbidden) // no implicit 200 (finding 12)
		return
	}
	defer s.trackSession(sess)()

	if hook := s.testHookPostUpgradeSubscribe.Load(); hook != nil {
		(*hook)(id)
	}

	// R19 (docs/24 Decision 6): ?delivery=reliable opts this subscriber into
	// carrier delivery — the query param is the negotiation surface because
	// the WebTransport JS API can't set headers (publish-secret precedent).
	// Unknown values fall back to datagram delivery; a mode change is a
	// reconnect, never an in-session morph.
	// R21 (docs/26 Decision 7): ?buffer=<ms> additionally opts into DVR
	// delivery, served from the broadcast's ring at this subscriber's own
	// cursor. The value is the viewer's guaranteed MINIMUM playout offset, not
	// its current one. No value can reject the session — every unusable one
	// degrades to a working mode.
	reliable := r.URL.Query().Get("delivery") == "reliable"
	mode, bufferMs := hub.NegotiateDelivery(reliable, r.URL.Query().Get("buffer"), s.cfg.DVRWindow)
	// R29 (docs/34 §5.1): ?parity=0|1|2 opts a live-edge viewer down from the
	// fleet default. Carrier modes are served 0 regardless — their deltas ride
	// QUIC retransmission, so parity would be pure egress waste.
	parityRequested, parityServed := hub.NegotiateParity(
		r.URL.Query().Get("parity"), s.cfg.ParityDefault, mode != wire.DeliveryDatagrams)
	adapter := &webtransportSessionAdapter{sess}
	var sub *hub.Subscriber
	switch {
	case isStripeLeg:
		// A leg is a plain datagram subscriber with a per-leg share filter;
		// its parity prefix matches the primary's negotiation so the R29
		// share composes unchanged (docs/35 §5.2).
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
			sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcastEnded), "broadcast ended")
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
		// A leg is not a viewer (docs/35 §5.1): no delivery ack, no
		// capabilities, no telemetry hello — the viewer's primary session
		// carries all three. Upgrade success IS the leg's acceptance signal;
		// its share starts flowing with the next delta frame.
		log = log.With("stripe_leg", stripeLeg.Member, "stripe_n", stripeLeg.N)
		log.Info("stripe leg session started")
		legLimiter := newTimeSyncLimiter()
		for {
			dgram, err := sess.ReceiveDatagram(r.Context())
			if err != nil {
				log.Info("stripe leg session ended", "reason", sessionEndReason(r.Context(), err), "dropped", sub.Dropped())
				return
			}
			// Any inbound datagram renews the leg's liveness lease (docs/35
			// §14 Decision 5) — the viewer heartbeats with its 1 Hz
			// StripeState refresh. TimeSync is answered for symmetry with
			// every other route, everything else is discarded.
			sub.NoteLegAlive()
			maybeAnswerTimeSync(sess, dgram, legLimiter)
		}
	}
	// R21 (docs/26 Decision 7a): tell the viewer what it was ACTUALLY served.
	// A DVR-replayed GOP is byte-identical to a live one, so without this the
	// viewer cannot tell an honoured request from a downgrade, or from a relay
	// too old to know the parameter — and a viewer that cannot name what it
	// got is what made the 2026-07-22 investigation so expensive (BUGS.md).
	// Best-effort: a failed ack costs a diagnostics row, never the session.
	ack := wire.AppendDeliveryAck(nil, mode, uint16(bufferMs))
	if err := sess.SendDatagram(ack); err != nil {
		log.Warn("delivery ack not sent; the viewer cannot report its served mode", "err", err)
	}
	// ...and re-announce it a few times. This one datagram is the viewer's
	// only way to name what it was served, and it was sent exactly once, at
	// the instant the CONNECT was accepted — the moment a viewer is least
	// likely to be draining its datagram queue. Unreliable delivery plus a
	// one-shot announcement means a single loss leaves the viewer reporting
	// the wrong mode for the whole session, with no way to ask again (the
	// one-way data flow is deliberate — docs/15 Decision 6). Re-emitting is
	// the same answer the cached DecoderConfig and the 1 Hz audio config
	// already use, bounded here because unlike those this never changes.
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
	// R29: the viewer is told the fleet level too, so its overlay can show
	// "requested 2 / active 1" rather than leaving a refusal invisible.
	s.sendRelayCapabilities(sess, log)

	// R28 TM1: the viewer half of the correlation ID. Best-effort, and after
	// the delivery ack — a subscriber whose hello fails still watches, it just
	// never reports, which the dashboard shows as unknown rather than ok.
	sub.SetTelemetrySession(s.sendTelemetryHello(sess, id, wire.TelemetryRoleViewer, log))

	log.Info("subscriber session started")
	tsLimiter := newTimeSyncLimiter()
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("subscriber session ended", "reason", sessionEndReason(r.Context(), err), "dropped", sub.Dropped())
			return
		}
		// Subscribers send TimeSync pings (R5 Q2) and — R30 — StripeState.
		// Everything else keeps being discarded, as before.
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

// handleInternalSubscribe serves a downstream EDGE pod pulling this
// broadcast (R17 W4, docs/22 Decision 9). Rejections are plain HTTP statuses
// — the dialer is Go and can read them (browsers never dial this route):
// 401 bad PSK, 404 not-origin (or cluster mode off), 409 stale generation,
// 426 protocol version skew (pods of version N and N+1 coexist mid-rollout;
// the edge retries until the rollout completes). Generation fencing is
// guard 2 against cascade loops: a pod serves the route only while it
// believes it is origin for exactly that originGeneration — an edge can
// never feed another edge, bounding depth at 2 structurally.
//
// No per-IP rate limiting here: the PSK is the gate, and under etp=Cluster
// SNAT the limiter would see node IPs anyway (docs/22 Decision 13).
func (s *Server) handleInternalSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.rejectedDraining(w, "internal") {
		return
	}
	if s.cluster == nil {
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
	heldGen, held := s.cluster.OriginGeneration(normID)
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
		// surfaces upstream statuses in its logs (finding 12).
		w.WriteHeader(http.StatusForbidden)
		return
	}
	defer s.trackSession(sess)()

	sub, err := s.registry.SubscribeInternal(normID, &webtransportSessionAdapter{sess})
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
		// R18 (docs/23 Decisions 5b/6): the edge's viewer-count report — the
		// ONLY route where a client-sent ViewerCount is accepted, because the
		// peer here is a PSK-authenticated, generation-fenced edge. Stored on
		// this edge's subscriber; the count pump sums it into the origin's
		// global total.
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

// isTrustedAddr reports whether addr falls in any trusted CIDR (R17 W5).
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
	// The k8s startup/liveness/readiness probes exec gawk-echo against
	// 127.0.0.1 on a tight period, forever; quiet that specific traffic
	// while still logging real (off-pod) echo diagnostic use normally.
	quiet := s.cfg.QuietProbeLogs && isLoopbackAddr(r.RemoteAddr)
	log := s.log.With("remote", sess.RemoteAddr(), "route", "echo")
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
}

func (w *webtransportSessionAdapter) CloseWithError(code uint32, reason string) error {
	return w.Session.CloseWithError(webtransport.SessionErrorCode(code), reason)
}

// OpenKeyframeStream opens a server-initiated unidirectional stream carrying
// one keyframe to this subscriber (R8). Non-blocking: if the peer's stream
// limit is momentarily reached it returns an error and the hub counts a drop.
func (w *webtransportSessionAdapter) OpenKeyframeStream() (hub.KeyframeStream, error) {
	s, err := w.Session.OpenUniStream()
	if err != nil {
		return nil, err
	}
	return keyframeSendStream{s}, nil
}

// OpenCarrierStream opens a server-initiated unidirectional stream used as a
// reliable delta carrier (R19). Transport-identical to a keyframe stream —
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
