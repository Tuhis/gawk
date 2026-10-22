package transport

import (
	"context"
	"errors"
	"log/slog"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// claimMint is the mint path's origin claim. Cluster mode: create the fresh
// ID's origin Lease — where the cluster-wide MaxBroadcasts binds (the
// registry limit is per-pod). On rejection the hub goes into grace and is
// GC'd; nobody knows the ID. False means the session was closed and the
// handler must return.
func (s *Server) claimMint(ctx context.Context, sess *webtransport.Session, id string, pub *hub.Publisher, log *slog.Logger) (ok bool) {
	coord := s.clusterCoord()
	if coord == nil {
		return true
	}
	if _, cerr := coord.Claim(ctx, id, false); cerr != nil {
		pub.Close()
		log.Warn("origin lease create failed for minted broadcast", "id", id, "err", cerr)
		if errors.Is(cerr, cluster.ErrMaxBroadcasts) {
			s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
			sess.CloseWithError(429, "max concurrent broadcasts reached")
		} else {
			s.metrics.Connection("publish", metrics.OutcomeError)
			sess.CloseWithError(500, "failed to claim origin lease")
		}
		return false
	}
	return true
}
