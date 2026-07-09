// Package transport owns the HTTP/3 + WebTransport endpoint: routes,
// session acceptance and server lifecycle. It contains no media logic —
// datagrams are handed to the hub (from milestone B onwards) as opaque
// bytes.
package transport

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
)

// Server wraps a webtransport.Server with the gawk routes.
type Server struct {
	cfg config.Config
	log *slog.Logger
	wt  *webtransport.Server
}

// New builds the server. getCert supplies the TLS certificate per handshake
// (a tlsutil.Reloader in production, a fixed dev cert locally).
func New(cfg config.Config, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), log *slog.Logger) *Server {
	s := &Server{cfg: cfg, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("CONNECT /echo", s.handleEcho)

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
				MaxIdleTimeout:  30 * time.Second,
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
			return slices.Contains(allowed, r.Header.Get("Origin"))
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
	select {
	case <-ctx.Done():
		s.wt.Close()
		<-errCh // wait for ListenAndServe to return
		return nil
	case err := <-errCh:
		return err
	}
}

// handleEcho upgrades the CONNECT request and echoes every datagram back.
// Kept permanently as a connectivity diagnostic.
func (s *Server) handleEcho(w http.ResponseWriter, r *http.Request) {
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.log.Warn("echo upgrade failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log := s.log.With("remote", sess.RemoteAddr(), "route", "echo")
	log.Info("session started")
	for {
		dgram, err := sess.ReceiveDatagram(r.Context())
		if err != nil {
			log.Info("session ended", "reason", err)
			return
		}
		if err := sess.SendDatagram(dgram); err != nil {
			log.Warn("echo send failed", "err", err)
			return
		}
	}
}
