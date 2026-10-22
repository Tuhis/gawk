package transport

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/netip"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// PopHome is the home side of R74 PoP links (docs/76 §6.3) as the /pop/*
// routes call it: *popsrv.Manager. An interface so transport never imports
// popsrv (popsrv's test harness builds on transport).
type PopHome interface {
	// Authenticate checks a /pop/* request's Bearer credential: the Pop, or
	// the status to refuse with (401 / 403 / 429).
	Authenticate(r *http.Request) (*pops.Pop, int, error)
	// CheckPairing validates a pairing dial before the upgrade (404 / 410
	// / 403 / 429).
	CheckPairing(r *http.Request) (*pops.Pop, int, error)
	// RedeemPairing redeems the string after the upgrade and returns the
	// Pop as written and the new credential.
	RedeemPairing(ctx context.Context, p *pops.Pop) (*pops.Pop, string, int, error)
	// ServeLink runs the link until it ends; issued is the credential to
	// deliver first ("" on an ordinary link).
	ServeLink(ctx context.Context, p *pops.Pop, sess poplink.Session, issued string) error
	// RecordAddr records the address a /pop/addr dial came from.
	RecordAddr(pop string, remote netip.Addr)
}

type popHomeBox struct{ h PopHome }

// SetPopHome installs the PoP link manager (serve's wirePops, with -pops
// on). Until then, and with -pops off, every /pop/* route answers 404.
// Safe while serving.
func (s *Server) SetPopHome(h PopHome) {
	if h == nil {
		s.popHome.Store(nil)
		return
	}
	s.popHome.Store(&popHomeBox{h: h})
}

func (s *Server) popHomeOrNil() PopHome {
	if b := s.popHome.Load(); b != nil {
		return b.h
	}
	return nil
}

// registerPopRoutes adds the R74 home-side /pop/* routes (docs/76 §9) to
// the public mux. Called from New only with -pops on, so with it off not
// even a 404 handler of ours answers there.
//
// None of these handlers calls rateLimited or an IP-ban check: a venue's
// public address carries every guest at the party, and an IP ban on it
// must not cut the PoP's link or legs (docs/76 item 30). Authenticate has
// its own failure limiter instead — the exemption is by construction.
func (s *Server) registerPopRoutes(mux *http.ServeMux) {
	mux.HandleFunc("CONNECT /pop/link", s.handlePopLink)
	mux.HandleFunc("CONNECT /pop/addr", s.handlePopAddr)
	mux.HandleFunc("CONNECT /pop/publish/{id}", s.handlePopPublish)
	mux.HandleFunc("CONNECT /pop/subscribe/{id}", s.handlePopSubscribe)
	mux.HandleFunc("CONNECT /internal/pop/{name}", s.handleInternalPop)
}

// popRefuse writes a /pop/* refusal: the status, and the reason header the
// console reads (a used and an expired pairing string are both 410).
func popRefuse(w http.ResponseWriter, status int, reason string) {
	if reason != "" {
		w.Header().Set(poplink.ReasonHeader, reason)
	}
	w.WriteHeader(status)
}

// SetInternalRootCAs re-points the pod-to-pod edge dialer at pool instead
// of the system roots: the in-process fleet harnesses (popsrv's
// StartHomeAndPop) run a self-signed fleet certificate. No-op without
// -cluster-mode.
func (s *Server) SetInternalRootCAs(pool *x509.CertPool) {
	if em := s.edgeManager(); em != nil {
		em.dial = newEdgeDialer(s.cfg.InternalServerName, s.cfg.InternalPSK, pool, s.log)
	}
}
