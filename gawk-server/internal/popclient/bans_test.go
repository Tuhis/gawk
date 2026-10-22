package popclient

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

type banClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *banClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *banClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// moderated is a PoP with EnableModeration running on the wall clock.
func moderated(t *testing.T) (*Client, *Moderation, *moderation.Set) {
	t.Helper()
	r, srv := relayParts(t)
	c := New(Options{Log: discard(), Registry: r, Server: srv})
	set := moderation.NewSet()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return c, c.EnableModeration(ctx, set), set
}

// clockedModeration is the moderation reach on a test clock, without its
// reassert loop (the test calls reassert).
func clockedModeration(t *testing.T) (*Moderation, *moderation.Set, *banClock) {
	t.Helper()
	clk := &banClock{t: time.Now()}
	set := moderation.NewSet()
	m := &Moderation{c: New(Options{Log: discard()}), bans: set, now: clk.now, forwarded: map[string]moderation.Record{}}
	return m, set, clk
}

func ptr[T any](v T) *T { return &v }

// Item 29: a forwarded ban goes into the relay's own set (so a reclaim is
// refused here) and ends the live broadcast; it is listed while in force.
func TestForwardedBanEndsTheBroadcast(t *testing.T) {
	c, m, set := moderated(t)
	id, _, err := c.opts.Registry.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	c.dispatch(poplink.TBan, &poplink.Ban{ID: id, Until: &until})
	rec, banned := set.BannedID(id, time.Now())
	if !banned || rec.CreatedBy != forwardedBanCreator || rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(until) {
		t.Fatalf("set after a forwarded ban: %+v %v", rec, banned)
	}
	if a := m.Active(); len(a) != 1 || a[0].Target.Value != id {
		t.Fatalf("Active = %+v", a)
	}
	deadline := time.Now().Add(waitFor)
	for {
		if _, _, known := c.opts.Registry.BroadcastState(id); !known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the banned broadcast is still in the hub")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A ban on something that is not a broadcast ID is ignored.
func TestForwardedBanNotAnID(t *testing.T) {
	c, m, _ := moderated(t)
	c.dispatch(poplink.TBan, &poplink.Ban{ID: "not an id!"})
	c.dispatch(poplink.TBan, poplink.Wall{}) // not a *Ban: ignored
	if a := m.Active(); len(a) != 0 {
		t.Fatalf("Active = %+v", a)
	}
}

// Home's lift (an Until in the past) removes the forwarded ban, but not a
// ban of the host's own that replaced it in the set; a lift for a ban never
// forwarded is a no-op.
func TestForwardedBanLift(t *testing.T) {
	m, set, clk := clockedModeration(t)
	m.apply(poplink.Ban{ID: "ABC234"})
	if _, banned := set.BannedID("ABC234", clk.now()); !banned {
		t.Fatal("permanent forwarded ban not in the set")
	}
	m.apply(poplink.Ban{ID: "ABC234", Until: ptr(poplink.BanLifted)})
	if _, banned := set.BannedID("ABC234", clk.now()); banned {
		t.Fatal("a lifted forwarded ban is still in the set")
	}
	if len(m.Active()) != 0 {
		t.Fatal("a lifted ban is still listed")
	}

	m.apply(poplink.Ban{ID: "DEF567", Until: ptr(clk.now().Add(time.Hour))})
	// The host's own file reload replaced it with a ban of its own.
	if err := set.Upsert(moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "DEF567"}, CreatedBy: "host"}); err != nil {
		t.Fatal(err)
	}
	m.apply(poplink.Ban{ID: "DEF567", Until: ptr(poplink.BanLifted)})
	if rec, banned := set.BannedID("DEF567", clk.now()); !banned || rec.CreatedBy != "host" {
		t.Fatalf("home's lift removed the host's ban: %+v %v", rec, banned)
	}

	// A host ban home never forwarded: the lift leaves it.
	if err := set.Upsert(moderation.Record{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "GHJ892"}, CreatedBy: forwardedBanCreator}); err != nil {
		t.Fatal(err)
	}
	m.apply(poplink.Ban{ID: "GHJ892", Until: ptr(poplink.BanLifted)})
	if _, banned := set.BannedID("GHJ892", clk.now()); !banned {
		t.Fatal("a lift for a ban never forwarded removed one")
	}
}

// A forwarded cooldown never shortens the host's own longer ban.
func TestForwardedBanDoesNotShortenTheHostsBan(t *testing.T) {
	m, set, clk := clockedModeration(t)
	target := moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC234"}
	// Permanent host ban.
	if err := set.Upsert(moderation.Record{Target: target, CreatedBy: "host"}); err != nil {
		t.Fatal(err)
	}
	m.apply(poplink.Ban{ID: "ABC234", Until: ptr(clk.now().Add(time.Hour))})
	if rec, _ := set.BannedID("ABC234", clk.now()); rec.CreatedBy != "host" || rec.ExpiresAt != nil {
		t.Fatalf("a cooldown replaced a permanent host ban: %+v", rec)
	}
	// A longer host cooldown stays; a shorter one is replaced.
	long := clk.now().Add(48 * time.Hour)
	if err := set.Upsert(moderation.Record{Target: target, ExpiresAt: &long, CreatedBy: "host"}); err != nil {
		t.Fatal(err)
	}
	m.apply(poplink.Ban{ID: "ABC234", Until: ptr(clk.now().Add(time.Hour))})
	if rec, _ := set.BannedID("ABC234", clk.now()); rec.CreatedBy != "host" {
		t.Fatalf("a shorter cooldown replaced a longer host ban: %+v", rec)
	}
	m.apply(poplink.Ban{ID: "ABC234"})
	if rec, _ := set.BannedID("ABC234", clk.now()); rec.CreatedBy != forwardedBanCreator || rec.ExpiresAt != nil {
		t.Fatalf("a permanent forwarded ban did not replace a host cooldown: %+v", rec)
	}
}

// A host's set reload drops forwarded bans; reassert writes the ones
// still in force back and forgets the ended ones.
func TestForwardedBansReassertedAfterAReload(t *testing.T) {
	m, set, clk := clockedModeration(t)
	m.apply(poplink.Ban{ID: "ABC234"})
	m.apply(poplink.Ban{ID: "DEF567", Until: ptr(clk.now().Add(time.Minute))})
	set.Replace(nil)
	clk.add(2 * time.Minute)
	m.reassert()
	if _, banned := set.BannedID("ABC234", clk.now()); !banned {
		t.Fatal("a permanent forwarded ban was not reasserted")
	}
	if _, banned := set.BannedID("DEF567", clk.now()); banned {
		t.Fatal("an ended forwarded ban came back")
	}
	if a := m.Active(); len(a) != 1 || a[0].Target.Value != "ABC234" {
		t.Fatalf("Active = %+v", a)
	}
	m.mu.Lock()
	n := len(m.forwarded)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d forwarded bans kept, want 1", n)
	}
}

// The reassert loop runs on its own: a reload is repaired within a tick.
func TestReassertLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("waits a reassert period")
	}
	_, m, set := moderated(t)
	m.apply(poplink.Ban{ID: "ABC234"})
	set.Replace(nil)
	deadline := time.Now().Add(banReassert + waitFor)
	for {
		if _, banned := set.BannedID("ABC234", time.Now()); banned {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the reassert loop did not restore the ban")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
