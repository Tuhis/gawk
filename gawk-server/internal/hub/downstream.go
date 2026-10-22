package hub

import "github.com/Tuhis/gawk/gawk-server/internal/broadcastid"

// EdgeDemand reports, for an edge pull of broadcast id, what keeps it alive
// and what it reports upstream (docs/76 items 23 and 25).
//
// serving counts every session that consumes the hub: the external ones
// (stripe legs included, as ExternalSubscribers counts them) and the
// internal ones. On an edge hub the only internal subscribers are R74 PoP
// downlink legs (an edge never feeds an edge, docs/22), and a leg is
// demand: an edge pod serving only a PoP must not linger out under it.
//
// viewers counts the watching humans here and below: local viewers (stripe
// legs excluded) plus each internal subscriber's last-reported downstream
// count, the number an origin sums into G. Without the second term a party
// viewer whose leg landed on an edge pod would never reach home's G.
func (r *Registry) EdgeDemand(id string) (serving, viewers int) {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, exists := r.hubs[normID]
	if !exists {
		return 0, 0
	}
	return len(b.subs), int(b.globalViewersLocked())
}
