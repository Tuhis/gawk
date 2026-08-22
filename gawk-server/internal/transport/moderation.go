package transport

// R39 AP3 kill actuation (docs/42 §4.3, §4.1 step 3).
//
// AP2 put the ban SET on the publish path — the gate that keeps a banned
// broadcaster OUT. This is the other half: the ban EVENT, which throws out
// whoever is already in. Both halves matter, and only together do they hold:
// without the gate a killed broadcaster auto-resumes within seconds; without
// the kill a ban only takes effect at the next reconnect.
//
// Every pod runs this on its OWN informer event, independently of
// -cluster-mode. Enforcement is not a federation feature: a single-pod relay
// must kill just as well, and in a fleet each pod acting on the CR is what
// makes the kill simultaneous rather than a cascade through the Lease.

import (
	"net/netip"

	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// terminationReason is the close reason every kill carries. Deliberately not
// the ban's own reason: reasons are operator-private context (docs/42 §5) and
// a close reason travels to the client.
const terminationReason = "terminated by operator"

// HandleBanAdded actuates one ban against this pod's live state.
//
// Wired to the ban set's change callback, so it runs for every source — the
// k8s informer's add/update AND a file-source reload — and for every record
// the source (re-)applies. It must therefore be IDEMPOTENT: a resync that
// re-delivers a ban already actuated finds no hub and no publisher, and does
// nothing.
//
// A record whose ExpiresAt has already passed kills nothing. Expiry is
// evaluated here against the same clock the publish path uses, so a ban CR
// that outlived its cooldown (janitor down, docs/42 §6) is inert on both
// paths alike rather than inert on one and lethal on the other.
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

// publishersIn lists the broadcast IDs whose LIVE publisher's recorded source
// address falls inside the prefix. Snapshotted under the lock and acted on
// outside it: terminate closes sessions, which must never happen while
// holding sessMu.
//
// Only live publishers are walked. A broadcast in grace has no address to
// match — its ban is enforced by the 451 on the reclaim, which is the same
// answer the mint path gives (docs/42 §4.3).
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

// terminate kills one broadcast on this pod: publisher session first, then
// the hub and every subscriber, all with 4006.
//
// Order matters. Closing the publisher first stops new media arriving while
// the hub is being torn down; TerminateBroadcast then closes viewers, edge
// sessions and stripe legs, purges the caches and the DVR ring, folds the
// counters, and (origin, cluster mode) fires OnBroadcastExpired so the Lease
// is deleted fleet-wide. The later lease-deletion informer event finds no hub
// and no-ops — both paths are idempotent, so the race has no wrong order
// (docs/42 §6).
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
		_ = pub.sess.CloseWithError(wire.CloseCodeTerminatedByOperator, terminationReason)
	}

	removed := s.registry.TerminateBroadcast(broadcastID,
		uint32(wire.CloseCodeTerminatedByOperator), terminationReason)
	if pub == nil && !removed {
		// Nothing of this broadcast lives here. Not an event.
		return
	}
	s.metrics.Termination()
	// The raw ID stays out of this line: it is a join capability (docs/42 §5,
	// D8), a kill cooldown expires, and a graced broadcast outlives its
	// publisher — so a "terminated" ID is not a spent one, and this is the
	// line most likely to be shipped to an aggregator. broadcast_key is the
	// same per-process HMAC /statusz and the metrics labels carry, so an
	// operator can still join the three together; the ID itself is one Debug
	// level away for whoever needs to act on it.
	s.log.Warn("broadcast terminated by operator",
		"broadcast_key", s.broadcastKey(broadcastID), "reason", why,
		"publisher_closed", pub != nil, "hub_removed", removed)
	s.log.Debug("broadcast termination detail", "id", broadcastID, "reason", why)
}
