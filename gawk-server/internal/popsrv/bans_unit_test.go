package popsrv

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/adminapi"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/moderation"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// nextBan reads records until a Ban (a link may interleave a Config).
func nextBan(t *testing.T, r io.Reader) *poplink.Ban {
	t.Helper()
	for range 4 {
		if b, ok := next(t, r).(*poplink.Ban); ok {
			return b
		}
	}
	t.Fatal("no ban record")
	return nil
}

func idBan(id string, until *time.Time) moderation.Record {
	return moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: id}, ExpiresAt: until}
}

// The forwarder over one link: a reserved and banned ID goes out on the
// new link, an ID the PoP names later hears of its ban at once, a removed
// ban is lifted, IP and expired bans are not this subsystem's, and every
// path is idempotent across sweeps.
func TestBanForwarderOverALink(t *testing.T) {
	h := newUnitHome(t, nil)
	set := moderation.NewSet()
	res := NewMemReservations()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := res.Reserve(ctx, "K7XQ2M", "lan", 0); err != nil {
		t.Fatal(err)
	}
	if err := set.Upsert(idBan("k7xq2m", nil)); err != nil {
		t.Fatal(err)
	}
	f := h.m.EnableModeration(ctx, ModerationOptions{Bans: set, Reservations: res, Sweep: 30 * time.Millisecond})
	t.Cleanup(f.Stop)

	_, c, _ := h.up(t, poplink.Hello{})
	if b := nextBan(t, c); b.ID != "K7XQ2M" || b.Until != nil {
		t.Fatalf("ban on the new link %+v, want K7XQ2M permanent", b)
	}

	// The PoP names IDs on every record kind; junk IDs are skipped.
	mustWrite(t, c, poplink.State{ID: "ABC234", Live: true})
	mustWrite(t, c, poplink.Wall{Op: "attach", ID: "DEF567"})
	mustWrite(t, c, poplink.Stats{Broadcasts: []adminapi.PopBroadcast{{ID: "GHJ789"}, {ID: "not-an-id"}}})
	mustWrite(t, c, poplink.Reserve{IDs: []string{"K7XQ2M", "bad"}})
	WaitFor(t, "IDs learned", func() bool { return len(f.ids("lan")) == 4 })

	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	rec := idBan("def567", &exp)
	_ = set.Upsert(rec)
	f.HandleBan(rec)
	if b := nextBan(t, c); b.ID != "DEF567" || b.Until == nil || !b.Until.Equal(exp) {
		t.Fatalf("ban %+v, want DEF567 until %v", b, exp)
	}
	// Not ours: an IP ban, an expired ban, a malformed one, and an ID no
	// PoP owns or named.
	f.HandleBan(moderation.Record{Target: moderation.Target{Type: moderation.TargetIP, Value: "198.51.100.1"}})
	past := time.Now().Add(-time.Minute)
	f.HandleBan(idBan("GHJ789", &past))
	f.HandleBan(idBan("bad!", nil))
	f.HandleBan(idBan("MNP234", nil))
	if owners := f.owners("K7XQ2M"); len(owners) != 1 || owners[0] != "lan" {
		t.Fatalf("owners = %v; reservation and naming are one PoP", owners)
	}

	// The operator removes the DEF567 ban: the sweep lifts it at the PoP.
	if err := set.Remove(rec.Target); err != nil {
		t.Fatal(err)
	}
	if b := nextBan(t, c); b.ID != "DEF567" || b.Until == nil || !b.Until.Equal(poplink.BanLifted) {
		t.Fatalf("lift %+v, want DEF567 until BanLifted", b)
	}

	// A released ID is forgotten.
	mustWrite(t, c, poplink.Release{IDs: []string{"GHJ789", "junk"}})
	WaitFor(t, "ID forgotten", func() bool { return len(f.ids("lan")) == 3 })

	// A revoke releases the PoP's reservations.
	h.src.edit("lan", func(p *pops.Pop) { p.Spec.Revoked = true })
	WaitFor(t, "reservations released", func() bool { return len(res.List("lan")) == 0 })
}

// A ban for a PoP whose link this pod does not hold goes nowhere and is not
// remembered as sent, so the link-home pod's sweep still sends it.
func TestBanForwarderWithoutTheLink(t *testing.T) {
	h := newUnitHome(t, nil)
	set := moderation.NewSet()
	f := h.m.EnableModeration(context.Background(), ModerationOptions{Bans: set})
	defer f.Stop()
	if f.o.Sweep != poplink.LeaseRenew {
		t.Fatalf("default sweep %v", f.o.Sweep)
	}
	f.learn("lan", "K7XQ2M")
	_ = set.Upsert(idBan("K7XQ2M", nil))
	f.forward("lan", "K7XQ2M", nil)
	f.sweepPop("lan")
	f.mu.Lock()
	n := len(f.sent["lan"])
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d bans remembered as sent without a link", n)
	}
	// No reservation store: a revoke releases nothing and does not panic.
	f.checkRevoked(context.Background(), "missing", true)
}

type failingRelease struct{ ModerationReservations }

func (failingRelease) ReleaseAll(context.Context, string) error { return errors.New("api down") }
func (failingRelease) List(string) []string                     { return []string{"K7XQ2M"} }

// A failed release keeps what the forwarder knows, so the sweep retries;
// the sweep skips a Pop whose cache shows no reservations.
func TestCheckRevokedRetriesAFailedRelease(t *testing.T) {
	h := newUnitHome(t, nil)
	h.src.edit("lan", func(p *pops.Pop) { p.Spec.Revoked = true })
	f := h.m.EnableModeration(context.Background(), ModerationOptions{Bans: moderation.NewSet(), Reservations: failingRelease{}, Sweep: time.Hour})
	defer f.Stop()
	f.learn("lan", "K7XQ2M")
	f.checkRevoked(context.Background(), "lan", false)
	if len(f.ids("lan")) == 0 {
		t.Fatal("a failed release forgot the PoP's IDs")
	}

	res := NewMemReservations()
	g := h.m.EnableModeration(context.Background(), ModerationOptions{Bans: moderation.NewSet(), Reservations: res, Sweep: time.Hour})
	defer g.Stop()
	g.learn("lan", "ABC234")
	g.checkRevoked(context.Background(), "lan", false) // nothing reserved: skipped
	if len(g.ids("lan")) != 1 {
		t.Fatal("the sweep released a PoP with no reservations")
	}
	g.checkRevoked(context.Background(), "lan", true) // an event always asks
	if len(g.ids("lan")) != 0 {
		t.Fatal("an event's release kept the IDs")
	}
}

func TestLearnIsBounded(t *testing.T) {
	h := newUnitHome(t, nil)
	f := h.m.EnableModeration(context.Background(), ModerationOptions{Bans: moderation.NewSet(), Sweep: time.Hour})
	defer f.Stop()
	f.mu.Lock()
	f.known["lan"] = map[string]struct{}{}
	for i := range maxKnownPerPop {
		f.known["lan"][string(rune(i))] = struct{}{}
	}
	f.mu.Unlock()
	f.learn("lan", "K7XQ2M")
	f.mu.Lock()
	_, ok := f.known["lan"]["K7XQ2M"]
	f.mu.Unlock()
	if ok {
		t.Fatal("learn went past maxKnownPerPop")
	}
}
