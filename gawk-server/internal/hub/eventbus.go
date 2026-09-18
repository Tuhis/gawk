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
		Key:  r.ObfuscateID(id),
		Time: time.Now(),
		Data: data,
	})
}

// busStarted publishes a publisher session claiming a broadcast: a fresh mint,
// a reclaim, or the session that supersedes a deposed one.
func (r *Registry) busStarted(id string) {
	if r.opts.OnEvent == nil {
		return
	}
	r.emitEvent(events.TypeBroadcastStarted, id, events.BroadcastStartedData{
		ID:        id,
		Key:       r.ObfuscateID(id),
		Role:      events.RoleOrigin,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	})
}

// busEnded publishes a broadcast's removal with why it went:
//
//	replaced — a token-bearing reclaim superseded the session
//	killed   — an operator ended it (R39)
//	gc       — the grace period expired with no publisher
//
// The three are distinct because a consumer acts differently on each: a
// replacement is a broadcaster reconnecting, a kill is enforcement, a gc is
// the ordinary end of a stream.
func (r *Registry) busEnded(id, reason string) {
	if r.opts.OnEvent == nil {
		return
	}
	r.emitEvent(events.TypeBroadcastEnded, id, events.BroadcastEndedData{
		ID:     id,
		Key:    r.ObfuscateID(id),
		Reason: reason,
	})
}

// busStalled publishes the away/back transition. The registry's stall sweep
// already computes it once per transition; this is the same fact for the bus.
func (r *Registry) busStalled(id string, stalled bool) {
	if r.opts.OnEvent == nil {
		return
	}
	typ := events.TypeBroadcastPublisherBack
	if stalled {
		typ = events.TypeBroadcastPublisherAway
	}
	r.emitEvent(typ, id, events.BroadcastPublisherData{ID: id, Key: r.ObfuscateID(id)})
}

// busViewers publishes a viewer count. The publisher coalesces these to at
// most one per broadcast per interval and only on change, so this may be
// called on every pump tick.
//
// An edge pod reports only what it can know — its own local count — and says
// role: edge, which is how a consumer tells the fleet-wide number (origin,
// with viewersGlobal) from a per-pod one.
func (r *Registry) busViewers(id string, local int, global *int, edge bool) {
	if r.opts.OnEvent == nil {
		return
	}
	role := events.RoleOrigin
	if edge {
		role, global = events.RoleEdge, nil
	}
	r.emitEvent(events.TypeBroadcastViewers, id, events.BroadcastViewersData{
		ID:            id,
		Key:           r.ObfuscateID(id),
		Role:          role,
		ViewersLocal:  local,
		ViewersGlobal: global,
	})
}
