package popsrv

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
)

// The home side of R74's reservations and uplink demand (docs/76 §6.4-6.5,
// PP8), as one subsystem on the Manager:
//
//   - reserve / release records from a PoP become reservations
//     (Reservations: gawk/pop Leases in cluster mode, a map single-pod),
//     answered with one reserved record per ID — ok, conflict, or limit
//     against the Pop's spec.limits.maxBroadcasts;
//   - Want, on the link-home pod, forwards want{X} on the link — one per
//     broadcast however many pods ask — and hands back the PoP's refuse
//     record, if one arrives within the demand timeout. transport calls it
//     from /internal/pop and from its own subscribe miss (transport's
//     PopDemandHome).

// UplinkOptions wires the uplink subsystem.
type UplinkOptions struct {
	// Reservations is the home's reservation set (serve's Wired.Reservations).
	Reservations Reservations
	// DemandTimeout is -pop-demand-timeout: how long a want waits for the
	// PoP's answer (default 5 s).
	DemandTimeout time.Duration
}

// Uplink is the home's reservation and demand subsystem. It implements
// transport.PopDemandHome.
type Uplink struct {
	m       *Manager
	res     Reservations
	timeout time.Duration

	mu    sync.Mutex
	waits map[string]*pendingWant // by pop + "/" + id
}

var _ transport.PopDemandHome = (*Uplink)(nil)

// pendingWant is one forwarded want and the pods waiting on its answer.
type pendingWant struct {
	sent   time.Time
	done   chan struct{}
	reason string
}

// StartUplink installs the subsystem on the manager's link records.
func (m *Manager) StartUplink(o UplinkOptions) *Uplink {
	if o.DemandTimeout <= 0 {
		o.DemandTimeout = 5 * time.Second
	}
	u := &Uplink{m: m, res: o.Reservations, timeout: o.DemandTimeout, waits: map[string]*pendingWant{}}
	m.OnLinkRecord(poplink.TReserve, func(pop string, rec any) {
		if r, ok := rec.(*poplink.Reserve); ok {
			go u.reserve(pop, r.IDs)
		}
	})
	m.OnLinkRecord(poplink.TRelease, func(pop string, rec any) {
		if r, ok := rec.(*poplink.Release); ok {
			go u.release(pop, r.IDs)
		}
	})
	m.OnLinkRecord(poplink.TRefuse, func(pop string, rec any) {
		if r, ok := rec.(*poplink.Refuse); ok {
			u.refused(pop, r.ID, r.Reason)
		}
	})
	return u
}

// reserve answers a reserve record, one reserved record per ID.
func (u *Uplink) reserve(pop string, ids []string) {
	for _, id := range ids {
		result := u.reserveOne(pop, id)
		if err := u.m.SendToLink(pop, poplink.Reserved{ID: id, Result: result}); err != nil {
			u.m.log.Debug("pop reserved reply not sent", "pop", pop, "err", err)
		}
	}
}

func (u *Uplink) reserveOne(pop, id string) string {
	if u.res == nil {
		return poplink.ReservedConflict
	}
	// A relinking PoP re-reserves what it already holds: that is still ok.
	if owner, ok := u.res.Owner(id); ok && owner == pop {
		return poplink.ReservedOK
	}
	// Single-pod: the in-memory set knows reservations only, so a live home
	// broadcast of the same ID is checked here (cluster mode's Lease
	// creation is the check itself).
	if u.m.opts.Coord == nil && u.m.opts.Registry != nil &&
		!errors.Is(u.m.opts.Registry.CheckSubscribe(id), hub.ErrNotFound) {
		return poplink.ReservedConflict
	}
	limit := 0
	if p, ok := u.m.pop(pop); ok {
		limit = p.EffectiveLimits().MaxBroadcasts
	}
	ctx, cancel := background()
	defer cancel()
	result, err := u.res.Reserve(ctx, id, pop, limit)
	if err != nil {
		u.m.log.Warn("pop reservation failed", "pop", pop, "err", err)
		// Undecided is not ok: the broadcast stays party-only, and the PoP
		// re-reserves on its next relink.
		return poplink.ReservedConflict
	}
	if result == poplink.ReservedOK {
		u.m.log.Info("pop broadcast reserved", "pop", pop, "broadcast_key", u.key(id))
	}
	return result
}

func (u *Uplink) release(pop string, ids []string) {
	if u.res == nil {
		return
	}
	ctx, cancel := background()
	defer cancel()
	for _, id := range ids {
		if err := u.res.Release(ctx, id, pop); err != nil {
			u.m.log.Debug("pop release refused", "pop", pop, "err", err)
		}
	}
}

func (u *Uplink) key(id string) string {
	if u.m.opts.Registry == nil {
		return ""
	}
	return u.m.opts.Registry.ObfuscateID(id)
}

// LinkHolder implements transport.PopDemandHome.
func (u *Uplink) LinkHolder(pop string) (string, int64, bool) { return u.m.LinkHolder(pop) }

// HoldsLink implements transport.PopDemandHome.
func (u *Uplink) HoldsLink(pop string) (int64, bool) { return u.m.HoldsLink(pop) }

// ReservedFor implements transport.PopDemandHome.
func (u *Uplink) ReservedFor(id string) (string, bool) {
	if u.res == nil {
		return "", false
	}
	return u.res.Owner(id)
}

// Want implements transport.PopDemandHome: forward want{id} on pop's link
// (once per demand timeout, whoever asks) and wait for the PoP's refusal.
func (u *Uplink) Want(ctx context.Context, pop, id string) (string, error) {
	if u.m.local(pop) == nil {
		return "", transport.ErrPopNotHolder
	}
	key := pop + "/" + id
	u.mu.Lock()
	w := u.waits[key]
	send := w == nil || time.Since(w.sent) >= u.timeout
	if send {
		w = &pendingWant{sent: time.Now(), done: make(chan struct{})}
		u.waits[key] = w
	}
	u.mu.Unlock()
	if send {
		if err := u.m.SendToLink(pop, poplink.Want{ID: id}); err != nil {
			u.drop(key, w)
			return "", err
		}
		u.m.log.Debug("pop want forwarded", "pop", pop, "broadcast_key", u.key(id))
		time.AfterFunc(u.timeout, func() { u.drop(key, w) })
	}
	t := time.NewTimer(u.timeout)
	defer t.Stop()
	select {
	case <-w.done:
		return w.reason, nil
	case <-ctx.Done():
	case <-t.C:
	}
	return "", nil
}

func (u *Uplink) drop(key string, w *pendingWant) {
	u.mu.Lock()
	if u.waits[key] == w {
		delete(u.waits, key)
	}
	u.mu.Unlock()
}

// refused hands a refuse record to every pod waiting on that want.
func (u *Uplink) refused(pop, id, reason string) {
	key := pop + "/" + id
	u.mu.Lock()
	w := u.waits[key]
	delete(u.waits, key)
	u.mu.Unlock()
	if w == nil {
		return
	}
	w.reason = reason
	close(w.done)
	u.m.log.Info("pop want refused", "pop", pop, "broadcast_key", u.key(id), "reason", reason)
}
