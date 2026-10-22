package transport

import (
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// handlePopLink is CONNECT /pop/link (docs/76 §6.3): a PoP's link to the
// home fleet. ?pair=<token> is the pairing dial, redeemed after the
// upgrade; everything else carries Authorization: Bearer <credential>.
//
// Statuses before the upgrade: 404 without a PoP manager, 426 for another
// proto major, 401 / 403 / 429 from Authenticate, 404 / 410 / 403 / 429
// from CheckPairing, each with poplink.ReasonHeader. A pairing that loses a
// redeem race after the upgrade closes the session with the status as the
// session code (Go reads it, as the mint path's 429).
func (s *Server) handlePopLink(w http.ResponseWriter, r *http.Request) {
	const route = "pop_link"
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
		s.log.Warn("pop link protocol skew", "remote", r.RemoteAddr, "proto", r.URL.Query().Get("proto"))
		popRefuse(w, http.StatusUpgradeRequired, poplink.ReasonProto)
		return
	}
	pairing := r.URL.Query().Has("pair")
	var check func(*http.Request) (*pops.Pop, int, error)
	if pairing {
		check = home.CheckPairing
	} else {
		check = home.Authenticate
	}
	pop, status, err := check(r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUnauthorized)
		reason := poplink.RefusalReason(err)
		if status == http.StatusTooManyRequests {
			reason = poplink.ReasonLimited
		}
		s.log.Info("pop link refused", "remote", r.RemoteAddr, "status", status, "reason", reason, "pairing", pairing)
		popRefuse(w, status, reason)
		return
	}

	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		s.log.Warn("pop link upgrade failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer s.trackSession(sess)()

	var issued string
	if pairing {
		p, cred, status, err := home.RedeemPairing(r.Context(), pop)
		if err != nil {
			s.metrics.Connection(route, metrics.OutcomeConflict)
			s.log.Info("pop pairing refused after upgrade", "pop", pop.Name, "status", status, "err", err)
			sess.CloseWithError(webtransport.SessionErrorCode(status), poplink.RefusalReason(err))
			return
		}
		pop, issued = p, cred
	}
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	if err := home.ServeLink(r.Context(), pop, poplink.WrapSession(sess), issued); err != nil {
		s.log.Info("pop link ended", "pop", pop.Name, "err", err)
	}
}
