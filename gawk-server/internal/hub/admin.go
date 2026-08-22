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

	"github.com/Tuhis/gawk/gawk-server/adminapi"
)

// AdminBroadcast is one row of GET /internal/admin/broadcasts. An alias, not a
// copy: the struct lives in the public `adminapi` package so gawk-admin's
// relayscan parses the exact type this file fills in ("reuse it; never mirror
// it").
type AdminBroadcast = adminapi.Broadcast

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
