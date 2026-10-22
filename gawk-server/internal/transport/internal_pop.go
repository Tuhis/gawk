package transport

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strconv"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// refusalReadGrace is how long the link-home pod keeps a want session open
// after writing a refusal, for the asking pod to read it and close.
const refusalReadGrace = 2 * time.Second

// handleInternalPop is CONNECT /internal/pop/{name}?want={id}&gen=…&psk=…,
// pod to link-home pod (docs/76 item 17): forward want{id} on the PoP's link
// this pod holds and answer with the PoP's refusal, if any, on one uni
// stream (a poplink.Refuse record). No answer is a clean close.
//
// Fenced like /internal/subscribe: the cluster PSK, wire.Version as proto
// (426), and the link generation the asking pod read from status.lease — a
// pod that no longer holds the link is 404, a stale generation 409, so the
// asker re-reads where the link is now.
func (s *Server) handleInternalPop(w http.ResponseWriter, r *http.Request) {
	const route = "internal_pop"
	if s.rejectedDraining(w, route) {
		return
	}
	h := s.popDemand()
	if h == nil || s.clusterCoord() == nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("psk")), []byte(s.cfg.InternalPSK)) != 1 {
		s.metrics.Connection(route, metrics.OutcomeUnauthorized)
		s.log.Warn("internal pop unauthorized: bad PSK", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if q.Get("proto") != strconv.Itoa(wire.Version) {
		s.metrics.Connection(route, metrics.OutcomeError)
		w.WriteHeader(http.StatusUpgradeRequired)
		return
	}
	pop := r.PathValue("name")
	id, err := broadcastid.Normalize(q.Get("want"))
	if err != nil || pop == "" {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	held, ok := h.HoldsLink(pop)
	if !ok {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if gen, err := strconv.ParseInt(q.Get("gen"), 10, 64); err != nil || gen != held {
		s.metrics.Connection(route, metrics.OutcomeConflict)
		w.WriteHeader(http.StatusConflict)
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

	ctx, cancel := context.WithTimeout(r.Context(), s.popDemandTimeout())
	refusal, err := h.Want(ctx, pop, id)
	cancel()
	if err != nil {
		s.log.Debug("internal pop want not forwarded", "pop", pop, "broadcast_key", s.broadcastKey(id), "err", err)
	}
	if refusal == "" {
		_ = sess.CloseWithError(0, "")
		return
	}
	if err := writeRefusal(r.Context(), sess, id, refusal); err != nil {
		s.log.Debug("internal pop refusal not delivered", "err", err)
		_ = sess.CloseWithError(0, "")
		return
	}
	t := time.NewTimer(refusalReadGrace)
	defer t.Stop()
	select {
	case <-sess.Context().Done():
	case <-t.C:
		_ = sess.CloseWithError(0, "")
	}
}

func writeRefusal(ctx context.Context, sess *webtransport.Session, id, reason string) error {
	st, err := sess.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	if err := poplink.Write(st, poplink.Refuse{ID: id, Reason: reason}); err != nil {
		st.CancelWrite(0)
		return err
	}
	return st.Close()
}
