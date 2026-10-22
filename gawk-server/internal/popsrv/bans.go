package popsrv

// bans.go: the home half of the moderation reach (docs/76 §6.9).
//
// Item 29: the link-home pod forwards every active ID ban (kill cooldowns
// are ID bans with an expiry) that names one of its PoP's broadcasts as a
// ban{id, until} record; the PoP ends the broadcast with 4006 and refuses
// its reclaims until the ban ends. "Its PoP's broadcasts" is the PoP's
// reservations, plus every ID the PoP has named on the link (reserve,
// state, wall, stats): a party-only broadcast past limits.maxBroadcasts has
// no reservation, and a kill of a forwarded one deletes its Lease fleet-wide
// in the same instant the ban event arrives.
//
// Item 31: a revoked, deleted or expired Pop loses its reservations, which
// ends the outside copies fleet-wide through the informers (viewers see
// 4000, a new dial 404). The link itself ends in link.go (bye + 4006).
//
// Every path here is idempotent: the ban source re-fires every record on
// each resync, and the Pop source on every status write.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// maxKnownPerPop bounds the IDs remembered per PoP from its records; a PoP
// carries at most a few dozen broadcasts, so the cap only guards memory.
const maxKnownPerPop = 4096

// ModerationReservations is the slice of Reservations the moderation reach
// reads: who owns an ID, a PoP's IDs, and the revoke's release.
type ModerationReservations interface {
	Owner(id string) (pop string, ok bool)
	List(pop string) []string
	ReleaseAll(ctx context.Context, pop string) error
}

// ModerationOptions wires the moderation reach onto a Manager.
type ModerationOptions struct {
	// Bans is this pod's ban set (the one its ban source fills).
	Bans *moderation.Set
	// Reservations is the fleet's reservation store; nil forwards only
	// the IDs the PoP named and releases nothing on a revoke.
	Reservations ModerationReservations
	// Sweep is how often the forwarder re-checks every linked PoP's IDs
	// against the ban set and every Pop for a revoke or a passed end date
	// (default poplink.LeaseRenew).
	Sweep time.Duration
}

// BanForwarder is the moderation reach on one pod. HandleBan is chained on
// the transport's ban callback (transport.Server.AddBanObserver).
type BanForwarder struct {
	m    *Manager
	o    ModerationOptions
	log  *slog.Logger
	stop func()

	mu    sync.Mutex
	known map[string]map[string]struct{} // pop -> IDs it named on the link
	sent  map[string]map[string]string   // pop -> ID -> until key last forwarded
	// sentGen is the link generation sent[pop] describes. A new link (a
	// relink, or a restarted PoP that lost its local bans) has a new
	// generation, which starts sent[pop] afresh so every ban goes out on it
	// again. Keying on the generation rather than clearing on the hello
	// matters: a sweep may forward on the new link before its hello is
	// read, and clearing then would send that ban twice.
	sentGen map[string]int64
}

// sentOn is pop's sent set for the link of generation gen, reset when the
// link changed. f.mu must be held.
func (f *BanForwarder) sentOn(pop string, gen int64) map[string]string {
	if f.sentGen[pop] != gen || f.sent[pop] == nil {
		f.sent[pop] = map[string]string{}
		f.sentGen[pop] = gen
	}
	return f.sent[pop]
}

