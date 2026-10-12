package hub

import (
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
)

// Broadcast lifecycle hooks (docs/51). One publisher per fact: the ORIGIN pod
// publishes a broadcast's start and end, since an edge would report the same
// broadcast once per pod. The exception is broadcast.viewers, which both roles
// publish: an edge knows its own viewersLocal and the origin does not.
//
// Called with the registry lock held at most call sites. That is safe only
// because Publish is a non-blocking channel send; nothing here may block.

// emitEvent publishes one bus event about a broadcast. Nil-safe: no hook, no
// work.
func (r *Registry) emitEvent(typ, id string, data any) {
	if r.opts.OnEvent == nil {
		return
	}
	r.opts.OnEvent(eventbus.Event{
		Type: typ,
		// Both forms: the HMAC'd key routes on the bus, the raw ID is the
		// event's subject.
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
//	killed   — an OPERATOR ended it (close code 4006)
//	replaced — a token-bearing reclaim superseded the session
//
// A consumer acts differently on each: a replacement is a broadcaster
// reconnecting, a kill is enforcement somebody should see, a gc is the
// ordinary end of a stream. So the automatic stall timeout is a gc even though
// it removes the hub as forcefully as an operator kill.
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

// busStalled publishes the away/back transition, once per transition.
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
// An edge pod reports only its local count with role: edge, which is how a
// consumer tells the fleet-wide number (origin) from a per-pod one. An edge's
// viewersGlobal is its local count: the schema requires the property, and zero
// would read as "nobody is watching".
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
