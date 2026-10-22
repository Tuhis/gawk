package hub

import "github.com/Tuhis/gawk/gawk-server/internal/broadcastid"

// DownstreamSessions counts every session a broadcast's hub serves on this
// pod: local viewers, edge pulls and PoP legs alike — the "no downstream"
// test of an R74 uplink leg's linger (docs/76 item 20). An edge pull with no
// viewers of its own still counts: it is about to linger out on its own
// clock, and the leg's linger is the longer of the two. 0 for an unknown ID.
func (r *Registry) DownstreamSessions(id string) int {
	normID, err := broadcastid.Normalize(id)
	if err != nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.hubs[normID]
	if !ok {
		return 0
	}
	return len(b.subs)
}

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
