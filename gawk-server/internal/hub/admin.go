package hub

// The R39 admin view of the registry (docs/42 §4.5, D8).
//
// This is the ONE snapshot that carries RAW broadcast IDs. It is served only
// from GET /internal/admin/broadcasts on the ops listener, which is
// ClusterIP-only AND credential-gated — strictly more protected than
// /statusz, which stays HMAC-only and byte-identical to what it has always
// been. Stats() is deliberately untouched: two callers, two shapes, no shared
// mutation, so nothing here can leak an ID into the public surface.

import (
	"sort"
	"time"
)

// AdminBroadcast is one row of GET /internal/admin/broadcasts. The json tags
// ARE the gawk.admin.broadcasts.v1 schema's per-broadcast object.
type AdminBroadcast struct {
	// ID is the RAW, joinable broadcast ID — the scoped relaxation of the
	// never-expose-raw-IDs invariant (docs/42 D8). The operator needs it to
	// join and judge a stream before killing it.
	ID string `json:"id"`
	// Key is ObfuscateID(ID): the same HMAC'd handle /statusz, the metrics
	// broadcast label and the telemetry UI's #/broadcast/<key> route use, so
	// the portal deep-links into telemetry without ever holding -stats-key.
	Key string `json:"key"`
	// Role is "origin" or "edge" (R17), the same vocabulary as /statusz.
	Role            string `json:"role"`
	PublisherActive bool   `json:"publisherActive"`
	// PublisherRemoteIP is filled in by the ops layer from the transport's
	// live-publisher bookkeeping; the hub has no view of session addresses.
	// Empty when there is no live publisher (or its address was unparseable).
	PublisherRemoteIP string `json:"publisherRemoteIp,omitempty"`
	// PublisherSessionID is the R28 telemetry session handle, omitted when
	// telemetry is off — the join into the broadcaster's own reports.
	PublisherSessionID string `json:"publisherSessionId,omitempty"`
	// StartedAt is when this pod registered the hub, not when the current
	// publisher session began: a broadcast that survived a reclaim keeps its
	// age.
	StartedAt time.Time `json:"startedAt"`
	// ViewersLocal counts WATCHING HUMANS on this pod: internal edge sessions
	// and R30 stripe legs are excluded, so one viewer with three legs is one
	// number here — the same rule ViewersGlobal follows (docs/35 §5.8).
	ViewersLocal int `json:"viewersLocal"`
	// ViewersGlobal is the fleet-wide count this pod computes as origin; 0 on
	// edge hubs, which receive the number from upstream (docs/23 Decision 9).
	ViewersGlobal         uint32 `json:"viewersGlobal"`
	GraceRemainingSeconds int    `json:"graceRemainingSeconds"`
	// DVRBytes is what this broadcast's R21 ring currently retains, 0 when no
	// ring was ever allocated.
	DVRBytes int `json:"dvrBytes"`
}

// AdminStats snapshots every broadcast this pod hosts, sorted by raw ID so
// the response is stable across scrapes (a portal diffing two polls should
// see content changes, not map-iteration noise).
//
// A separate walk from Stats() on purpose: Stats() is the public /statusz
// shape and must not grow a raw-ID field that some future handler forgets to
// strip.
func (r *Registry) AdminStats() []AdminBroadcast {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]AdminBroadcast, 0, len(r.hubs))
	for id, b := range r.hubs {
		row := AdminBroadcast{
			ID:                 id,
			Key:                r.ObfuscateID(id),
			Role:               "origin",
			PublisherActive:    b.publisherActive,
			PublisherSessionID: b.publisherSessionID,
			StartedAt:          b.startedAt,
		}
		if b.edge {
			row.Role = "edge"
		} else {
			row.ViewersGlobal = b.globalViewersLocked()
		}
		for s := range b.subs {
			if !s.internal && !s.stripeLeg {
				row.ViewersLocal++
			}
		}
		if !b.publisherActive && !b.graceStart.IsZero() {
			if rem := r.opts.BroadcastGrace - time.Since(b.graceStart); rem > 0 {
				row.GraceRemainingSeconds = int(rem.Seconds())
			}
		}
		if b.dvr != nil {
			row.DVRBytes = b.dvr.Bytes()
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
