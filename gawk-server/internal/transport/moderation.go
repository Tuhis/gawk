package transport

// Moderation kill actuation (docs/42 §4.3). The ban set on the publish path
// keeps a banned broadcaster out; the ban event throws out whoever is already
// in. Both are needed: without the gate a killed broadcaster auto-resumes in
// seconds; without the kill a ban waits for the next reconnect. Every pod
// acts on its own informer event, independent of -cluster-mode, so the kill
// is simultaneous fleet-wide rather than a cascade through the Lease.

import (
	"net/netip"

	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// terminationReason is the close reason every kill carries. Deliberately not
// the ban's own reason: reasons are operator-private context and a close
// reason travels to the client.
const terminationReason = "terminated by operator"

// HandleBanAdded actuates one ban against this pod's live state. It runs from
// the ban set's change callback for every source and every (re-)applied
// record, so it must be idempotent: a resync finds no hub and no publisher.
// An already-expired record kills nothing — evaluated on the same clock as
// the publish path, so a ban CR that outlived its cooldown is inert on both.
func (s *Server) HandleBanAdded(rec moderation.Record) {
	if !rec.Active(s.clock()) {
		s.log.Debug("moderation ban not actuated: already expired",
			"target_type", string(rec.Target.Type))
		return
	}
	norm, err := moderation.Normalize(rec)
	if err != nil {
		s.log.Warn("moderation ban not actuated: unparseable target",
			"target_type", string(rec.Target.Type), "err", err)
		return
	}
	switch norm.Target.Type {
	case moderation.TargetBroadcastID:
		s.terminate(norm.Target.Value, "ban:broadcastId")
	case moderation.TargetIP:
		prefix, err := moderation.ParsePrefix(norm.Target.Value)
		if err != nil {
			s.log.Warn("moderation ban not actuated: unparseable CIDR", "err", err)
			return
		}
		for _, id := range s.publishersIn(prefix) {
			s.terminate(id, "ban:ip")
		}
	}
}

// publishersIn lists the broadcast IDs whose live publisher's source address
// falls inside the prefix. Snapshotted under sessMu and acted on outside it:
// terminate closes sessions, which must never happen holding sessMu. A
// broadcast in grace has no address; its reclaim gets the 451.
func (s *Server) publishersIn(prefix netip.Prefix) []string {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	var ids []string
	for id, entry := range s.publishers {
		if entry.remote.IsValid() && prefix.Contains(moderation.CanonicalAddr(entry.remote)) {
			ids = append(ids, id)
		}
	}
	return ids
}

// terminate kills one broadcast on this pod with 4006: publisher first, so no
// new media arrives during teardown, then TerminateBroadcast (viewers, edge
// sessions, stripe legs, caches, DVR ring, counters, and on a cluster origin
// the fleet-wide Lease deletion). The later lease-deletion event finds no hub
// and no-ops; both paths are idempotent, so the race has no wrong order.
func (s *Server) terminate(broadcastID, why string) {
	// An edge pod's "publisher" is its own upstream pull: stop it first, or
	// the re-attach loop would rebuild the hub we are about to delete. Cheap
	// and synchronous on a pod that has no edge for this ID.
	if edges := s.edgeManager(); edges != nil {
		edges.StopEdge(broadcastID)
	}

	s.sessMu.Lock()
	pub := s.publishers[broadcastID]
	delete(s.publishers, broadcastID)
	s.sessMu.Unlock()
	if pub != nil {
		// With the in-band notice: a browser never reads the 4006 itself,
		// and would otherwise retry into the ban.
		closeWithNoticeAsync(pub.sess, wire.CloseCodeTerminatedByOperator, terminationReason)
	}

	removed := s.registry.TerminateBroadcast(broadcastID,
		uint32(wire.CloseCodeTerminatedByOperator), terminationReason)
	if pub == nil && !removed {
		// Nothing of this broadcast lives here. Not an event.
		return
	}
	s.metrics.Termination()
	// The raw ID stays out of this Warn line, the one most likely shipped to
	// an aggregator: it is a join capability, and a "terminated" ID is not
	// spent (cooldowns expire; graced broadcasts outlive their publisher).
	s.log.Warn("broadcast terminated by operator",
		"broadcast_key", s.broadcastKey(broadcastID), "reason", why,
		"publisher_closed", pub != nil, "hub_removed", removed)
	s.log.Debug("broadcast termination detail", "id", broadcastID, "reason", why)
}
