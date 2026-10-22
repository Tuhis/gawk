package popclient

// bans.go: the PoP half of the moderation reach (docs/76 §6.9).
//
// Item 29: home forwards ID bans (kills included) on the PoP's broadcasts
// as ban{id, until}. The PoP puts each into its own ban set until it ends,
// so a reclaim of the ID is refused here as it would be at home, and ends
// the broadcast with 4006 through the relay's ordinary kill (the publisher
// and every viewer get docs/59's moderator copy). A forwarded ban outlives
// the link: a PoP that goes offline or is revoked keeps enforcing it. Home
// lifting the ban while linked lifts it here (a Ban already ended).
//
// Item 32: while linked, the PoP's RelayIdentity (0x11) carries the R40
// screening notice whenever home's config says home's does.

import (
	"context"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// banReassert is how often forwarded bans are written back into the ban
// set: a host's own -moderation-source file reload replaces the whole set,
// and would otherwise drop a forwarded ban before it ends.
const banReassert = 2 * time.Second

// forwardedBanCreator is the CreatedBy of a forwarded ban in the PoP's set.
const forwardedBanCreator = "home"

// Moderation is the PoP's moderation reach, installed by EnableModeration.
type Moderation struct {
	c    *Client
	bans *moderation.Set
	now  func() time.Time

	mu        sync.Mutex
	forwarded map[string]moderation.Record // normalized ID -> record
}

// EnableModeration makes the PoP enforce the bans home forwards into bans
// (the relay's own ban set, which its publish path consults) and mirror
// home's screening notice. It runs until ctx ends. Call it before Run.
func (c *Client) EnableModeration(ctx context.Context, bans *moderation.Set) *Moderation {
	m := &Moderation{c: c, bans: bans, now: time.Now, forwarded: map[string]moderation.Record{}}
	c.OnRecord(poplink.TBan, func(rec any) {
		if b, ok := rec.(*poplink.Ban); ok {
			m.apply(*b)
		}
	})
	if srv := c.opts.Server; srv != nil {
		cancel := c.Subscribe(func(s LinkState) {
			srv.SetScreeningNotice(s.Header == HeaderLinked && s.Config != nil && s.Config.ScreeningNotice)
		})
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}
	go m.reassertLoop(ctx)
	return m
}

// apply enforces one forwarded ban: into the set first, so a reclaim racing
// the kill is refused, then the kill itself (4006 to the publisher and the
// viewers), off the link's reader goroutine.
func (m *Moderation) apply(b poplink.Ban) {
	rec := moderation.Record{
		Target:    moderation.Target{Type: moderation.TargetBroadcastID, Value: b.ID},
		ExpiresAt: b.Until,
		CreatedBy: forwardedBanCreator,
	}
	norm, err := moderation.Normalize(rec)
	if err != nil {
		m.c.log.Warn("forwarded ban ignored: not a broadcast ID", "err", err)
		return
	}
	if !norm.Active(m.now()) {
		// An ended ban is home's lift (poplink.BanLifted): drop the
		// forwarded one, and remove it from the set unless the host's own
		// ban has replaced it there.
		m.lift(norm.Target)
		return
	}
	m.mu.Lock()
	m.forwarded[norm.Target.Value] = norm
	m.mu.Unlock()
	if !m.coveredLonger(norm) {
		if err := m.bans.Upsert(norm); err != nil {
			m.c.log.Warn("forwarded ban not stored", "err", err)
			return
		}
	}
	m.c.log.Info("home ended a party broadcast", "permanent", norm.ExpiresAt == nil)
	if srv := m.c.opts.Server; srv != nil {
		go srv.HandleBanAdded(norm)
	}
}

// lift ends the forwarded ban on t, if there is one.
func (m *Moderation) lift(t moderation.Target) {
	m.mu.Lock()
	_, had := m.forwarded[t.Value]
	delete(m.forwarded, t.Value)
	m.mu.Unlock()
	if !had {
		return
	}
	if cur, banned := m.bans.BannedID(t.Value, m.now()); banned && cur.CreatedBy == forwardedBanCreator {
		if err := m.bans.Remove(t); err != nil {
			m.c.log.Warn("lifted ban not removed", "err", err)
			return
		}
	}
	m.c.log.Info("home lifted a ban on a party broadcast")
}

// Active lists the forwarded bans still in force (the console and tests).
func (m *Moderation) Active() []moderation.Record {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []moderation.Record
	for _, r := range m.forwarded {
		if r.Active(now) {
			out = append(out, r)
		}
	}
	return out
}

func (m *Moderation) reassertLoop(ctx context.Context) {
	t := time.NewTicker(banReassert)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m.reassert()
	}
}

// reassert drops forwarded bans that ended and writes the rest back into
// the set where a reload dropped them.
func (m *Moderation) reassert() {
	now := m.now()
	m.mu.Lock()
	var keep []moderation.Record
	for id, r := range m.forwarded {
		if !r.Active(now) {
			delete(m.forwarded, id)
			continue
		}
		keep = append(keep, r)
	}
	m.mu.Unlock()
	for _, r := range keep {
		if _, banned := m.bans.BannedID(r.Target.Value, now); !banned {
			_ = m.bans.Upsert(r)
		}
	}
}

// coveredLonger reports the set already bans the ID at least as long as
// rec does (the host's own ban): a forwarded cooldown must not shorten it.
func (m *Moderation) coveredLonger(rec moderation.Record) bool {
	cur, banned := m.bans.BannedID(rec.Target.Value, m.now())
	if !banned {
		return false
	}
	if cur.ExpiresAt == nil {
		return true
	}
	return rec.ExpiresAt != nil && !cur.ExpiresAt.Before(*rec.ExpiresAt)
}
