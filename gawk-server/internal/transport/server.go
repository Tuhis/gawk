// Package transport owns the HTTP/3 + WebTransport endpoint: routes,
// session acceptance and server lifecycle. It contains no media logic —
// datagrams are handed to the hub as opaque bytes.
package transport

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/wire"
)

// Server wraps a webtransport.Server with the gawk routes.
type Server struct {
	cfg      config.Config
	registry *hub.Registry
	log      *slog.Logger
	wt       *webtransport.Server
	limiter  *ipRateLimiter

	// testHookPostUpgradeSubscribe runs between the session upgrade and the
	// authoritative Subscribe, letting tests deterministically widen the
	// CheckSubscribe→Subscribe race window. Always nil in production.
	testHookPostUpgradeSubscribe func(id string)

	// testHookRateLimitLoopback disables the loopback bypass of the
	// connection rate limiter, so tests (which dial from 127.0.0.1) can
	// exercise the 429 path. Always false in production.
	testHookRateLimitLoopback bool
}

// rateLimited reports whether a connection attempt should be rejected with
// 429. Loopback bypasses the limiter (k8s exec probes hit /echo on every
// probe cycle) unless the test hook forces it.
func (s *Server) rateLimited(r *http.Request) bool {
	if s.limiter == nil {
		return false
	}
	if isLoopbackAddr(r.RemoteAddr) && !s.testHookRateLimitLoopback {
		return false
	}
	if s.limiter.Allow(r.RemoteAddr) {
		return false
	}
	s.log.Warn("connection rate limited",
		"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "path", r.URL.Path)
	return true
}

// New builds the server. getCert supplies the TLS certificate per handshake
// (a tlsutil.Reloader in production, a fixed dev cert locally).
func New(cfg config.Config, r *hub.Registry, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), log *slog.Logger) *Server {
	var limiter *ipRateLimiter
	if cfg.ConnRateLimit > 0 {
		limiter = newIPRateLimiter(cfg.ConnRateLimit, cfg.ConnBurstLimit)
	}
	s := &Server{cfg: cfg, registry: r, log: log, limiter: limiter}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /statusz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.registry.Stats()); err != nil {
			log.Warn("statusz encode failed", "err", err)
		}
	})
	mux.HandleFunc("CONNECT /echo", s.handleEcho)
	mux.HandleFunc("CONNECT /publish", s.handlePublish)
	mux.HandleFunc("CONNECT /publish/{id}", s.handlePublish)
	mux.HandleFunc("CONNECT /subscribe/{id}", s.handleSubscribe)

	s.wt = &webtransport.Server{
		H3: &http3.Server{
			Addr: cfg.Addr,
			// ConfigureTLSConfig adds the h3 ALPN; webtransport-go passes
			// this config to quic.ListenEarly as-is, so it must be set here.
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{GetCertificate: getCert}),
			Handler:         mux,
			EnableDatagrams: true,
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
			if isLoopbackAddr(r.RemoteAddr) {
				return true
			}
			origin := r.Header.Get("Origin")
			if slices.Contains(allowed, origin) {
				return true
			}
			// A disallowed origin is otherwise rejected silently inside
			// webtransport-go; log it so blocked cross-origin dials are
			// visible (with the offending origin + remote) rather than a
			// mystery to operators.
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

// Run serves until ctx is cancelled, then closes the server. It always
// returns a non-nil listen error, or nil after a graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.wt.ListenAndServe() }()

	s.log.Info("listening", "addr", s.cfg.Addr)
	defer func() {
		if s.limiter != nil {
			s.limiter.Close()
		}
	}()
	select {
	case <-ctx.Done():
		s.wt.Close()
		<-errCh // wait for ListenAndServe to return
		return nil
	case err := <-errCh:
		return err
	}
}