// EnableModeration installs the moderation reach: it learns each linked
// PoP's broadcast IDs from its records, forwards bans on a new link and on
// every sweep, and releases a revoked PoP's reservations. It runs until ctx
// ends.
func (m *Manager) EnableModeration(ctx context.Context, o ModerationOptions) *BanForwarder {
	if o.Sweep <= 0 {
		o.Sweep = poplink.LeaseRenew
	}
	f := &BanForwarder{
		m:       m,
		o:       o,
		log:     m.log.With("subsystem", "moderation"),
		known:   map[string]map[string]struct{}{},
		sent:    map[string]map[string]string{},
		sentGen: map[string]int64{},
	}
	m.OnLinkRecord(poplink.THello, func(pop string, _ any) {
		// A new link (a relink, or a restarted PoP that lost its local
		// bans): re-send. sentOn already forgot what went out on the old
		// link, by its generation.
		go f.sweepPop(pop)
	})
	m.OnLinkRecord(poplink.TReserve, func(pop string, rec any) {
		if r, ok := rec.(*poplink.Reserve); ok {
			f.learn(pop, r.IDs...)
		}
	})
	m.OnLinkRecord(poplink.TState, func(pop string, rec any) {
		if r, ok := rec.(*poplink.State); ok {
			f.learn(pop, r.ID)
		}
	})
	m.OnLinkRecord(poplink.TWall, func(pop string, rec any) {
		if r, ok := rec.(*poplink.Wall); ok {
			f.learn(pop, r.ID)
		}
	})
	m.OnLinkRecord(poplink.TStats, func(pop string, rec any) {
		if r, ok := rec.(*poplink.Stats); ok {
			for _, b := range r.Broadcasts {
				f.learn(pop, b.ID)
			}
		}
	})
	m.OnLinkRecord(poplink.TRelease, func(pop string, rec any) {
		if r, ok := rec.(*poplink.Release); ok {
			f.forget(pop, r.IDs...)
		}
	})
	var unsub func()
	if src := m.opts.Source; src != nil {
		unsub = src.Subscribe(func(name string) { go f.checkRevoked(ctx, name, true) })
	}
	sctx, cancel := context.WithCancel(ctx)
	f.stop = func() {
		cancel()
		if unsub != nil {
			unsub()
		}
	}
	go f.sweepLoop(sctx)
	return f
}

// Stop ends the sweep and the source subscription (tests; production runs
// until the context ends).
func (f *BanForwarder) Stop() { f.stop() }

// HandleBan is the ban callback: an active ID ban that names a PoP's
// broadcast goes to that PoP if this pod holds its link. Everything else
// (IP bans, expired records, IDs no PoP owns) is not this subsystem's.
func (f *BanForwarder) HandleBan(rec moderation.Record) {
	if rec.Target.Type != moderation.TargetBroadcastID || !rec.Active(f.m.now()) {
		return
	}
	norm, err := moderation.Normalize(rec)
	if err != nil {
		return
	}
	id := norm.Target.Value
	for _, pop := range f.owners(id) {
		go f.forward(pop, id, &norm)
	}
}

