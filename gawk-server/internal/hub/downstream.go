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
