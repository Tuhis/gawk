package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// PopCoordinator is the coordinator slice an R74 PoP's uplink leg
// (/pop/publish/{id}, docs/76 item 17) claims through: only a Lease reserved
// for that PoP is takeable, and every other claimant gets
// cluster.ErrReserved. Reached by a type assertion on the installed
// ClusterCoordinator (popCoord), so the existing fakes need not grow it.
type PopCoordinator interface {
	ClaimForPop(ctx context.Context, broadcastID, pop string, force bool) (int64, error)
}

// popCoord returns the cluster coordinator as a PopCoordinator, or nil when
// cluster mode is off or the coordinator cannot claim for a PoP.
func (s *Server) popCoord() PopCoordinator {
	pc, _ := s.clusterCoord().(PopCoordinator)
	return pc
}

// HandleOriginClaimed is the cluster informer's OnOriginClaimed dispatch: a
// PoP reservation gained a holder (the PoP's uplink leg landed on some pod,
// docs/76 item 17), so a pull on this pod waiting on it re-resolves and
// dials now rather than on its next poll. A no-op with cluster mode off or
// nothing waiting.
func (s *Server) HandleOriginClaimed(broadcastID string, _ cluster.Origin) {
	if edges := s.edgeManager(); edges != nil {
		edges.KickReservation(broadcastID)
	}
}

// mintRedraws bounds how many fresh IDs the mint path draws after its
// claim lands on a PoP reservation (cluster.ErrReserved). One collision is
// already ~1 in 31^6 per reservation; eight in a row means something other
// than chance, so the mint is refused (503) rather than looping.
const mintRedraws = 8

// reservedInCache reports whether the lease cache already shows the ID as a
// PoP's (reserved or held through its leg): a resume of it is refused
// before anything is touched. The authoritative refusal is Claim's
// ErrReserved; this only keeps a refused resume from stopping an edge pull
// or taking a hub's publisher slot first.
func reservedInCache(coord ClusterCoordinator, id string) bool {
	if coord == nil {
		return false
	}
	origin, _, _, ok := coord.Lookup(id)
	return ok && origin.Pop != ""
}

// isReserved reports a claim refused because the Lease is a PoP's.
func isReserved(err error) bool { return errors.Is(err, cluster.ErrReserved) }

// rejectReservedResume answers a token-bearing /publish/{id} for a PoP's
// broadcast: 409, pre-upgrade. The token proves a home mint of the ID, and a
// PoP's reservation is not one (the PoP minted it).
func (s *Server) rejectReservedResume(w http.ResponseWriter, r *http.Request, id string) {
	s.metrics.Connection("publish", metrics.OutcomeConflict)
	s.log.Warn("publish claim refused: broadcast is reserved for a PoP",
		"broadcast_key", s.broadcastKey(id), "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"))
	w.WriteHeader(http.StatusConflict)
}
