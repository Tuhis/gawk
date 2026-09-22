package hub

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
)

// The R50 broadcast lifecycle hooks (docs/51 D4). One publisher per fact: the
// ORIGIN pod publishes a broadcast's start and end, because an edge pod holds
// derived state and would otherwise report the same broadcast once per pod.
// The single exception is broadcast.viewers, which both roles publish — an
// edge knows its own viewersLocal and the origin does not.
//
// Called with the registry lock held at most call sites. That is safe only
// because the publisher's Publish is a non-blocking channel send; nothing here
// may grow a blocking call.

// emitEvent publishes one bus event about a broadcast. Nil-safe: no hook, no
// work.
func (r *Registry) emitEvent(typ, id string, data any) {
	if r.opts.OnEvent == nil {
		return
	}
	r.opts.OnEvent(eventbus.Event{
		Type: typ,
		// Both forms: the HMAC'd key routes on the bus, the raw ID is the
		// event's subject (docs/52 D9).
		Key:     r.ObfuscateID(id),
		Subject: id,
		Time:    time.Now(),
		Data:    data,
	})
}

// busStarted publishes a publisher session claiming a broadcast: a fresh mint,
// a reclaim, or the session that supersedes a deposed one.
func (r *Registry) busStarted(id string) {
	if r.opts.OnEvent == nil {
		return
	}
	r.emitEvent(events.TypeBroadcastStarted, id, events.BroadcastStartedData{
		BroadcastID:  id,
		BroadcastKey: r.ObfuscateID(id),
		Role:         events.RoleOrigin,
		StartedAt:    time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
	})
}

// busEnded publishes a broadcast's removal with why it went:
//
//	gc       — it ended on its own: the grace expired with no publisher, or
//	           the publisher went silent past it
//	killed   — an OPERATOR ended it (R39 close code 4006)
//	replaced — a token-bearing reclaim superseded the session
//
// The three are distinct because a consumer acts differently on each: a
// replacement is a broadcaster reconnecting, a kill is enforcement somebody
// should see, a gc is the ordinary end of a stream. That is why the automatic
// stall timeout is a gc even though it removes the hub the same forceful way
// an operator does — the code it closes with is the difference, and the only
// one that reads as enforcement is the operator's.
func (r *Registry) busEnded(id, reason string) {
	if r.opts.OnEvent == nil {
		return
	}
	r.emitEvent(events.TypeBroadcastEnded, id, events.BroadcastEndedData{
		BroadcastID:  id,
		BroadcastKey: r.ObfuscateID(id),
		Reason:       reason,
	})
}

// busStalled publishes the away/back transition. The registry's stall sweep
// already computes it once per transition; this is the same fact for the bus.
func (r *Registry) busStalled(id string, stalled bool) {
	if r.opts.OnEvent == nil {
		return
	}
	key := r.ObfuscateID(id)
	if stalled {
		r.emitEvent(events.TypeBroadcastPublisherAway, id, events.BroadcastPublisherAwayData{
			BroadcastID: id, BroadcastKey: key})
		return
	}
	r.emitEvent(events.TypeBroadcastPublisherBack, id, events.BroadcastPublisherBackData{
		BroadcastID: id, BroadcastKey: key})
}

// busViewers publishes a viewer count. The publisher coalesces these to at
// most one per broadcast per interval and only on change, so this may be
// called on every pump tick.
//
// An edge pod reports what it can know — its own local count — and says
// role: edge, which is how a consumer tells the fleet-wide number (origin,
// with viewersGlobal) from a per-pod one. An edge's viewersGlobal is its local
// count rather than a number it made up: the schema requires the property, and
// zero would read as "nobody is watching".
func (r *Registry) busViewers(id string, local, global int, edge bool) {
	if r.opts.OnEvent == nil {
		return
	}
	role := events.RoleOrigin
	if edge {
		role, global = events.RoleEdge, local
	}
	r.emitEvent(events.TypeBroadcastViewers, id, events.BroadcastViewersData{
		BroadcastID:   id,
		BroadcastKey:  r.ObfuscateID(id),
		Role:          role,
		ViewersLocal:  local,
		ViewersGlobal: global,
	})
}
