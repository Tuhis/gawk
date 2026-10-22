package transport

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// claimMint is the mint path's origin claim. Cluster mode: create the fresh
// ID's origin Lease — where the cluster-wide MaxBroadcasts binds (the
// registry limit is per-pod). On rejection the hub goes into grace and is
// GC'd; nobody knows the ID.
//
// A draw that lands on an R74 PoP reservation (cluster.ErrReserved, docs/76
// item 14) is redrawn: its hub is removed outright — left in grace it would
// satisfy CheckSubscribe and keep the PoP broadcast's viewers on this pod off
// the edge pull — and a fresh ID is minted and claimed, at most mintRedraws
// times, all before anything is announced. The returned id and pub are the
// ones that won; ok false means the session was closed and the handler must
// return.
func (s *Server) claimMint(ctx context.Context, sess *webtransport.Session, id string, pub *hub.Publisher, log *slog.Logger) (string, *hub.Publisher, bool) {
	coord := s.clusterCoord()
	if coord == nil {
		return id, pub, true
	}
	for redraw := 0; ; redraw++ {
		_, cerr := coord.Claim(ctx, id, false)
		if cerr == nil {
			return id, pub, true
		}
		pub.Close()
		if isReserved(cerr) {
			s.registry.EndBroadcast(id)
			if redraw == mintRedraws {
				s.metrics.Connection("publish", metrics.OutcomeError)
				log.Warn("minted broadcast IDs kept landing on PoP reservations", "redraws", mintRedraws)
				sess.CloseWithError(http.StatusServiceUnavailable, "failed to mint a broadcast ID")
				return "", nil, false
			}
			log.Info("minted broadcast ID is reserved for a PoP; redrawing", "broadcast_key", s.broadcastKey(id))
			var err error
			id, pub, err = s.registry.StartPublish("")
			if err != nil {
				log.Warn("failed to start publish session on redraw", "err", err)
				if errors.Is(err, hub.ErrMaxBroadcasts) {
					s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
					sess.CloseWithError(429, "max concurrent broadcasts reached")
				} else {
					s.metrics.Connection("publish", metrics.OutcomeError)
					sess.CloseWithError(500, "failed to start publish session")
				}
				return "", nil, false
			}
			continue
		}
		log.Warn("origin lease create failed for minted broadcast", "id", id, "err", cerr)
		if errors.Is(cerr, cluster.ErrMaxBroadcasts) {
			s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
			sess.CloseWithError(429, "max concurrent broadcasts reached")
		} else {
			s.metrics.Connection("publish", metrics.OutcomeError)
			sess.CloseWithError(500, "failed to claim origin lease")
		}
		return "", nil, false
	}
}
