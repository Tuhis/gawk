package bridge

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/voice"
)

// Routes.
const (
	// VoicePath is the browser's route on the public listener.
	VoicePath = "/voice"
)

// QUIC timers. The keepalive keeps a listen-only browser's session up while
// nobody talks (the effective idle timeout is the smaller of both ends', and
// browsers advertise ~30 s).
const (
	maxIdleTimeout  = 30 * time.Second
	keepAlivePeriod = 10 * time.Second
	linkIdleTimeout = 15 * time.Second
	linkKeepAlive   = 3 * time.Second
	drainFlushDelay = 250 * time.Millisecond
)

// Server is one bridge pod.
type Server struct {
	opts    Options
	timings Timings
	log     *slog.Logger
	metrics *metrics
	names   *nameRegistry
	now     func() time.Time

	certs     certSource
	relayRoot *x509.CertPool

	publicWT   *webtransport.Server
	internalWT *webtransport.Server
	ops        *http.Server

	publicConn, internalConn net.PacketConn
	opsLn                    net.Listener
	publicTr, internalTr     *quic.Transport

	linkTr *webtransport.Transport

	started   atomic.Bool
	draining  atomic.Bool
	drainDone atomic.Bool

	mu             sync.Mutex
	sessions       map[*voiceSession]struct{}
	activeSessions int // sessions past the ticket and the cap
	observers      map[*observer]struct{}
	tempChannels   int
	failureCounts  map[string]uint64
	lastFailure    *FailureStatus

	wg sync.WaitGroup
}

// New builds a server from opts. It loads the certificate (or makes the dev
// one) and the relay roots, so a misconfigured pod never starts.
func New(opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	s := &Server{
		opts:          opts,
		timings:       opts.Timings.withDefaults(),
		log:           opts.Logger,
		metrics:       newMetrics(opts.Version),
		names:         newNameRegistry(),
		now:           time.Now,
		sessions:      map[*voiceSession]struct{}{},
		observers:     map[*observer]struct{}{},
		failureCounts: map[string]uint64{},
	}
	switch {
	case opts.DevCert:
		hosts := []string{"localhost", "127.0.0.1", "::1"}
		if h, err := os.Hostname(); err == nil {
			hosts = append(hosts, h)
		}
		cert, sum, err := NewDevCert(hosts)
		if err != nil {
			return nil, err
		}
		s.certs = staticCert{cert: &cert}
		s.log.Warn("serving an in-memory self-signed development certificate", "sha256", sum)
	default:
		fc, err := newFileCert(opts.CertFile, opts.KeyFile, s.log)
		if err != nil {
			return nil, err
		}
		s.certs = fc
	}
	if opts.Relay.CAFile != "" {
		pool, err := loadCertPool(opts.Relay.CAFile)
		if err != nil {
			return nil, fmt.Errorf("relay CA file: %w", err)
		}
		s.relayRoot = pool
	}
	s.linkTr = &webtransport.Transport{
		TLSClientConfig: &tls.Config{ServerName: opts.Relay.ServerName, RootCAs: s.relayRoot},
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			MaxIdleTimeout:                   linkIdleTimeout,
			KeepAlivePeriod:                  linkKeepAlive,
		},
	}

	pubMux := http.NewServeMux()
	pubMux.HandleFunc("CONNECT "+VoicePath, s.handleVoice)
	s.publicWT = s.newWT(pubMux, s.checkPublicOrigin)

	intMux := http.NewServeMux()
	intMux.HandleFunc("CONNECT "+voice.ObservePath, s.handleObserve)
	s.internalWT = s.newWT(intMux, checkLinkOrigin)

	opsMux := http.NewServeMux()
	opsMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	opsMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	opsMux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{}))
	opsMux.HandleFunc("GET /statusz", s.handleStatusz)
	s.ops = &http.Server{Handler: opsMux, ReadHeaderTimeout: 5 * time.Second}
	return s, nil
}

func (s *Server) newWT(mux *http.ServeMux, origin func(*http.Request) bool) *webtransport.Server {
	wt := &webtransport.Server{
		// The WT flow-control SETTINGS Safari needs (the relay's
		// server.go explains): maximal, never binding.
		Config: &webtransport.Config{
			MaxIncomingStreams:    1 << 60,
			MaxIncomingUniStreams: 1 << 60,
			MaxIncomingData:       1 << 60,
		},
		H3: &http3.Server{
			TLSConfig:       http3.ConfigureTLSConfig(&tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return s.certs.GetCertificate(h) }}),
			Handler:         mux,
			EnableDatagrams: true,
			QUICConfig: &quic.Config{
				EnableDatagrams:                  true,
				EnableStreamResetPartialDelivery: true,
				MaxIdleTimeout:                   maxIdleTimeout,
				KeepAlivePeriod:                  keepAlivePeriod,
			},
		},
		CheckOrigin: origin,
	}
	webtransport.ConfigureHTTP3Server(wt.H3)
	return wt
}

