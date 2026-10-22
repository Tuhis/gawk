package transport

import (
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
)

// handlePopAddr is CONNECT /pop/addr (docs/76 item 13): a short-lived dial
// a linked PoP makes over the address family its link did not use, so the
// fleet records both of its public addresses (offers match on them). It
// records the peer address and closes the session at once. Any pod serves
// it: the address goes into status.link.publicAddrs.
func (s *Server) handlePopAddr(w http.ResponseWriter, r *http.Request) {
	const route = "pop_addr"
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
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer s.trackSession(sess)()
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	home.RecordAddr(pop.Name, remoteIP(r.RemoteAddr))
	sess.CloseWithError(webtransport.SessionErrorCode(0), "recorded")
}