// handlePublish claims a publisher slot, upgrades the session and feeds received
// datagrams to the publisher. If the path value "id" is present, it attempts to
// reclaim the slot. If "id" is empty, it upgrades first, then mints a new ID,
// starts publishing, and announces the ID to the publisher over a unidirectional stream.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	if s.cfg.PublishSecret != "" &&
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("secret")), []byte(s.cfg.PublishSecret)) != 1 {
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
		// Reclaim path: pre-upgrade checks
		id, pub, err = s.registry.StartPublish(id)
		if err != nil {
			s.log.Warn("publish reclaim rejected",
				"id", id, "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "err", err)
			if errors.Is(err, hub.ErrNotFound) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if errors.Is(err, hub.ErrPublisherActive) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		sess, err = s.wt.Upgrade(w, r)
		if err != nil {
			pub.Close() // Release on upgrade failure
			s.log.Warn("publish upgrade failed", "err", err)
			return
		}
	} else {
		// Mint path: reject at-capacity pre-upgrade so the browser sees a
		// clean HTTP 429 (a failed dial), mirroring the subscribe path;
		// StartPublish below re-checks authoritatively after the upgrade.
		if err := s.registry.CheckPublishNew(); err != nil {
			s.log.Warn("publish mint rejected",
				"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "err", err)
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		// Mint path: upgrade first
		sess, err = s.wt.Upgrade(w, r)
		if err != nil {
			s.log.Warn("publish upgrade failed", "err", err)
			return
		}

		id, pub, err = s.registry.StartPublish("")
		if err != nil {
			s.log.Warn("failed to start publish session after upgrade", "err", err)
			if errors.Is(err, hub.ErrMaxBroadcasts) {
				sess.CloseWithError(429, "max concurrent broadcasts reached")
			} else {
				sess.CloseWithError(500, "failed to start publish session")
			}
			return
		}
	}
	defer pub.Close()

	// Send BroadcastAnnounce on a server-initiated uni stream
	stream, err := sess.OpenUniStream()
	if err != nil {
		s.log.Warn("failed to open uni stream for broadcast announce", "err", err)
		sess.CloseWithError(500, "failed to open announce stream")
		return
	}
	announceBytes, err := wire.AppendBroadcastAnnounce(nil, id)
	if err != nil {
		s.log.Warn("failed to build broadcast announce bytes", "err", err)
		sess.CloseWithError(500, "failed to build announce")
		return
	}
	_, err = stream.Write(announceBytes)
	if err != nil {
		s.log.Warn("failed to write broadcast announce to uni stream", "err", err)
		sess.CloseWithError(500, "failed to write announce")
		return
	}
	err = stream.Close()
	if err != nil {
		s.log.Warn("failed to close uni stream for broadcast announce", "err", err)
		sess.CloseWithError(500, "failed to close announce stream")
		return
	}

	log := s.log.With("remote", sess.RemoteAddr(), "route", "publish", "broadcast_id", id)
	log.Info("publisher session started")

	// Keyframes arrive on publisher-initiated unidirectional streams (R8),
	// concurrently with the delta datagram loop. This goroutine shares the
	// request context, so it exits when the session ends alongside the loop
	// below — no separate shutdown signal.
	go s.acceptKeyframeStreams(r.Context(), sess, pub, log)

	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("publisher session ended", "reason", err)
			return
		}
		pub.HandleDatagram(dgram)
	}
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
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if err := s.registry.CheckSubscribe(id); err != nil {
		s.log.Warn("subscribe rejected pre-upgrade",
			"id", id, "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "err", err)
		if errors.Is(err, hub.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if errors.Is(err, hub.ErrFull) || errors.Is(err, hub.ErrTotalSubscribers) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.log.Warn("subscribe upgrade failed", "err", err)
		return
	}

	if s.testHookPostUpgradeSubscribe != nil {
		s.testHookPostUpgradeSubscribe(id)
	}

	sub, err := s.registry.Subscribe(id, &webtransportSessionAdapter{sess})
	if err != nil {
		s.log.Warn("subscribe rejected after upgrade", "id", id, "remote", sess.RemoteAddr(), "err", err)
		if errors.Is(err, hub.ErrNotFound) {
			// The broadcast was GC'd between the pre-upgrade check and now:
			// send the terminal code so the viewer shows "broadcast ended"
			// instead of burning its reconnect budget against a 404.
			sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeBroadcastEnded), "broadcast ended")
			return
		}
		if errors.Is(err, hub.ErrTotalSubscribers) {
			sess.CloseWithError(webtransport.SessionErrorCode(http.StatusTooManyRequests), "total subscriber limit reached")
			return
		}
		sess.CloseWithError(webtransport.SessionErrorCode(http.StatusTooManyRequests), "subscriber limit reached")
		return
	}
	defer sub.Close()

	log := s.log.With("remote", sess.RemoteAddr(), "route", "subscribe", "broadcast_id", id)
	log.Info("subscriber session started")
	for {
		if _, err := sess.ReceiveDatagram(r.Context()); err != nil {
			log.Info("subscriber session ended", "reason", err, "dropped", sub.Dropped())
			return
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

// handleEcho upgrades the CONNECT request and echoes every datagram back.
// Kept permanently as a connectivity diagnostic; it also doubles as the k8s
// exec probe target, so its routine session logs are quietable — see
// QuietProbeLogs.
func (s *Server) handleEcho(w http.ResponseWriter, r *http.Request) {
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.log.Warn("echo upgrade failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
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
				log.Info("session ended", "reason", err)
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

// keyframeSendStream adapts a webtransport SendStream to hub.KeyframeStream.
// Write/Close/SetWriteDeadline are promoted from the embedded stream; only
// CancelWrite needs a fixed application error code.
type keyframeSendStream struct {
	*webtransport.SendStream
}

func (k keyframeSendStream) CancelWrite() {
	k.SendStream.CancelWrite(0)
}