// checkPublicOrigin is the relay's allowedOrigins rule: an empty list allows
// any Origin (development), otherwise the Origin must be listed.
func (s *Server) checkPublicOrigin(r *http.Request) bool {
	if len(s.opts.AllowedOrigins) == 0 {
		return true
	}
	origin := r.Header.Get("Origin")
	if slices.Contains(s.opts.AllowedOrigins, origin) {
		return true
	}
	s.metrics.refused.WithLabelValues("origin").Inc()
	s.log.Warn("origin rejected", "origin", origin)
	return false
}

// checkLinkOrigin accepts only the internal links' fixed origin.
func checkLinkOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == voice.LinkOrigin
}

// Listen binds the three listeners. Call before Serve; separate so tests can
// bind port 0 and read the addresses.
func (s *Server) Listen() error {
	var err error
	if s.publicConn, err = net.ListenPacket("udp", s.opts.Listen); err != nil {
		return fmt.Errorf("listen %s: %w", s.opts.Listen, err)
	}
	if s.internalConn, err = net.ListenPacket("udp", s.opts.InternalListen); err != nil {
		s.publicConn.Close()
		return fmt.Errorf("internal-listen %s: %w", s.opts.InternalListen, err)
	}
	if s.opsLn, err = net.Listen("tcp", s.opts.OpsListen); err != nil {
		s.publicConn.Close()
		s.internalConn.Close()
		return fmt.Errorf("ops-listen %s: %w", s.opts.OpsListen, err)
	}
	return nil
}

// Addrs are the bound listener addresses (after Listen).
func (s *Server) Addrs() (public, internal, ops string) {
	return s.publicConn.LocalAddr().String(), s.internalConn.LocalAddr().String(), s.opsLn.Addr().String()
}

// Ready is /readyz: true from Serve until the drain has finished. The drain
// sends its 4002s while the pod is still Ready (docs/22: kube-proxy flushes
// UDP conntrack when an endpoint goes unready, so "unready, then linger"
// would cut the sessions without their close code).
func (s *Server) Ready() bool { return s.started.Load() && !s.drainDone.Load() }

// Serve runs until ctx ends, then drains (D19) and shuts down, within
// -drain-timeout.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 3)
	serveQUIC := func(conn net.PacketConn, wt *webtransport.Server, tr **quic.Transport) error {
		t := &quic.Transport{Conn: conn}
		*tr = t
		qc := wt.H3.QUICConfig.Clone()
		ln, err := t.ListenEarly(wt.H3.TLSConfig, qc)
		if err != nil {
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				qconn, err := ln.Accept(context.Background())
				if err != nil {
					return
				}
				go func() { _ = wt.ServeQUICConn(qconn) }()
			}
		}()
		return nil
	}
	if err := serveQUIC(s.publicConn, s.publicWT, &s.publicTr); err != nil {
		return err
	}
	if err := serveQUIC(s.internalConn, s.internalWT, &s.internalTr); err != nil {
		return err
	}
	go func() {
		if err := s.ops.Serve(s.opsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	s.started.Store(true)
	pub, in, ops := s.Addrs()
	s.log.Info("listening", "public", pub, "internal", in, "ops", ops)

	var err error
	select {
	case <-ctx.Done():
	case err = <-errCh:
	}
	s.Drain()
	// Let the last close capsules leave before the transports go.
	time.Sleep(drainFlushDelay)
	s.shutdown()
	return err
}

// Drain is the D19 drain: stop accepting, then SessionClosing 4002 and close
// 4002 to every browser, close every observer link (the relay redials and
// lands on another pod) and every Mumble connection — all while /readyz
// still answers ready. Bounded by -drain-timeout.
func (s *Server) Drain() {
	if s.draining.Swap(true) {
		return
	}
	s.metrics.draining.Set(1)
	deadline := time.Now().Add(s.opts.DrainTimeout)
	s.mu.Lock()
	sessions := make([]*voiceSession, 0, len(s.sessions))
	for vs := range s.sessions {
		sessions = append(sessions, vs)
	}
	observers := make([]*observer, 0, len(s.observers))
	for ob := range s.observers {
		observers = append(observers, ob)
	}
	s.mu.Unlock()
	if n := len(sessions) + len(observers); n > 0 {
		s.log.Info("draining", "voice_sessions", len(sessions), "observers", len(observers))
	}
	var wg sync.WaitGroup
	for _, vs := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vs.end(endCause{code: codeDraining, reason: "bridge draining", left: true})
		}()
	}
	for _, ob := range observers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ob.close(codeDraining, "bridge draining")
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		s.log.Warn("drain timed out")
	}
	s.drainDone.Store(true)
}

func (s *Server) shutdown() {
	_ = s.publicWT.Close()
	_ = s.internalWT.Close()
	if s.publicTr != nil {
		_ = s.publicTr.Close()
	}
	if s.internalTr != nil {
		_ = s.internalTr.Close()
	}
	_ = s.publicConn.Close()
	_ = s.internalConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.ops.Shutdown(ctx)
	s.wg.Wait()
}

// pskOK compares a link's psk in constant time.
func (s *Server) pskOK(r *http.Request) bool {
	got := r.URL.Query().Get("psk")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.PSK)) == 1
}

func (s *Server) roomKeyOf(code string) string { return RoomKey(s.opts.StatsKey, code) }