// owners is every PoP id belongs to: its reservation, and every PoP that
// named it on its link.
func (f *BanForwarder) owners(id string) []string {
	var out []string
	if r := f.o.Reservations; r != nil {
		if pop, ok := r.Owner(id); ok {
			out = append(out, pop)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for pop, ids := range f.known {
		if _, ok := ids[id]; !ok {
			continue
		}
		dup := false
		for _, p := range out {
			dup = dup || p == pop
		}
		if !dup {
			out = append(out, pop)
		}
	}
	return out
}

// forward sends one ban to pop's link when this pod holds it and the same
// ban (same ID, same end) has not gone out on this link already. rec nil
// reads the ban set; an ID that is not banned (any more) is cleared so a
// later ban goes out again.
func (f *BanForwarder) forward(pop, id string, rec *moderation.Record) {
	l := f.m.local(pop)
	if l == nil {
		return // another pod holds the link, or none does
	}
	if rec == nil {
		r, banned := f.o.Bans.BannedID(id, f.m.now())
		if !banned {
			f.lift(pop, id, l.gen)
			return
		}
		rec = &r
	}
	key := untilKey(rec.ExpiresAt)
	f.mu.Lock()
	sent := f.sentOn(pop, l.gen)
	if prev, ok := sent[id]; ok && prev == key {
		f.mu.Unlock()
		return
	}
	sent[id] = key
	f.mu.Unlock()

	var until *time.Time
	if rec.ExpiresAt != nil {
		t := rec.ExpiresAt.UTC()
		until = &t
	}
	if err := f.m.SendToLink(pop, poplink.Ban{ID: id, Until: until}); err != nil {
		f.mu.Lock()
		if f.sentGen[pop] == l.gen && f.sent[pop][id] == key {
			delete(f.sent[pop], id)
		}
		f.mu.Unlock()
		f.log.Debug("pop ban not forwarded; the next sweep retries", "pop", pop, "err", err)
		return
	}
	f.log.Info("ban forwarded to pop", "pop", pop, "permanent", until == nil)
}

// lift tells pop that a ban forwarded on this link no longer holds (an
// operator removed it, or a cooldown ended early or ran out), so the PoP
// stops refusing the ID; a ban never forwarded here sends nothing.
func (f *BanForwarder) lift(pop, id string, gen int64) {
	f.mu.Lock()
	on := f.sentOn(pop, gen)
	_, sent := on[id]
	delete(on, id)
	f.mu.Unlock()
	if !sent {
		return
	}
	until := poplink.BanLifted
	if err := f.m.SendToLink(pop, poplink.Ban{ID: id, Until: &until}); err != nil {
		f.log.Debug("pop ban lift not sent", "pop", pop, "err", err)
		return
	}
	f.log.Info("ban lifted at pop", "pop", pop)
}

// sweepPop forwards every active ban on one PoP's IDs.
func (f *BanForwarder) sweepPop(pop string) {
	if f.m.local(pop) == nil {
		return
	}
	for _, id := range f.ids(pop) {
		f.forward(pop, id, nil)
	}
}

// ids is pop's reservations and the IDs it named.
func (f *BanForwarder) ids(pop string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(id string) {
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	if r := f.o.Reservations; r != nil {
		for _, id := range r.List(pop) {
			add(id)
		}
	}
	f.mu.Lock()
	for id := range f.known[pop] {
		add(id)
	}
	f.mu.Unlock()
	return out
}

func (f *BanForwarder) sweepLoop(ctx context.Context) {
	t := time.NewTicker(f.o.Sweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		f.m.mu.Lock()
		linked := make([]string, 0, len(f.m.links))
		for name := range f.m.links {
			linked = append(linked, name)
		}
		f.m.mu.Unlock()
		for _, pop := range linked {
			f.sweepPop(pop)
		}
		if src := f.m.opts.Source; src != nil {
			for _, p := range src.List() {
				f.checkRevoked(ctx, p.Name, false)
			}
		}
	}
}

// checkRevoked releases a PoP's reservations once it is revoked, deleted or
// past its end date. From a source event it always asks the store (a
// deleted CR leaves nothing else to go on); from the sweep only when the
// cache still shows reservations, which is what catches an end date that
// passed with no event.
func (f *BanForwarder) checkRevoked(ctx context.Context, name string, event bool) {
	p, found := f.m.pop(name)
	if found && !ended(p, f.m.now()) {
		return
	}
	r := f.o.Reservations
	if r == nil {
		return
	}
	if !event && len(r.List(name)) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.ReleaseAll(rctx, name); err != nil {
		f.log.Warn("releasing a revoked pop's reservations failed; the sweep retries", "pop", name, "err", err)
		return
	}
	f.mu.Lock()
	delete(f.known, name)
	delete(f.sent, name)
	delete(f.sentGen, name)
	f.mu.Unlock()
	f.log.Info("pop reservations released", "pop", name, "deleted", !found)
}

// ended reports a Pop that admits no link: revoked or past spec.notAfter.
func ended(p *pops.Pop, now time.Time) bool {
	return p.Spec.Revoked || (p.Spec.NotAfter != nil && now.After(p.Spec.NotAfter.Time))
}

func (f *BanForwarder) learn(pop string, ids ...string) {
	var fresh []string
	f.mu.Lock()
	set := f.known[pop]
	if set == nil {
		set = map[string]struct{}{}
		f.known[pop] = set
	}
	for _, raw := range ids {
		id, err := broadcastid.Normalize(raw)
		if err != nil {
			continue
		}
		if _, ok := set[id]; ok || len(set) >= maxKnownPerPop {
			continue
		}
		set[id] = struct{}{}
		fresh = append(fresh, id)
	}
	f.mu.Unlock()
	// A PoP naming an ID that is banned already (a restarted PoP, or a
	// reclaim racing the kill) hears of the ban at once.
	for _, id := range fresh {
		go f.forward(pop, id, nil)
	}
}

func (f *BanForwarder) forget(pop string, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, raw := range ids {
		if id, err := broadcastid.Normalize(raw); err == nil {
			delete(f.known[pop], id)
		}
	}
}

func untilKey(t *time.Time) string {
	if t == nil {
		return "permanent"
	}
	return t.UTC().Format(time.RFC3339Nano)
}
